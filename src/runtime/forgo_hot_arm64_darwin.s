// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64 && darwin

#include "textflag.h"

// mach_vm_remap_trampoline atomically overlays a prepared private mapping on
// an existing text page. Arguments 9-11 use the arm64 C ABI's stack slots.
TEXT runtime·mach_vm_remap_trampoline(SB),NOSPLIT,$0
	MOVD	R0, R19
	SUB	$32, RSP
	MOVD	24(R19), R8
	MOVD	R8, 0(RSP)	// current_protection
	MOVD	32(R19), R8
	MOVD	R8, 8(RSP)	// max_protection
	MOVD	$2, R8
	MOVD	R8, 16(RSP)	// VM_INHERIT_NONE
	MOVD	0(R19), R1	// target_address
	MOVD	8(R19), R2	// size
	MOVD	$0, R3		// mask
	MOVD	$0x4000, R4	// VM_FLAGS_FIXED | VM_FLAGS_OVERWRITE
	MOVD	16(R19), R6	// source_address
	MOVD	$1, R7		// copy
	MOVD	$libc_mach_task_self_(SB), R0
	MOVW	0(R0), R0
	MOVW	R0, R5		// source task is also this task
	BL	libc_mach_vm_remap(SB)
	ADD	$32, RSP
	RET

TEXT runtime·fgohot_dlopen_trampoline(SB),NOSPLIT,$0
	MOVD	R0, R19
	MOVW	8(R0), R1
	MOVD	0(R0), R0
	BL	libc_dlopen(SB)
	MOVD	R0, 16(R19)
	RET

TEXT runtime·fgohot_dlsym_trampoline(SB),NOSPLIT,$0
	MOVD	R0, R19
	MOVD	8(R0), R1
	MOVD	0(R0), R0
	BL	libc_dlsym(SB)
	MOVD	R0, 16(R19)
	RET
