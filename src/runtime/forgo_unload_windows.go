// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Windows half of unloading a c-shared library; see forgo_unload.go.

package runtime

import (
	"internal/abi"
	"internal/runtime/syscall/windows"
	"unsafe"
)

// Process-wide callbacks the runtime registered, which the unload removes.
var (
	forgoExceptionHandlers [3]uintptr // AddVectored*Handler results
	forgoCtrlHandlerPC     uintptr
	forgoPowerHandle       uintptr
	forgoPowerUnregister   stdFunction
)

//go:nosplit
//go:nowritebarrierrec
func forgoSaveSig(sig uint32) {}

func forgoRestoreHandlers() {
	if forgoExceptionHandlers[0] != 0 {
		stdcall(_RemoveVectoredExceptionHandler, forgoExceptionHandlers[0])
	}
	for _, h := range forgoExceptionHandlers[1:] {
		if h != 0 {
			stdcall(_RemoveVectoredContinueHandler, h)
		}
	}
	if forgoCtrlHandlerPC != 0 {
		stdcall(_SetConsoleCtrlHandler, forgoCtrlHandlerPC, 0)
	}
	if forgoPowerHandle != 0 && forgoPowerUnregister != nil {
		stdcall(forgoPowerUnregister, forgoPowerHandle)
	}
}

func forgoClosePoller() {
	if netpollinited() && iocphandle != windows.INVALID_HANDLE_VALUE {
		stdcall(_CloseHandle, iocphandle)
	}
}

// forgoWakeSignalReceiver wakes signal_recv, which sleeps in a system
// call, the same way sigsend does.
func forgoWakeSignalReceiver() {
	if sig.state.CompareAndSwap(sigReceiving, sigIdle) {
		notewakeup(&sig.note)
	}
}

func forgoTLSKey() uintptr { return 0 }

func forgoThreadExitOS(mp *m) {
	// Keep mp.thread open: forgoJoinThreads needs it.
	mdestroy(mp)
}

// forgoM0Exit is unused on Windows: every M ends in ExitThread, see
// forgoThreadExitFinal.
func forgoM0Exit() {}

// forgoThreadExitFinal ends the thread. The loader lock is held while the
// library unloads, so the thread may block in ExitThread until the unload
// is over, but it is out of the library's code.
//
//go:nosplit
func forgoThreadExitFinal() {
	stdcall(_ExitThread, 0)
}

// forgoJoinThreads waits until every exited M's thread has left Go code.
// The threads cannot finish exiting while the loader lock is held, so it
// checks where each one is instead of waiting for it.
func forgoJoinThreads() {
	self := getg().m
	var c *windows.Context
	var cbuf [unsafe.Sizeof(*c) + 15]byte
	c = (*windows.Context)(unsafe.Pointer((uintptr(unsafe.Pointer(&cbuf[15]))) &^ 15))
	for mp := allm; mp != nil; mp = mp.alllink {
		if mp == self || mp.isextra || mp.thread == 0 {
			continue
		}
		for {
			if stdcall(_WaitForSingleObject, mp.thread, 0) == 0 {
				break // WAIT_OBJECT_0: gone
			}
			c.ContextFlags = windows.CONTEXT_CONTROL
			out := false
			if int32(stdcall(_SuspendThread, mp.thread)) != -1 {
				if stdcall(_GetThreadContext, mp.thread, uintptr(unsafe.Pointer(c))) != 0 {
					pc := c.PC()
					out = pc < firstmoduledata.text || firstmoduledata.etext <= pc
				}
				stdcall(_ResumeThread, mp.thread)
			}
			if out {
				break
			}
			usleep(100)
		}
		stdcall(_CloseHandle, mp.thread)
		mp.thread = 0
	}
}

//go:nosplit
func forgoMapRaw(n uintptr) unsafe.Pointer {
	return unsafe.Pointer(stdcall(_VirtualAlloc, 0, n, _MEM_COMMIT|_MEM_RESERVE, _PAGE_READWRITE))
}

//go:nosplit
func forgoUnmapRaw(p unsafe.Pointer, n uintptr) {
	stdcall(_VirtualFree, uintptr(p), 0, _MEM_RELEASE)
}

// forgoLibInit enables unloading. It runs from libInit before the runtime
// starts its first thread, without a g.
//
//go:nosplit
func forgoLibInit() {
	if forgoUnloadable() && _cgo_forgo_lib_init != nil {
		asmcgocall_no_g(_cgo_forgo_lib_init, unsafe.Pointer(abi.FuncPCABIInternal(forgoUnloadCallback)))
	}
}

func forgoReleaseSignalStack(mp *m) {}
