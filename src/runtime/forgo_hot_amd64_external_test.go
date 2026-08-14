// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"testing"
)

func TestFgohotWriteJumpAMD64(t *testing.T) {
	runtime.XTestFgohotWriteJumpAMD64(t)
}

func TestFgohotWriteJumpAMD64RejectsNil(t *testing.T) {
	runtime.XTestFgohotWriteJumpAMD64RejectsNil(t)
}
