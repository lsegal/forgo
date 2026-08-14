// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64

package fgohot

// The function-entry patch itself has full 64-bit reach (see
// runtime/forgo_hot_amd64.go). The reload image still has to remain within
// +-2GB of the running program: ordinary compiler-generated calls and
// references to pinned package data use signed 32-bit PC-relative operands.
// A call can grow a linker trampoline, but an arbitrary data instruction
// cannot, so the address-space reservation must respect the tighter limit.
const (
	maxReach  = 1 << 31
	probeStep = 256 << 20
)
