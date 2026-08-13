// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Runtime support for forgo hot reload (`forgo run --watch`).
//
// A hot reload works like Dart's: the program keeps running, keeps its heap,
// its goroutines and every package-level variable, and only the *code* of the
// functions that changed is replaced.
//
// The forgo hot linker (cmd/link -buildmode=fgohot) compiles the changed
// packages into a small, fully relocated image whose references to everything
// that did *not* change — the runtime, unchanged packages, and, crucially,
// every package-level variable — point straight at the addresses those symbols
// already occupy in the running process. So the new code reads and writes the
// same globals the old code did; no state is copied, migrated or lost.
//
// This file provides the three things that image needs from the runtime and
// that cannot be done from outside it:
//
//   - fgohotAddModule: splice the image's moduledata into the module list so
//     the garbage collector, the stack unwinder, profiles and panics all
//     understand the new code.
//   - fgohotInitTasks / fgohotDoInit: run the init tasks of packages that are
//     new in this image (existing packages are never re-initialized — that is
//     what preserves their state).
//   - fgohotPatch: redirect the entry point of every changed function at the
//     old address to its new body, with the world stopped.
//
// The exported names live in package runtime/fgohot, which is the in-process
// agent the watcher talks to.

package runtime

import (
	"internal/abi"
	"unsafe"
)

// fgohotSupported reports whether hot reload can work on this platform.
//
// The entry-point patching below needs to write an architecture specific
// jump over the first instructions of a live function, and, on arm64, flush
// the instruction cache afterward. amd64 is implemented on every OS forgo
// hot reload otherwise supports; arm64 is implemented only for darwin
// (Apple Silicon), whose I-cache maintenance goes through a libSystem call
// this file only wires up there. Other combinations report a clean error
// rather than corrupting the text segment.
//
//go:linkname fgohotSupported runtime/fgohot.supported
func fgohotSupported() bool {
	return GOARCH == "amd64" || (GOARCH == "arm64" && GOOS == "darwin")
}

// fgohotTextRange reports the text segment of the running program, so the
// agent can make the pages it is about to patch temporarily writable.
//
//go:linkname fgohotTextRange runtime/fgohot.textRange
func fgohotTextRange() (start, end uintptr) {
	return firstmoduledata.text, firstmoduledata.etext
}

// fgohotAddModule links the moduledata at mdaddr, which the hot linker wrote
// into an image the agent has already mapped, into the running program's
// module list. It returns "" on success or a description of what went wrong.
//
// After this returns the runtime can find functions, unwind stacks, scan
// stacks and globals, and resolve types in the new image.
//
//go:linkname fgohotAddModule runtime/fgohot.addModule
func fgohotAddModule(mdaddr uintptr) string {
	if mdaddr == 0 {
		return "hot reload: nil moduledata"
	}
	md := (*moduledata)(unsafe.Pointer(mdaddr))
	if md.pcHeader == nil {
		return "hot reload: image has no pclntab"
	}
	if md.pcHeader.magic != abi.CurrentPCLnTabMagic {
		return "hot reload: image pclntab has the wrong magic; toolchain mismatch"
	}
	if md.next != nil {
		return "hot reload: image moduledata is already linked"
	}

	for _, pmd := range activeModules() {
		if pmd == md {
			return "hot reload: image is already loaded"
		}
		if inRange(pmd.text, pmd.etext, md.text, md.etext) ||
			inRange(pmd.data, pmd.edata, md.data, md.edata) ||
			inRange(pmd.bss, pmd.ebss, md.bss, md.ebss) {
			return "hot reload: image overlaps an already loaded module"
		}
	}

	// Publish the module. modulesinit builds the GC's globals bitmaps for it
	// and swaps in a new active-module slice atomically, so a concurrent GC
	// either sees the module completely or not at all.
	lastmoduledatap.next = md
	lastmoduledatap = md
	modulesinit()
	typelinksinit()
	moduledataverify1(md)

	lock(&itabLock)
	for _, i := range md.itablinks {
		itabAdd(i)
	}
	unlock(&itabLock)

	return ""
}

