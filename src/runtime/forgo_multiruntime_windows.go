// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Console control events for several Go runtimes in one process.
//
// Every Go runtime registers its own console control handler, and Windows
// calls the newest one first, stopping at the first that returns TRUE. A
// runtime returns TRUE when os/signal.Notify wants the event, so of several
// runtimes that asked for os.Interrupt or SIGTERM only the one loaded last
// would get it, and for a close, logoff, or shutdown event it then blocks
// until Windows ends the process, so the others never get to clean up.
//
// The runtimes find each other through a small named file mapping keyed by
// the process ID. Each one stores a callback there that delivers an event to
// its own os/signal and reports whether anybody was listening. The first
// handler Windows calls delivers the event to every registered runtime, then
// keeps it if any of them, itself included, wanted it. C handlers below it
// still lose the event, as they would to a single Go runtime.

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

//go:cgo_import_dynamic runtime._CreateFileMappingW CreateFileMappingW%6 "kernel32.dll"
//go:cgo_import_dynamic runtime._MapViewOfFile MapViewOfFile%5 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetCurrentProcessId GetCurrentProcessId%0 "kernel32.dll"

var (
	_CreateFileMappingW,
	_MapViewOfFile,
	_GetCurrentProcessId stdFunction
)

const (
	forgoCtrlMaxRuntimes = 64
	forgoFileMapWrite    = 0x0002
)

var (
	// forgoCtrlSlots is the process-wide list of delivery callbacks, one per
	// runtime, filled from the front. It is nil if the mapping failed.
	forgoCtrlSlots *[forgoCtrlMaxRuntimes]uintptr
	// forgoCtrlSelf is this runtime's own delivery callback.
	forgoCtrlSelf uintptr
)

// forgoCtrlRegister adds this runtime to the process's list. It is called
// right after the runtime installs its console control handler.
func forgoCtrlRegister() {
	var name [64]uint16
	n := 0
	for _, c := range "Local\\forgo-console-ctrl-" {
		name[n] = uint16(c)
		n++
	}
	var buf [20]byte
	for _, c := range itoa(buf[:], uint64(stdcall(_GetCurrentProcessId))) {
		name[n] = uint16(c)
		n++
	}
	const size = forgoCtrlMaxRuntimes * unsafe.Sizeof(uintptr(0))
	h := stdcall(_CreateFileMappingW, ^uintptr(0), 0, _PAGE_READWRITE, 0, size, uintptr(unsafe.Pointer(&name[0])))
	if h == 0 {
		return
	}
	// The handle stays open for the life of the process, like the mapping.
	p := stdcall(_MapViewOfFile, h, forgoFileMapWrite, 0, 0, size)
	if p == 0 {
		return
	}
	var fn any = forgoCtrlDeliver
	self := compileCallback(*efaceOf(&fn), true)
	slots := (*[forgoCtrlMaxRuntimes]uintptr)(unsafe.Pointer(p))
	for i := range slots {
		if atomic.Casuintptr(&slots[i], 0, self) {
			forgoCtrlSelf = self
			forgoCtrlSlots = slots
			return
		}
	}
}

// forgoCtrlDeliver is the callback another runtime calls to deliver signal s
// to this runtime's os/signal. It reports whether this runtime wanted it.
func forgoCtrlDeliver(s uint32) uintptr {
	if sigsend(s) {
		return 1
	}
	return 0
}

// forgoCtrlSend delivers signal s, for a console control event, to this
// runtime and to every other registered runtime, and reports whether any of
// them wanted it. ctrlHandler calls it in place of sigsend.
func forgoCtrlSend(s uint32) bool {
	ok := sigsend(s)
	slots := forgoCtrlSlots
	if slots == nil {
		return ok
	}
	for i := range slots {
		pc := atomic.Loaduintptr(&slots[i])
		if pc == 0 {
			break
		}
		if pc != forgoCtrlSelf && stdcall(stdFunction(unsafe.Pointer(pc)), uintptr(s)) != 0 {
			ok = true
		}
	}
	return ok
}
