// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux || darwin

// Unix half of unloading a c-shared library; see forgo_unload.go.

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe"
)

// forgoSigSaved holds the full handler each signal had before this runtime
// installed its own, so the unload can put it back.
var forgoSigSaved [_NSIG]forgoSigaction

// forgoSaveSig records sig's current handler. It is called wherever
// fwdSig[sig] is set, before the runtime installs its own handler.
//
//go:nosplit
//go:nowritebarrierrec
func forgoSaveSig(sig uint32) {
	if forgoUnloadable() {
		forgoSigStubInit()
		forgoGetSigaction(sig, &forgoSigSaved[sig])
	}
}

// Signal handlers form a chain: a handler installed after this runtime's
// (another Go runtime, a crash reporter) saves it and forwards signals to
// it, so the runtime cannot take its handler out of the chain while one is
// on top. Instead it installs its handler through a stub that runtime/cgo
// maps outside the library (see gcc_forgo_unload_unix.c), which stays
// mapped after the library is gone. At unload the stub is pointed at the
// handler that was installed before this runtime's.

// forgoSigStubTable is the stub's table.
// Keep in sync with struct forgo_sigstub_table in runtime/cgo.
type forgoSigStubTable struct {
	owner    uintptr
	signalFn uintptr
	raiseFn  uintptr
	_        uintptr
	ent      [128]struct{ fn, kind uintptr }
}

// What the stub does with a signal; see gcc_forgo_unload_unix.c.
const (
	forgoStubJump         = 0 // jump to fn
	forgoStubJumpUnmarked = 1 // drop a signal another forgo runtime handed down, else jump to fn
	forgoStubIgnore       = 2 // return
	forgoStubDefault      = 3 // drop a marked signal, else take the default action
)

// forgoSigStub is the stub, filled in by x_cgo_forgo_sigstub_init; entry
// is 0 when there is none and the runtime installs its handler directly.
var forgoSigStub struct {
	owner, entry uintptr
	table        *forgoSigStubTable
	base, size   uintptr
}

var forgoSigStubTried bool

// Filled in by runtime/cgo; see gcc_forgo_unload_unix.c.
//
//go:linkname _cgo_forgo_sigstub_init _cgo_forgo_sigstub_init
var _cgo_forgo_sigstub_init unsafe.Pointer

// forgoSigStubbed[sig] is set while this runtime has the stub installed
// for sig.
var forgoSigStubbed [_NSIG]bool

// forgoSigStubInit maps the stub. It runs from initsig, before the runtime
// installs any handler and without a g.
//
//go:nosplit
//go:nowritebarrierrec
func forgoSigStubInit() {
	if forgoSigStubTried || _cgo_forgo_sigstub_init == nil {
		return
	}
	forgoSigStubTried = true
	forgoSigStub.owner = abi.FuncPCABI0(cgoSigtramp)
	asmcgocall_no_g(_cgo_forgo_sigstub_init, unsafe.Pointer(&forgoSigStub))
}

// forgoSigStubFn returns the handler setsig should install for sig in
// place of fn: the stub, when fn is this runtime's handler.
//
//go:nosplit
//go:nowritebarrierrec
func forgoSigStubFn(sig uint32, fn uintptr) uintptr {
	if forgoSigStub.entry == 0 || sig >= _NSIG {
		return fn
	}
	if fn != abi.FuncPCABI0(cgoSigtramp) {
		forgoSigStubbed[sig] = false
		return fn
	}
	e := &forgoSigStub.table.ent[sig]
	atomic.Storeuintptr(&e.fn, fn)
	atomic.Storeuintptr(&e.kind, forgoStubJump)
	forgoSigStubbed[sig] = true
	return forgoSigStub.entry
}