// fgohotInitTasks returns the init tasks recorded in the image's moduledata.
// The hot linker only records tasks for packages that are new in this image;
// packages that already exist in the process keep the state their init built.
//
//go:linkname fgohotInitTasks runtime/fgohot.moduleInitTasks
func fgohotInitTasks(mdaddr uintptr) []*initTask {
	return (*moduledata)(unsafe.Pointer(mdaddr)).inittasks
}

// fgohotDoInit runs init tasks returned by fgohotInitTasks.
//
//go:linkname fgohotDoInit runtime/fgohot.doInitTasks
func fgohotDoInit(tasks []*initTask) {
	doInit(tasks)
}

// fgohotWorldStop holds the token between fgohotBeginPatch and
// fgohotEndPatch. A plain package variable is safe here because the agent
// only ever has one patch in flight at a time — its own serve loop is the
// sole caller, never concurrent with itself.
var fgohotWorldStop worldStop

// fgohotBeginPatch stops the world. The caller must call fgohotEndPatch
// exactly once after, whether or not fgohotPatch itself is ultimately
// called.
//
// This is a separate step from fgohotPatch, rather than being inside it as
// it once was, because the agent needs the world stopped for more than just
// the writes fgohotPatch makes: on a platform that cannot mark a page both
// writable and executable at once (observed on darwin/arm64; see
// runtime/fgohot's fgohot.go), it briefly has to make the page
// writable-only around those writes, and if the world were still running
// at that moment, another goroutine calling straight into the function
// being patched would fault on the vanished exec permission. Stopping the
// world before that protection change, not just before the writes,
// closes that window.
//
//go:linkname fgohotBeginPatch runtime/fgohot.beginPatch
func fgohotBeginPatch() {
	fgohotWorldStop = stopTheWorld(stwUnknown)
}

// fgohotEndPatch resumes what fgohotBeginPatch stopped.
//
//go:linkname fgohotEndPatch runtime/fgohot.endPatch
func fgohotEndPatch() {
	startTheWorld(fgohotWorldStop)
}

// fgohotPatch redirects functions to new implementations. pairs holds
// (oldEntry, newEntry) tuples: after it returns, every call that targets
// oldEntry executes the body at newEntry instead.
//
// Frames already executing the old body keep running it to completion — the
// jump only affects entries, never a function already in progress. That is
// the same guarantee Dart's hot reload gives: the change takes effect the
// next time the function is called.
//
// The caller must have the world stopped already, via fgohotBeginPatch —
// see there for why that happens outside this function now. This refuses to
// patch if any goroutine is stopped inside the handful of bytes it needs to
// overwrite.
//
//go:linkname fgohotPatch runtime/fgohot.patchFuncs
func fgohotPatch(pairs []uintptr) string {
	if !fgohotSupported() {
		return "hot reload: unsupported architecture " + GOARCH
	}
	if len(pairs)%2 != 0 {
		return "hot reload: malformed patch list"
	}
	if len(pairs) == 0 {
		return ""
	}

	msg := fgohotCheckStacks(pairs)
	if msg == "" {
		for i := 0; i < len(pairs); i += 2 {
			if msg = fgohotWriteJump(pairs[i], pairs[i+1]); msg != "" {
				break
			}
		}
	}
	return msg
}

// fgohotCheckStacks reports whether any goroutine is parked with a program
// counter inside a region fgohotWriteJump is about to overwrite.
func fgohotCheckStacks(pairs []uintptr) string {
	msg := ""
	self := getg()
	systemstack(func() {
		forEachGRace(func(gp *g) {
			if msg != "" || gp == self {
				return
			}
			switch readgstatus(gp) &^ _Gscan {
			case _Gdead, _Gidle:
				return
			}
			var u unwinder
			for u.init(gp, 0); u.valid(); u.next() {
				pc := u.frame.pc
				for i := 0; i < len(pairs); i += 2 {
					if pc >= pairs[i] && pc < pairs[i]+fgohotPatchLen {
						msg = "hot reload: a goroutine is executing the entry of a function being replaced; retry"
						return
					}
				}
			}
		})
	})
	return msg
}

// fgohotWriteJump overwrites the first fgohotPatchLen bytes at old with an
// unconditional jump to new. The caller must have stopped the world and made
// the page writable. Implemented per architecture — see forgo_hot_amd64.go
// and forgo_hot_arm64.go.
