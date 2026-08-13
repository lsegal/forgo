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

// The functions below are implemented in package runtime (in
// forgo_hot_darwin.go) and made available here with //go:linkname, because
// macOS syscalls — unlike Linux's — have to go through libSystem, and the
// trampolines that do that are runtime-internal.
func rtmmapfixed(addr, size uintptr, prot int32) (uintptr, int32)
func rtmprotect(addr, size uintptr, prot int32) int32
func rtvmregion(addr uintptr) (regionStart, regionSize uintptr, kr int32)

func pageSize() int { return syscall.Getpagesize() }

// minReserve is the smallest region reserve will settle for (see below) —
// below this it gives up rather than hand the agent a slice of address
// space too small to be useful for even one reload.
const minReserve = 256 << 10

// reserve claims up to size bytes of address space close enough to the
// program's code that a direct call from newly linked code can still reach
// pinned functions elsewhere in the program (see reach_arm64.go's maxReach),
// without committing any memory. Unlike every other platform forgo hot
// reload supports, it may hand back less than size: the caller must use the
// returned amount, not size, as how much it actually got.
//
// Unlike Linux, darwin has no MAP_FIXED_NOREPLACE — mmap either honors a
// hint address or silently maps somewhere else entirely (observed in
// practice: it does the latter essentially always, at least for a
// PROT_NONE anonymous mapping this size), and a plain MAP_FIXED would
// happily clobber whatever was already there instead of failing. So this
// finds a genuinely free stretch of address space itself first, with the
// same mach_vm_region query vmmap(1) uses to walk the process's own memory
// map, and only then asks for it with MAP_FIXED.
//
// Within maxReach — necessarily tight, unlike amd64's +-2GB — Go's own heap
// arena reservations (sysReserve's giant PROT_NONE placeholders) routinely
// blanket most of the address space around a small program's text on
// darwin/arm64, so a gap the full requested size may not exist at all. This
// asks for less, repeatedly halving, rather than fail outright: a small
// reload region still supports hot reload, just with a lower ceiling on how
// many generations run before the program needs restarting.
func reserve(near, size uintptr) (uintptr, uintptr, error) {
	for ; size >= minReserve; size /= 2 {
		if p, ok := reserveExact(near, size); ok {
			return p, size, nil
		}
	}
	return 0, 0, errors.New("no free address space within reach of the program's code")
}

// reserveExact is reserve's search at one fixed size.
func reserveExact(near, size uintptr) (uintptr, bool) {
	ps := uintptr(pageSize())
	// Every byte of maxReach matters here — unlike amd64's +-2GB, there's no
	// room to spare skipping past a large gap on principle before the search
	// even starts.
	const gap = 1 << 20
	addr := (near + gap) &^ (ps - 1)
	limit := near + maxReach
	for addr < limit {
		regionStart, regionSize, kr := rtvmregion(addr)
		if kr != 0 {
			// No mapped region at or after addr: everything from here to
			// the top of the address space is free.
			if limit-addr >= size {
				if p, errno := rtmmapfixed(addr, size, protR); errno == 0 {
					return p, true
				}
			}
			return 0, false
		}
		if regionStart > addr && regionStart-addr >= size {
			if p, errno := rtmmapfixed(addr, size, protR); errno == 0 {
				return p, true
			}
			// Something raced us for this hole; resume just past it rather
			// than spinning on the same address.
			addr += ps
			continue
		}
		if regionSize == 0 {
			addr += ps // a degenerate region report; don't get stuck
		} else {
			addr = regionStart + regionSize
		}
	}
	return 0, false
}

// commit backs a slice of the reserved region with memory.
func commit(addr, size uintptr) error {
	return protect(addr, size, protRW)
}

func protect(addr, size uintptr, prot int) error {
	ps := uintptr(pageSize())
	end := (addr + size + ps - 1) &^ (ps - 1)
	addr &^= ps - 1
	if errno := rtmprotect(addr, end-addr, int32(prot)); errno != 0 {
		return syscall.Errno(errno)
	}
	return nil
}
