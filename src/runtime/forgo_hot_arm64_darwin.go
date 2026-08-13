// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64 && darwin

package runtime

import (
	"internal/abi"
	"unsafe"
)

// fgohotPatchLen is the number of bytes fgohotPatch overwrites at the entry
// point of a function it redirects: LDR X16, #8 ; BR X16 ; <8 byte target>.
//
// A direct B only reaches +-128MB (a signed 26 bit word offset) — nowhere
// near enough here. Go's own heap arena reservations (sysReserve's giant
// PROT_NONE placeholders, visible as VM_ALLOCATE regions in vmmap) routinely
// blanket the address space for hundreds of MB around a small program's
// text on darwin/arm64, so a spot for the reserved region often does not
// exist within even a few hundred MB, let alone 128MB. Loading the target
// through a register instead reaches the entire address space, so where the
// agent's reserved region ends up placed no longer matters for reachability
// (see runtime/fgohot's reach_arm64.go).
const fgohotPatchLen = 16

// fgohotWriteJump overwrites the fgohotPatchLen bytes at old with an
// indirect branch to new. The caller must have stopped the world and made
// the page writable.
//
//go:nosplit
func fgohotWriteJump(old, new uintptr) string {
	if old == 0 || new == 0 {
		return "hot reload: nil patch target"
	}
	if old&3 != 0 {
		return "hot reload: misaligned patch target"
	}
	p := (*[2]uint32)(unsafe.Pointer(old))
	p[0] = 0x58000050 // LDR X16, #8 (loads the 8 bytes right after this pair)
	p[1] = 0xd61f0200 // BR X16
	*(*uintptr)(unsafe.Pointer(old + 8)) = new

	// ARM64's instruction cache is not coherent with writes through the data
	// path: without this, a core could keep fetching the old instructions
	// from its I-cache indefinitely. sys_icache_invalidate is the API Apple
	// documents for exactly this — self-modifying/JIT code — rather than
	// raw IC IVAU, which needs privilege Apple's hardened runtime does not
	// grant from EL0 on every SoC. The literal at old+8 is data, not fetched
	// as an instruction, so only the two instruction words need flushing.
	fgohotFlushICache(old, 8)
	return ""
}

//go:nosplit
func fgohotFlushICache(addr, size uintptr) {
	args := struct{ addr, size uintptr }{addr, size}
	libcCall(unsafe.Pointer(abi.FuncPCABI0(sys_icache_invalidate_trampoline)), unsafe.Pointer(&args))
}
func sys_icache_invalidate_trampoline()

//go:cgo_import_dynamic libc_sys_icache_invalidate sys_icache_invalidate "/usr/lib/libSystem.B.dylib"
