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
//
// os/signal.Notify has the same problem. In a library, Notify installs a Go
// handler for an asynchronous signal on top of whatever was there, and that
// handler kept the signal to itself, so when two runtimes asked for the same
// signal only the one that asked last ever got it. A handler installed for
// Notify now also passes every signal on to the handler below it, so every
// runtime that asked for it, and a host handler installed before them, gets
// it. Reset and Stop used to put back the handler this runtime found, even
// when another runtime had since installed its own on top, which silently
// removed the other runtime's handler; a runtime that is no longer on top now
// stays in the chain and just passes signals through. Calling Notify again
// while still in the chain reuses that place instead of installing a second
// handler that would forward to itself.

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// forgoSigfwdForeign forwards sigPreempt and SIGPROF to the previously
// installed handler when they arrive on a thread that is not executing this
// runtime's Go code, and signals this runtime handles for os/signal.Notify
// wherever they arrive. It is called from sigtrampgo after sigfwdgo has decided
// that this runtime handles the signal; this runtime's own handling still
// runs afterwards.
//
//go:nosplit
//go:nowritebarrierrec
func forgoSigfwdForeign(sig uint32, info *siginfo, ctx unsafe.Pointer) {
	if !isarchive && !islibrary || sig >= _NSIG {
		return
	}
	fwdFn := atomic.Loaduintptr(&fwdSig[sig])
	if fwdFn == _SIG_DFL || fwdFn == _SIG_IGN {
		return
	}
	if sig != sigPreempt && sig != _SIGPROF {
		// A signal this runtime only handles for os/signal.Notify goes
		// to the handlers below as well, wherever it arrives.
		if atomic.Loaduintptr(&forgoSigHandler[sig]) != 0 {
			sigfwd(fwdFn, sig, info, ctx)
		}
		return
	}
	gp := sigFetchG(&sigctxt{info, ctx})
	if gp != nil && gp.m != nil && gp.m.curg != nil && !gp.m.isExtraInC && !gp.m.incgo {
		// This runtime is running Go code here, so the signal is ours.
		return
	}
	sigfwd(fwdFn, sig, info, ctx)
}

// forgoSigHandler[sig] is the handler sigenable installed for sig on behalf
// of os/signal.Notify, as getsig reports it, or 0 when this runtime has no
// such handler in the process's chain for sig.
var forgoSigHandler [_NSIG]uintptr

// forgoSigInstalled records the handler sigenable just installed for sig.
func forgoSigInstalled(sig uint32) {
	atomic.Storeuintptr(&forgoSigHandler[sig], getsig(sig))
}

// forgoSigStillChained reports whether the handler this runtime installed
// for sig is still in the process's chain, so that sigenable must not
// install another one.
func forgoSigStillChained(sig uint32) bool {
	return atomic.Loaduintptr(&forgoSigHandler[sig]) != 0
}

// forgoSigKeepChained reports whether sigdisable must leave this runtime's
// handler for sig in place because another handler has been installed on
// top of it and forwards to it. Otherwise sigdisable restores the previous
// handler and this runtime leaves the chain.
func forgoSigKeepChained(sig uint32) bool {
	if forgoSigForwardedCopy(sig) {
		return true
	}
	atomic.Storeuintptr(&forgoSigHandler[sig], 0)
	return false
}

// forgoSigUnchained records that sigignore replaced every handler for sig.
func forgoSigUnchained(sig uint32) {
	atomic.Storeuintptr(&forgoSigHandler[sig], 0)
}

// forgoSigForwardedCopy reports whether this runtime's handler for sig is in
// the chain below another handler. A signal that reaches it while this
// runtime is not handling sig was forwarded by that other handler, which
// already handled it, so it must not get the default action.
//
//go:nosplit
//go:nowritebarrierrec
func forgoSigForwardedCopy(sig uint32) bool {
	h := atomic.Loaduintptr(&forgoSigHandler[sig])
	return h != 0 && getsig(sig) != h
}

// syscall_forgoRuntimeIsLibrary reports whether this runtime was built into
// a c-shared or c-archive library. A library leaves process-wide limits,
// such as the open-file limit a Go program raises at startup, to its host:
// a runtime loaded after another one would otherwise record the limit the
// first runtime raised as the host's original and hand that to its child
// processes.
//
//go:linkname syscall_forgoRuntimeIsLibrary syscall.forgoRuntimeIsLibrary
func syscall_forgoRuntimeIsLibrary() bool { return isarchive || islibrary }
