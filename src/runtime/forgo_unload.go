// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Unloading a c-shared library.
//
// A plugin host unloads a plugin (dlclose, FreeLibrary) when its last
// instance goes away and may load it again later. Once the library is
// unmapped, nothing may run its code again, so before that the runtime
// shuts itself down:
//
//  1. The library's destructor (runtime/cgo) calls forgoUnloadCallback on
//     the unloading thread, through crosscall2 like any //export function.
//  2. forgoUnload stops the world. Goroutines are never resumed; their
//     stacks and the heap go away with everything else.
//  3. It sets forgoUnloading and wakes every M the runtime created. Each M
//     that sees the flag at a parking point leaves Go for good and its
//     thread exits (forgoMExit). An M in a system call or C call does the
//     same once the call returns. If one does not return in time the
//     unload is refused with a fatal error, since the library is about to
//     disappear under it.
//  4. It waits until every thread has really exited, restores the signal
//     handlers (Unix) or removes the exception handlers (Windows) it
//     installed, closes the network poller and hands the destructor the
//     list of address ranges the runtime mapped (forgoMem), which the
//     destructor unmaps once Go code has returned.
//
// When the process exits instead, the destructor does nothing, as before.

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Filled in by runtime/cgo; see gcc_forgo_unload_unix.c and
// gcc_libinit_windows.c.
//
//go:linkname _cgo_forgo_lib_init _cgo_forgo_lib_init
//go:linkname _cgo_forgo_join _cgo_forgo_join
//go:linkname _cgo_forgo_m0_exit _cgo_forgo_m0_exit
var (
	_cgo_forgo_lib_init unsafe.Pointer
	_cgo_forgo_join     unsafe.Pointer
	_cgo_forgo_m0_exit  unsafe.Pointer
)

// forgoUnloading is set once the unload has begun. An M that sees it at a
// parking point exits.
var forgoUnloading atomic.Bool

// forgoUnloadable reports whether this runtime is a c-shared library on a
// platform where unloading it is supported.
//
//go:nosplit
func forgoUnloadable() bool {
	if !islibrary {
		return false
	}
	switch GOOS {
	case "linux", "windows":
		return GOARCH == "amd64" || GOARCH == "arm64"
	case "darwin":
		return GOARCH == "arm64"
	}
	return false
}

// forgoUnloadArg is filled in for the library's destructor.
// Keep in sync with struct forgo_unload_arg in runtime/cgo.
type forgoUnloadArg struct {
	ok          uintptr
	regions     unsafe.Pointer
	nregions    uintptr
	regionsSize uintptr
	tlsKey      uintptr
}

// forgoUnloadCallback is called by the library's destructor through
// crosscall2. frame is a *forgoUnloadArg in C memory.
func forgoUnloadCallback(frame unsafe.Pointer) {
	forgoUnload((*forgoUnloadArg)(frame))
}

// forgoStragglerTimeout is how long the unload waits for Ms that are in
// system calls or C calls to come back.
const forgoStragglerTimeout = 5e9

func forgoUnload(a *forgoUnloadArg) {
	stopTheWorld(stwForgoUnload)
	self := getg().m

	// A host thread that is inside a call into this library still has Go
	// frames on its stack; the library cannot go away under it.
	lock(&sched.lock)
	for mp := allm; mp != nil; mp = mp.alllink {
		if mp != self && mp.isextra && !mp.isExtraInC {
			unlock(&sched.lock)
			throw("unloading a Go library while a call into it is in progress")
		}
	}
	unlock(&sched.lock)

	forgoUnloading.Store(true)
	start := nanotime()
	for {
		forgoWakeAll()
		running := 0
		lock(&sched.lock)
		for mp := allm; mp != nil; mp = mp.alllink {
			if mp != self && !mp.isextra && !mp.forgoExited.Load() {
				running++
			}
		}
		unlock(&sched.lock)
		if running == 0 {
			break
		}
		if nanotime()-start > forgoStragglerTimeout {
			print("runtime: ", running, " threads are still in system calls or C calls\n")
			throw("unloading a Go library while goroutines are blocked in system calls or C code")
		}
		usleep(1000)
	}
	forgoJoinThreads()

	forgoRestoreHandlers()
	forgoClosePoller()

	// Other host threads that called into Go may still use the signal
	// stack their extra M gave them; keep those mapped.
	for mp := allm; mp != nil; mp = mp.alllink {
		if mp.isextra && mp.newSigstack && mp != self && mp.gsignal != nil {
			s := mp.gsignal.stack
			forgoMemRemove(unsafe.Pointer(s.lo), s.hi-s.lo)
		}
	}
	forgoReleaseSignalStack(self)
	a.tlsKey = forgoTLSKey()
	a.regions, a.nregions, a.regionsSize = forgoMemTake()
	a.ok = 1
}

