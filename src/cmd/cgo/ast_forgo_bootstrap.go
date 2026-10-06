// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build compiler_bootstrap

package main

// walkForgo reports false: the bootstrap toolchain's go/ast has no forgo
// node types, and the bootstrap cgo never sees forgo source.
func (f *File) walkForgo(x any, visit func(*File, any, astContext)) bool {
	return false
}
