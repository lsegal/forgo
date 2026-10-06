// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux || darwin

// Unix half of unloading a c-shared library; see forgo_unload.go.

package runtime

import (
	"internal/abi"
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
		forgoGetSigaction(sig, &forgoSigSaved[sig])
	}
}

// forgoRestoreHandlers puts back the handlers that were installed before
// this runtime's. Signal handlers form a chain: a handler installed after
// this one may forward signals to it. Only when this runtime's handler is
// still the installed one can it be removed; otherwise it is left in place,
// which is why libraries must be unloaded in the reverse order of loading
// (see README.md).
func forgoRestoreHandlers() {
	ours := abi.FuncPCABI0(cgoSigtramp)
	for i := uint32(0); i < _NSIG; i++ {
		if handlingSig[i] == 0 {
			continue
		}
		if getsig(i) == ours {
			forgoSetSigaction(i, &forgoSigSaved[i])
		}
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

// forgoReleaseSignalStack stops the unloading thread from using the signal
// stack its extra M gave it, which is about to be unmapped.
func forgoReleaseSignalStack(mp *m) {
	if mp.newSigstack {
		unminitSignals()
		mp.newSigstack = false
	}
}
