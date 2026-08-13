// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows

package fgohot

// resolveImports is a PE-only step: fixing up a manually mapped image's
// Import Address Table, which only Windows needs since it's the only format
// forgo hot reload targets that resolves external references this way.
func resolveImports(base uintptr, image []byte) string { return "" }
