// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !amd64 && !arm64

package fgohot

// Hot reload is not implemented for this architecture
// (runtime.fgohotSupported reports false, so the agent never reserves
// anything), but mem_linux.go and mem_windows.go still have to build here.
// These mirror amd64's conservative +-2GB.
const (
	maxReach  = 1 << 31
	probeStep = 256 << 20
)