// forgoWakeAll wakes every parked M, sysmon, the template thread and the
// network poller, so that they see forgoUnloading.
func forgoWakeAll() {
	lock(&sched.lock)
	// Take idle Ms off the idle list so that nothing else wakes them too.
	for mget() != nil {
	}
	for mp := allm; mp != nil; mp = mp.alllink {
		if !mp.isextra && !mp.forgoExited.Load() {
			forgoNoteWake(&mp.park)
		}
	}
	if sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	lock(&newmHandoff.lock)
	if newmHandoff.waiting {
		newmHandoff.waiting = false
		notewakeup(&newmHandoff.wake)
	}
	unlock(&newmHandoff.lock)
	forgoWakeSignalReceiver()
	if netpollinited() {
		netpollBreak()
	}
}

// forgoCheckExit is called by an M on its g0 stack at the points where it
// parks. If the library is being unloaded it does not return.
//
//go:nosplit
func forgoCheckExit() {
	if forgoUnloading.Load() {
		forgoMExit()
	}
}

// forgoMExit unwinds the M to mstart0, which calls forgoThreadExit.
//
//go:nosplit
func forgoMExit() {
	mp := getg().m
	gogo(&mp.g0.sched)
}

// forgoThreadExit runs at the top of an M's stack when the library is
// being unloaded. Once it marks the M exited the thread runs no more Go
// code.
func forgoThreadExit() {
	mp := getg().m
	forgoThreadExitOS(mp)
	mp.forgoExited.Store(true)
	forgoThreadExitFinal()
	if mp == &m0 {
		forgoM0Exit()
	}
}

// forgoMem lists the address ranges this runtime mapped from the OS, so
// that the unload can unmap them. It is only kept when forgoUnloadable.
var forgoMem struct {
	lock uint32
	r    uintptr // array of cap forgoMemRanges, mapped with forgoMapRaw
	n    uintptr
	cap  uintptr
}

type forgoMemRange struct {
	base, len uintptr
}

//go:nosplit
func forgoMemLock() {
	for !atomic.Cas(&forgoMem.lock, 0, 1) {
		procyield(10)
	}
}

//go:nosplit
func forgoMemUnlock() {
	atomic.Store(&forgoMem.lock, 0)
}

//go:nosplit
func forgoMemAt(i uintptr) *forgoMemRange {
	return (*forgoMemRange)(unsafe.Pointer(forgoMem.r + i*unsafe.Sizeof(forgoMemRange{})))
}

// forgoMemAppend adds a range. forgoMem must be locked.
//
//go:nosplit
func forgoMemAppend(base, n uintptr) {
	if forgoMem.n == forgoMem.cap {
		ncap := forgoMem.cap * 2
		if ncap == 0 {
			ncap = 4096 / unsafe.Sizeof(forgoMemRange{})
		}
		size := ncap * unsafe.Sizeof(forgoMemRange{})
		r := forgoMapRaw(size)
		if r == nil {
			throw("runtime: cannot record memory mapping")
		}
		if forgoMem.r != 0 {
			memmove(r, unsafe.Pointer(forgoMem.r), forgoMem.n*unsafe.Sizeof(forgoMemRange{}))
			forgoUnmapRaw(unsafe.Pointer(forgoMem.r), forgoMem.cap*unsafe.Sizeof(forgoMemRange{}))
		}
		forgoMem.r = uintptr(r)
		forgoMem.cap = ncap
	}
	*forgoMemAt(forgoMem.n) = forgoMemRange{base, n}
	forgoMem.n++
}

// forgoMemAdd records a new mapping [p, p+n).
//
//go:nosplit
func forgoMemAdd(p unsafe.Pointer, n uintptr) {
	if !forgoUnloadable() || p == nil || n == 0 {
		return
	}
	forgoMemLock()
	forgoMemAppend(uintptr(p), n)
	forgoMemUnlock()
}

// forgoMemRemove forgets [v, v+n), which has been unmapped or must stay
// mapped. Ranges that overlap it partly are trimmed or split.
//
//go:nosplit
func forgoMemRemove(v unsafe.Pointer, n uintptr) {
	if !forgoUnloadable() || n == 0 {
		return
	}
	lo, hi := uintptr(v), uintptr(v)+n
	forgoMemLock()
	for i := uintptr(0); i < forgoMem.n; i++ {
		r := forgoMemAt(i)
		rlo, rhi := r.base, r.base+r.len
		if rhi <= lo || hi <= rlo {
			continue
		}
		switch {
		case lo <= rlo && rhi <= hi:
			forgoMem.n--
			*r = *forgoMemAt(forgoMem.n)
			i--
		case rlo < lo && hi < rhi:
			r.len = lo - rlo
			forgoMemAppend(hi, rhi-hi)
		case rlo < lo:
			r.len = lo - rlo
		default:
			r.base, r.len = hi, rhi-hi
		}
	}
	forgoMemUnlock()
}

// forgoMemTake returns the recorded ranges as address, length pairs and the
// size of the mapping that holds them.
func forgoMemTake() (unsafe.Pointer, uintptr, uintptr) {
	forgoMemLock()
	defer forgoMemUnlock()
	return unsafe.Pointer(forgoMem.r), forgoMem.n, forgoMem.cap * unsafe.Sizeof(forgoMemRange{})
}
