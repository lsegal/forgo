// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 && darwin

#include "textflag.h"

// The darwin/amd64 halves of fgohotDlopen and fgohotDlsym in
// forgo_hot_darwin.go; called through libcCall with DI pointing at their
// argument structs.

TEXT runtime·fgohot_dlopen_trampoline(SB),NOSPLIT,$0
	MOVQ	DI, BX
	MOVQ	0(BX), DI		// arg 1 name
	MOVL	8(BX), SI		// arg 2 flags
	CALL	libc_dlopen(SB)
	MOVQ	AX, 16(BX)
	RET

TEXT runtime·fgohot_dlsym_trampoline(SB),NOSPLIT,$0
	MOVQ	DI, BX
	MOVQ	0(BX), DI		// arg 1 handle
	MOVQ	8(BX), SI		// arg 2 name
	CALL	libc_dlsym(SB)
	MOVQ	AX, 16(BX)
	RET
