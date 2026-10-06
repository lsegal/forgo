// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Drive is a pluginval test plugin: a c-shared library whose effect is a
// tanh soft clipper, with its parameter setting the drive from 1x to 10x.
package main

import "C"

import (
	"math"
	"unsafe"

	"forgo.dev/pluginval/internal/goplugin"
)

func drive(x, param float32) float32 {
	k := 1 + 9*float64(param)
	return float32(math.Tanh(k*float64(x)) / math.Tanh(k))
}

//export ForgoPluginNew
func ForgoPluginNew() C.int { return C.int(goplugin.New(drive)) }

//export ForgoPluginFree
func ForgoPluginFree(id C.int) { goplugin.Free(int32(id)) }

//export ForgoPluginProcess
func ForgoPluginProcess(id C.int, in, out unsafe.Pointer, nch, n C.int, param C.float) C.int {
	if !goplugin.Process(int32(id), in, out, int32(nch), int32(n), float32(param)) {
		return 0
	}
	return 1
}

func main() {}
