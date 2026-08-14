// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !darwin || !arm64

package fgohot

func resolveMachOImports(image []byte) string { return "" }
