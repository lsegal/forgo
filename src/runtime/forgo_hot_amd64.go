// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// fgohotPatchLen is the number of bytes fgohotPatch overwrites at the entry
// point of a function it redirects:
//
//	JMP QWORD PTR [RIP+0]
//	<8 byte target>
//
// Unlike JMP rel32, this sequence reaches the entire 64-bit address space.
// It also leaves every register untouched, which matters at a Go ABI entry:
// MOVABS target, RAX; JMP RAX would be two bytes shorter, but RAX may contain
// an argument. forgo run --watch links the host with -funcalign=16, so these
// 14 bytes never overlap the following function even when the original body
// is shorter than the patch.
const fgohotPatchLen = 14

// fgohotWriteJump overwrites the first fgohotPatchLen bytes at old with an
// unconditional jump to new. The caller must have stopped the world and made
// the page writable.
//
//go:nosplit
func fgohotWriteJump(old, new uintptr) string {
	if old == 0 || new == 0 {
		return "hot reload: nil patch target"
	}
	p := (*[fgohotPatchLen]byte)(unsafe.Pointer(old))
	p[0] = 0xff // JMP QWORD PTR [RIP+disp32]
	p[1] = 0x25
	p[2] = 0 // disp32 = 0: the pointer immediately follows the instruction
	p[3] = 0
	p[4] = 0
	p[5] = 0
	*(*uintptr)(unsafe.Pointer(old + 6)) = new
	return ""
}
