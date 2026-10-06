// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux || darwin || freebsd

package runtime

// forgoSigMagic in si_errno marks a Notify signal that one forgo runtime
// handed down to another (see forgoSigNotifyFwd). The kernel leaves si_errno
// zero for the signals Notify can see.
const forgoSigMagic = 0x66676f21 // "fgo!"

//go:nosplit
func forgoSigMarked(info *siginfo) bool { return info.si_errno == forgoSigMagic }

//go:nosplit
func forgoSigMark(info *siginfo) { info.si_errno = forgoSigMagic }
