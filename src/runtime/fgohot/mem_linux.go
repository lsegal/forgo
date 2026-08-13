// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fgohot

import (
	"errors"
	"syscall"
)

const (
	protR   = syscall.PROT_READ
	protRW  = syscall.PROT_READ | syscall.PROT_WRITE
	protRX  = syscall.PROT_READ | syscall.PROT_EXEC
	protRWX = syscall.PROT_READ | syscall.PROT_WRITE | syscall.PROT_EXEC
)

// mapFixedNoreplace fails the mapping instead of moving an existing one.
// Linux 4.17 and later; older kernels ignore it, which the range check below
// then catches.
const mapFixedNoreplace = 0x100000

func pageSize() int { return syscall.Getpagesize() }

// reserve claims size bytes of address space close enough to the program's
// code that a direct jump can reach it, without committing any memory.
func reserve(near, size uintptr) (uintptr, error) {
	ps := uintptr(pageSize())
	// Start just past the program's text and walk forward. Staying within
	// 2GB of the text is what lets a five byte jump reach the new code.
	const gap = 64 << 20
	for addr := (near + gap) &^ (ps - 1); addr < near+(1<<31)-size; addr += 256 << 20 {
		p, _, errno := syscall.Syscall6(syscall.SYS_MMAP, addr, size,
			syscall.PROT_NONE,
			syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE|mapFixedNoreplace,
			^uintptr(0), 0)
		if errno != 0 {
			continue
		}
		if p == addr {
			return p, nil
		}
		// An old kernel ignored MAP_FIXED_NOREPLACE and put it elsewhere.
		syscall.Syscall(syscall.SYS_MUNMAP, p, size, 0)
		if p > near && p-near < 1<<31 {
			return p, nil
		}
	}
	return 0, errors.New("no free address space within reach of the program's code")
}

// commit backs a slice of the reserved region with memory.
func commit(addr, size uintptr) error {
	return protect(addr, size, protRW)
}

func protect(addr, size uintptr, prot int) error {
	ps := uintptr(pageSize())
	end := (addr + size + ps - 1) &^ (ps - 1)
	addr &^= ps - 1
	if _, _, errno := syscall.Syscall(syscall.SYS_MPROTECT, addr, end-addr, uintptr(prot)); errno != 0 {
		return errno
	}
	return nil
}