// forgoSigStubRedirect points the stub for sig at the handler that was
// installed before this runtime's, or at its default or ignore behaviour.
func forgoSigStubRedirect(sig uint32) {
	e := &forgoSigStub.table.ent[sig]
	kind := uintptr(forgoStubJumpUnmarked)
	switch h := forgoSigactionHandler(&forgoSigSaved[sig]); h {
	case _SIG_IGN:
		kind = forgoStubIgnore
	case _SIG_DFL:
		// A stray SIGPROF is ignored, as in setProcessCPUProfilerTimer.
		kind = forgoStubDefault
		if sigtable[sig].flags&_SigIgn != 0 || sig == _SIGPROF {
			kind = forgoStubIgnore
		}
	default:
		if atomic.Load(&forgoSigBelowGo[sig]) != 0 {
			// Another forgo runtime's Notify handler, which wants
			// the marked signals.
			kind = forgoStubJump
		}
		atomic.Storeuintptr(&e.fn, h)
	}
	atomic.Storeuintptr(&e.kind, kind)
}

// forgoRestoreHandlers puts back the handlers that were installed before
// this runtime's. Where its stub is still the installed handler it is
// simply replaced; where another handler sits on top, the stub is
// redirected and stays mapped. A handler installed without the stub can
// only be removed while it is still the installed one.
func forgoRestoreHandlers() {
	ours := abi.FuncPCABI0(cgoSigtramp)
	chained := false
	for i := uint32(0); i < _NSIG; i++ {
		if forgoSigStubbed[i] {
			forgoSigStubRedirect(i)
			if getsig(i) == forgoSigStub.entry {
				forgoSetSigaction(i, &forgoSigSaved[i])
			} else {
				chained = true
			}
			continue
		}
		if handlingSig[i] != 0 && getsig(i) == ours {
			forgoSetSigaction(i, &forgoSigSaved[i])
		}
	}
	if forgoSigStub.entry == 0 {
		return
	}
	atomic.Storeuintptr(&forgoSigStub.table.owner, 0)
	if !chained {
		// Nothing forwards to the stub any more; the destructor
		// unmaps it with the rest.
		forgoMemAdd(unsafe.Pointer(forgoSigStub.base), forgoSigStub.size)
	}
}

func forgoClosePoller() {
	if !netpollinited() {
		return
	}
	forgoClosePollerOS()
}

// forgoJoinThreads waits until the threads of every exited M are gone.
func forgoJoinThreads() {
	asmcgocall(_cgo_forgo_join, nil)
}

// forgoWakeSignalReceiver wakes signal_recv, which sleeps in a system
// call, the same way sigsend does.
func forgoWakeSignalReceiver() {
	if sig.state.CompareAndSwap(sigReceiving, sigIdle) {
		notewakeup(&sig.note)
	}
}

func forgoThreadExitOS(mp *m) {
	sigblock(true)
	unminit()
}

// forgoM0Exit leaves the runtime's first thread, which cannot return from
// mstart.
func forgoM0Exit() {
	asmcgocall(_cgo_forgo_m0_exit, nil)
	throw("forgo: m0 exit returned")
}

//go:nosplit
func forgoMapRaw(n uintptr) unsafe.Pointer {
	p, err := mmap(nil, n, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	if err != 0 {
		return nil
	}
	return p
}

//go:nosplit
func forgoUnmapRaw(p unsafe.Pointer, n uintptr) {
	munmap(p, n)
}

// forgoThreadExitFinal returns, so the thread returns from mstart into
// runtime/cgo's threadentry and exits there.
//
//go:nosplit
func forgoThreadExitFinal() {}

// forgoLibInit enables unloading. It runs from libInit before the runtime
// starts its first thread, without a g.
//
//go:nosplit
func forgoLibInit() {
	if forgoUnloadable() && _cgo_forgo_lib_init != nil {
		asmcgocall_no_g(_cgo_forgo_lib_init, unsafe.Pointer(abi.FuncPCABIInternal(forgoUnloadCallback)))
	}
}

// forgoReleaseSignalStack keeps the signal stack the unloading thread's
// extra M gave it mapped, as for every other host thread. Another runtime
// that the thread called into may have found it installed and use it as
// its own, so it can be neither unmapped nor disabled.
func forgoReleaseSignalStack(mp *m) {
	if mp.newSigstack && mp.gsignal != nil {
		s := mp.gsignal.stack
		forgoMemRemove(unsafe.Pointer(s.lo), s.hi-s.lo)
	}
}
