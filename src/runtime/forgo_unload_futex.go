// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dragonfly || freebsd || linux

package runtime

import "internal/runtime/atomic"

// forgoNoteWake is notewakeup that may be called on a note that has
// already been woken. Only the unload uses it.
func forgoNoteWake(n *note) {
	if atomic.Xchg(key32(&n.key), 1) == 0 {
		futexwakeup(key32(&n.key), 1)
	}
}
