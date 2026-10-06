// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

// Signal routing between several Go runtimes in one process.
//
// A process can hold more than one Go runtime: a Go program that loads a
// c-shared library, or a C host (a DAW loading VST/CLAP plugins, Python
// loading extensions) that loads several c-shared libraries. Each runtime
// keeps its own heap, scheduler and g, and they only talk through the C ABI.
// Signal handlers are process-wide, though: the runtime that loaded last owns
// the handler and chains to the one before it through fwdSig.
//
// Synchronous signals (SIGSEGV, SIGBUS, ...) are already forwarded correctly
// by sigfwdgo: when the faulting thread is not running this runtime's Go
// code, the signal goes to the previous handler. The asynchronous signals a
// runtime sends to its own threads are not. preemptM sends sigPreempt
// (SIGURG) to one of its Ms and will not send another until that M
// acknowledges it; profiling sends SIGPROF. When the top runtime finds no g
// of its own on the thread, sigtrampgo treats the signal as stray and drops
// it, so the other runtime never sees its own preemption request. Its M keeps
// signalPending set forever, async preemption of that M stops working, and
// the next stop-the-world that has to interrupt a tight loop hangs.
//
// forgoSigfwdForeign forwards those signals to the previous handler whenever
// this runtime is not running Go code on the thread, so the runtime that sent
// them gets to handle them. Spurious SIGURG and SIGPROF are harmless by
// design, so forwarding one that turns out to be stale is safe.

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// forgoSigfwdForeign forwards sigPreempt and SIGPROF to the previously
// installed handler when they arrive on a thread that is not executing this
// runtime's Go code. It is called from sigtrampgo after sigfwdgo has decided
// that this runtime handles the signal; this runtime's own handling still
// runs afterwards.
//
//go:nosplit
//go:nowritebarrierrec
func forgoSigfwdForeign(sig uint32, info *siginfo, ctx unsafe.Pointer) {
	if !isarchive && !islibrary {
		return
	}
	if sig != sigPreempt && sig != _SIGPROF {
		return
	}
	fwdFn := atomic.Loaduintptr(&fwdSig[sig])
	if fwdFn == _SIG_DFL || fwdFn == _SIG_IGN {
		return
	}
	gp := sigFetchG(&sigctxt{info, ctx})
	if gp != nil && gp.m != nil && gp.m.curg != nil && !gp.m.isExtraInC && !gp.m.incgo {
		// This runtime is running Go code here, so the signal is ours.
		return
	}
	sigfwd(fwdFn, sig, info, ctx)
}
