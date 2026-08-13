// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fgohot

import (
	"errors"
	"syscall"
	"unsafe"
)

const (
	protR   = 0x02 // PAGE_READONLY
	protRW  = 0x04 // PAGE_READWRITE
	protRX  = 0x20 // PAGE_EXECUTE_READ
	protRWX = 0x40 // PAGE_EXECUTE_READWRITE

	memCommit  = 0x1000
	memReserve = 0x2000
	memRelease = 0x8000
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc   = kernel32.NewProc("VirtualAlloc")
	procVirtualFree    = kernel32.NewProc("VirtualFree")
	procVirtualProtec  = kernel32.NewProc("VirtualProtect")
	procLoadLibraryA   = kernel32.NewProc("LoadLibraryA")
	procGetProcAddress = kernel32.NewProc("GetProcAddress")
)

func pageSize() int { return syscall.Getpagesize() }

// reserve claims size bytes of address space close enough to the program's
// code that a direct jump can reach it, without committing any memory. On
// success it always returns exactly size — see mem_darwin.go's reserve for
// why the return also carries a (possibly smaller) size at all.
func reserve(near, size uintptr) (uintptr, uintptr, error) {
	// VirtualAlloc rounds a requested base down to the 64KB allocation
	// granularity, so walk forward in granularity-sized steps.
	const granularity = 64 << 10
	const gap = 64 << 20
	for addr := (near + gap) &^ (granularity - 1); addr < near+maxReach-size; addr += probeStep {
		p, _, _ := procVirtualAlloc.Call(addr, size, memReserve, protR)
		if p == 0 {
			continue
		}
		if p >= near && p-near < maxReach {
			return p, size, nil
		}
		procVirtualFree.Call(p, 0, memRelease)
	}
	return 0, 0, errors.New("no free address space within reach of the program's code")
}

// commit backs a slice of the reserved region with memory.
func commit(addr, size uintptr) error {
	p, _, err := procVirtualAlloc.Call(addr, size, memCommit, protRW)
	if p == 0 {
		return err
	}
	return nil
}

func protect(addr, size uintptr, prot int) error {
	ps := uintptr(pageSize())
	end := (addr + size + ps - 1) &^ (ps - 1)
	addr &^= ps - 1
	var old uint32
	r, _, err := procVirtualProtec.Call(addr, end-addr, uintptr(prot), uintptr(unsafe.Pointer(&old)))
	if r == 0 {
		return err
	}
	return nil
}
