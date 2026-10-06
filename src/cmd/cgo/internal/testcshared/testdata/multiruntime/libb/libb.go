// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

/*
typedef int (*bounce_fn)(int);

static int call_peer(void *fn, int n) { return ((bounce_fn)fn)(n); }

#ifdef _WIN32
#include <windows.h>
static void block_in_c(void) { for (;;) Sleep(1000); }
#else
#include <unistd.h>
static void block_in_c(void) { for (;;) sleep(1000); }
#endif
*/
import "C"

import (
	"time"
	"unsafe"

	"testcshared/multiruntime/mr"
)

// peer is the Bounce function of the other library, a C function pointer.
var peer unsafe.Pointer

//export Work
func Work(n C.int) C.int { return C.int(mr.Work(int(n), 1)) }

//export SetPeer
func SetPeer(fn unsafe.Pointer) { peer = fn }

// Bounce recovers a panic in this runtime and then calls the peer library's
// Bounce through C, so a call of depth n alternates between the two
// runtimes on one OS thread. It returns the number of recovered panics.
//
//export Bounce
func Bounce(n C.int) C.int {
	r := C.int(mr.RecoverPanic())
	if n > 0 && peer != nil {
		r += C.call_peer(peer, n-1)
	}
	return r
}

//export Fault
func Fault() C.int { return C.int(mr.RecoverNilDeref()) }

//export StartSpin
func StartSpin() { mr.StartSpin() }

//export CollectGarbage
func CollectGarbage() C.int { return C.int(mr.GC()) }

//export NewInstance
func NewInstance() C.int { return C.int(mr.NewInstance()) }

//export Process
func Process(id, frames C.int) C.int { return C.int(mr.Process(int32(id), int(frames))) }

//export DestroyInstance
func DestroyInstance(id C.int) C.int {
	if mr.DestroyInstance(int32(id)) {
		return 1
	}
	return 0
}

//export LiveInstances
func LiveInstances() C.int { return C.int(mr.LiveInstances()) }

//export Generation
func Generation() C.int { return C.int(mr.Generation()) }

//export StartBackground
func StartBackground() { mr.StartBackground() }

// BlockInC starts a goroutine that stays in a C call forever, which makes
// unloading the library impossible.
//
//export BlockInC
func BlockInC() {
	go C.block_in_c()
	time.Sleep(10 * time.Millisecond)
}

func main() {}
