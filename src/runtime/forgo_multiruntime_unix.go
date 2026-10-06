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
// os/signal.Notify also changes process-wide state. In a library, Notify
// installs a Go handler for an asynchronous signal on top of whatever was
// there and keeps the signal, so of several runtimes that ask for the same
// signal, the one that asked last gets it, as it would from a C handler
// installed before it. Reset and Stop used to put back the handler this
// runtime found even when another runtime had since installed its own on top,
// which silently took the signal away from that runtime. A runtime whose
// handler is no longer on top now leaves it in place and just passes signals
// through to the handler below, and the signal returns to it once the runtime
// above calls Reset. Calling Notify again while its handler is still in the
// chain reuses that place instead of installing a second handler, which
// would forward to itself forever.
//
// Notify signals also reach every other forgo runtime that asked for them.
// When the handler below this runtime's Notify handler is another forgo
// runtime's (forgoSigBelowGo), this runtime hands the signal down to it as
// well as keeping it, and marks the copy it hands down (forgoSigMark). A
// runtime that receives a marked signal it is not listening for passes it on
// only to another forgo runtime, never to a C handler or the default action,
// so a C handler installed before the libraries still loses the signal to
// Notify, as upstream documents. Telling the handlers apart needs the
// dynamic loader (see runtime/cgo/gcc_forgo_multiruntime.c): the module
// that holds the handler below must export _forgo_cgo_sighandler, and it
// must report that very handler.

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

// forgoSigHandler[sig] is the handler sigenable installed for sig on behalf
// of os/signal.Notify, as getsig reports it, or 0 when this runtime has no
// such handler in the process's chain for sig.
var forgoSigHandler [_NSIG]uintptr

// forgoSigBelowGo[sig] is 1 when the handler below this runtime's Notify
// handler for sig is another forgo runtime's Notify handler.
var forgoSigBelowGo [_NSIG]uint32

// _cgo_forgo_setsighandler and _cgo_forgo_isgosighandler are filled in by
// runtime/cgo on the platforms that support delivering Notify signals to
// several runtimes. See runtime/cgo/gcc_forgo_multiruntime.c.
//
//go:linkname _cgo_forgo_setsighandler
var _cgo_forgo_setsighandler unsafe.Pointer

//go:linkname _cgo_forgo_isgosighandler
var _cgo_forgo_isgosighandler unsafe.Pointer

// forgoSigInstalled records the handler sigenable just installed for sig,
// publishes it for the other runtimes in the process, and records whether
// the handler it replaced belongs to one of them.
func forgoSigInstalled(sig uint32) {
	h := getsig(sig)
	atomic.Storeuintptr(&forgoSigHandler[sig], h)
	if !isarchive && !islibrary || _cgo_forgo_setsighandler == nil || _cgo_forgo_isgosighandler == nil {
		return
	}
	asmcgocall(_cgo_forgo_setsighandler, unsafe.Pointer(&h))
	below := uint32(0)
	if fwd := atomic.Loaduintptr(&fwdSig[sig]); fwd != _SIG_DFL && fwd != _SIG_IGN {
		arg := [2]uintptr{fwd, 0}
		asmcgocall(_cgo_forgo_isgosighandler, unsafe.Pointer(&arg))
		below = uint32(arg[1])
	}
	atomic.Store(&forgoSigBelowGo[sig], below)
}

// forgoSigNotifyFwd hands an asynchronous Notify signal down to the forgo
// runtime below this one. It is called first thing in sigtrampgo and
// reports whether the signal is fully handled.
//
// A signal from the kernel that this runtime is listening for goes down
// marked, and this runtime then handles it as usual. A marked signal comes
// from a forgo runtime above: this runtime handles it if it is listening,
// passes it down if the handler below is another forgo runtime, and drops it
// otherwise.
//
//go:nosplit
//go:nowritebarrierrec
func forgoSigNotifyFwd(sig uint32, info *siginfo, ctx unsafe.Pointer) bool {
	if !isarchive && !islibrary || sig >= _NSIG || info == nil {
		return false
	}
	if sigtable[sig].flags&_SigPanic != 0 || sig == _SIGPIPE || sig == sigPreempt || sig == _SIGPROF {
		return false
	}
	marked := forgoSigMarked(info)
	if !marked && atomic.Loaduintptr(&forgoSigHandler[sig]) == 0 {
		return false
	}
	handling := atomic.Load(&handlingSig[sig]) != 0 && signalsOK
	below := atomic.Load(&forgoSigBelowGo[sig]) != 0
	if !marked && !handling {
		// Nobody above wants it: pass it down unchanged, as sigfwdgo does.
		return false
	}
	if below {
		fwdFn := atomic.Loaduintptr(&fwdSig[sig])
		if marked {
			sigfwd(fwdFn, sig, info, ctx)
		} else {
			fwd := *info
			forgoSigMark(&fwd)
			sigfwd(fwdFn, sig, &fwd, ctx)
		}
	}
	return marked && !handling
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
	h := atomic.Loaduintptr(&forgoSigHandler[sig])
	if h != 0 && getsig(sig) != h {
		return true
	}
	atomic.Storeuintptr(&forgoSigHandler[sig], 0)
	atomic.Store(&forgoSigBelowGo[sig], 0)
	return false
}

// forgoSigUnchained records that sigignore replaced every handler for sig.
func forgoSigUnchained(sig uint32) {
	atomic.Storeuintptr(&forgoSigHandler[sig], 0)
	atomic.Store(&forgoSigBelowGo[sig], 0)
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
