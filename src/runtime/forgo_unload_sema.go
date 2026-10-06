// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || netbsd || openbsd || plan9 || solaris || windows

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// forgoNoteWake is notewakeup that may be called on a note that has
// already been woken. Only the unload uses it.
func forgoNoteWake(n *note) {
	for {
		v := atomic.Loaduintptr(&n.key)
		if v == locked {
			return
		}
		if atomic.Casuintptr(&n.key, v, locked) {
			if v != 0 {
				semawakeup((*m)(unsafe.Pointer(v)))
			}
			return
		}
	}
}
