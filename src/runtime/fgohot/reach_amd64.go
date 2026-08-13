// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64

package fgohot

// A JMP rel32 reaches +-2GB, which is what patching a function's entry point
// needs and what bounds how far from the program's own text the reserved
// region for new code may be.
const (
	maxReach  = 1 << 31
	probeStep = 256 << 20
)
