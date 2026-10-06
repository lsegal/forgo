// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !amd64 && !(arm64 && darwin)

package runtime

// fgohotPatchLen is the number of bytes fgohotPatch would overwrite at the
// entry point of a function it redirects. Hot reload has no entry-point
// patch for this platform yet, so nothing is ever overwritten.
const fgohotPatchLen = 0

// fgohotWriteJump refuses: fgohotSupported reports false here, so
// fgohotPatch never reaches this, but the toolchain must still build for
// platforms hot reload does not support.
//
//go:nosplit
func fgohotWriteJump(old, new uintptr) string {
	return "hot reload: unsupported platform " + GOOS + "/" + GOARCH
}
