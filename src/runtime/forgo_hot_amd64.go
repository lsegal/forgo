// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// fgohotPatchLen is the number of bytes fgohotPatch overwrites at the entry
// point of a function it redirects.
const fgohotPatchLen = 5 // JMP rel32

// fgohotWriteJump overwrites the first fgohotPatchLen bytes at old with an
// unconditional jump to new. The caller must have stopped the world and made
// the page writable.
//
//go:nosplit
func fgohotWriteJump(old, new uintptr) string {
	if old == 0 || new == 0 {
		return "hot reload: nil patch target"
	}
	delta := int64(new) - int64(old+fgohotPatchLen)
	if delta < -0x7fffffff || delta > 0x7fffffff {
		// The agent reserves the image region within reach of the program's
		// text precisely so this cannot happen; if it does, refuse rather
		// than write a longer sequence over a function that may be shorter.
		return "hot reload: replacement code is out of jump range"
	}
	p := (*[fgohotPatchLen]byte)(unsafe.Pointer(old))
	p[0] = 0xe9 // JMP rel32
	p[1] = byte(delta)
	p[2] = byte(delta >> 8)
	p[3] = byte(delta >> 16)
	p[4] = byte(delta >> 24)
	return ""
}
