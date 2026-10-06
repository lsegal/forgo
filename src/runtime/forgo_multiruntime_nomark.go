// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix && !linux && !darwin && !freebsd

package runtime

// These platforms do not hand Notify signals down to other runtimes: runtime/cgo
// never fills in _cgo_forgo_isgosighandler, so no signal is ever marked.

//go:nosplit
func forgoSigMarked(info *siginfo) bool { return false }

//go:nosplit
func forgoSigMark(info *siginfo) {}
