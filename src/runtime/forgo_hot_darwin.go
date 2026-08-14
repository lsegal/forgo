// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The darwin half of the runtime support forgo hot reload needs.
//
// Unlike Linux, where a raw SYS_MMAP/SYS_MPROTECT/SYS_MADVISE-family syscall
// works from any package, macOS requires going through libSystem — and the
// trampolines that do that live in package runtime, unexported. This file
// republishes what runtime/fgohot needs from them (mapping memory at an
// exact address, changing its protection, and querying the process's own
// memory map to find a free address range in the first place) with
// uintptr-only signatures, so package fgohot never has to reason about
// unsafe.Pointer across the linkname boundary.

//go:build darwin

package runtime

import (
	"internal/abi"
	"unsafe"
)

//go:cgo_import_dynamic libc_mach_vm_remap mach_vm_remap "/usr/lib/libSystem.B.dylib"

// fgohotMprotect changes the protection of the size bytes at addr. It
// returns 0 on success or the errno on failure.
//
//go:linkname fgohotMprotect runtime/fgohot.rtmprotect
func fgohotMprotect(addr, size uintptr, prot int32) int32 {
	return mprotect(unsafe.Pointer(addr), size, prot)
}

// fgohotMmapFixed maps size bytes at exactly addr, refusing rather than
// silently placing the mapping elsewhere (mmap's ordinary behavior when a
// hint address isn't free) or silently displacing whatever was already
// there (MAP_FIXED's ordinary behavior, and not survivable if that
// happened to be, say, a live heap arena). The caller is expected to have
// confirmed addr..addr+size is unoccupied first, with fgohotVMRegion.
//
//go:linkname fgohotMmapFixed runtime/fgohot.rtmmapfixed
func fgohotMmapFixed(addr, size uintptr, prot int32) (uintptr, int32) {
	p, err := mmap(unsafe.Pointer(addr), size, prot, _MAP_ANON|_MAP_PRIVATE|_MAP_FIXED, -1, 0)
	if err != 0 {
		return 0, int32(err)
	}
	if uintptr(p) != addr {
		// Should be unreachable with MAP_FIXED, which either lands exactly
		// at addr or fails outright — but never trust a syscall boundary
		// blindly when the fallback (unmap and report as if it failed) is
		// this cheap.
		munmap(p, size)
		return 0, 1
	}
	return uintptr(p), 0
}

//go:linkname fgohotMmap runtime/fgohot.rtmmap
func fgohotMmap(size uintptr, prot int32) (uintptr, int32) {
	p, err := mmap(nil, size, prot, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	return uintptr(p), int32(err)
}

//go:linkname fgohotMunmap runtime/fgohot.rtmunmap
func fgohotMunmap(addr, size uintptr) {
	munmap(unsafe.Pointer(addr), size)
}

// fgohotRemap atomically replaces target with a private copy of the mapping
// at source. Both addresses and size must be page aligned.
//
//go:linkname fgohotRemap runtime/fgohot.rtremap
func fgohotRemap(target, source, size uintptr) int32 {
	targetAddress := uint64(target)
	var current, maximum int32
	args := struct {
		target  *uint64
		size    uintptr
		source  uintptr
		current *int32
		maximum *int32
	}{&targetAddress, size, source, &current, &maximum}
	return int32(libcCall(unsafe.Pointer(abi.FuncPCABI0(mach_vm_remap_trampoline)), unsafe.Pointer(&args)))
}

func mach_vm_remap_trampoline()

// fgohotVMRegion reports the mapped region at or after addr — the same
// query vmmap itself uses, and the only way on darwin to tell whether a
// range of address space is actually free before committing to it with
// fgohotMmapFixed. On success it returns the region's own start and size,
// which may be at or after addr (never before): a hole is anything between
// addr and that start. A nonzero kernel return code (typically
// KERN_INVALID_ADDRESS, 1) means there is no mapped region left between
// addr and the top of the address space — everything from addr up is free.
//
//go:linkname fgohotVMRegion runtime/fgohot.rtvmregion
func fgohotVMRegion(addr uintptr) (regionStart, regionSize uintptr, kr int32) {
	address := uint64(addr)
	var size uint64
	// Sized for vm_region_basic_info_data_64_t (36 bytes; see
	// runtime/pprof's defs_darwin_{amd64,arm64}.go, generated from the same
	// mach_vm_region call) with room to spare — its contents don't matter
	// here, only that the kernel has somewhere to write them.
	var info [40]byte
	kr = mach_vm_region(&address, &size, unsafe.Pointer(&info))
	return uintptr(address), uintptr(size), kr
}
