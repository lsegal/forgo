// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Gain is a pluginval test plugin: a c-shared library whose effect scales
// the signal by its parameter.
package main

import "C"

import (
	"unsafe"

	"forgo.dev/pluginval/internal/goplugin"
)

func gain(x, param float32) float32 { return x * param }

//export ForgoPluginNew
func ForgoPluginNew() C.int { return C.int(goplugin.New(gain)) }

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
