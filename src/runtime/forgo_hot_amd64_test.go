// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

func XTestFgohotWriteJumpAMD64(t TestingT) {
	var code [fgohotPatchLen]byte
	const target = uintptr(0x1122334455667788)
	if msg := fgohotWriteJump(uintptr(unsafe.Pointer(&code[0])), target); msg != "" {
		t.Fatal(msg)
	}
	want := [fgohotPatchLen]byte{
		0xff, 0x25, 0, 0, 0, 0,
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11,
	}
	if code != want {
		t.Fatalf("jump encoding = %x, want %x", code, want)
	}
}

func XTestFgohotWriteJumpAMD64RejectsNil(t TestingT) {
	var code [fgohotPatchLen]byte
	if msg := fgohotWriteJump(uintptr(unsafe.Pointer(&code[0])), 0); msg == "" {
		t.Fatal("nil target was accepted")
	}
}
