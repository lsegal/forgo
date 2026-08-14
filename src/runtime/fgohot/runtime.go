// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fgohot

import "unsafe"

// The functions below are implemented in package runtime, in
// src/runtime/forgo_hot.go, and made available here with //go:linkname.

// supported reports whether the runtime can patch function entry points on
// this architecture.
func supported() bool

// beginPatch stops the world; endPatch resumes it. apply brackets a whole
// patch application — including the protection changes around the writes,
// not just the writes themselves — between the two. See forgo_hot.go's
// fgohotBeginPatch for why the writes alone aren't enough.
func beginPatch()
func endPatch()

// textRange returns the bounds of the running program's own code.
func textRange() (start, end uintptr)

// addModule registers a mapped image's moduledata with the runtime, making
// its code visible to the garbage collector, the unwinder and the profiler.
// It returns "" on success.
func addModule(moduledata uintptr) string

// moduleInitTasks returns the init tasks the hot linker recorded for packages
// that are new in this image.
func moduleInitTasks(moduledata uintptr) []*initTask

// doInitTasks runs init tasks returned by moduleInitTasks.
func doInitTasks(tasks []*initTask)

// patchFuncs redirects function entry points. pairs holds (old, new) tuples.
// The caller must have stopped the world; it returns "" on success.
func patchFuncs(pairs []uintptr) string

// checkPatchFuncs refuses a patch when a stopped goroutine's PC is in bytes
// the patch will replace. Darwin/arm64 prepares jumps in a private page, so
// it must perform this check against the original addresses separately.
func checkPatchFuncs(pairs []uintptr) string

// flushICache publishes newly mapped instructions on architectures whose
// instruction cache is not coherent with data writes.
func flushICache(addr, size uintptr)

// initTask mirrors runtime.initTask. Only pointers to it cross the boundary,
// so the contents do not matter here.
type initTask struct{}

// copyIn writes image into the freshly committed pages at addr.
func copyIn(addr uintptr, image []byte) {
	dst := unsafe.Slice((*byte)(unsafe.Pointer(addr)), len(image))
	copy(dst, image)
}
