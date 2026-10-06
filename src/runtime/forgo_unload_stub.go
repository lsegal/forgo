// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux && !darwin && !windows

// Unloading is not supported here; forgoUnloadable is always false.

package runtime

import "unsafe"

func forgoRestoreHandlers()    {}
func forgoClosePoller()        {}
func forgoJoinThreads()        {}
func forgoWakeSignalReceiver() {}
func forgoThreadExitOS(mp *m)  {}
func forgoM0Exit()             {}
func forgoTLSKey() uintptr     { return 0 }

//go:nosplit
//go:nowritebarrierrec
func forgoSaveSig(sig uint32) {}

//go:nosplit
func forgoMapRaw(n uintptr) unsafe.Pointer { return nil }

//go:nosplit
func forgoUnmapRaw(p unsafe.Pointer, n uintptr) {}

//go:nosplit
func forgoThreadExitFinal() {}

//go:nosplit
func forgoLibInit() {}

func forgoReleaseSignalStack(mp *m) {}
