// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package syscall

// forgoRuntimeIsLibrary reports whether this program is a c-shared or
// c-archive library. A process can load several of those, each with its own
// Go runtime, and the open-file limit belongs to the host process, so a
// library does not raise it. If it did, every runtime loaded after the
// first would take the raised limit for the host's original one and pass it
// on to the processes it starts.
func forgoRuntimeIsLibrary() bool // in package runtime
