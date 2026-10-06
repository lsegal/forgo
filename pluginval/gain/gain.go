// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Gain is a pluginval test plugin: a VST3 module, built as a c-shared
// library, whose effect scales the signal by its parameter.
package main

import "C"

import (
	"forgo.dev/pluginval/internal/goplugin"
	"forgo.dev/pluginval/internal/vst3"
)

func gain(x, param float32) float32 { return x * param }

func init() {
	vst3.SetFactory(vst3.NewFactory("forgo", "https://github.com/lsegal/forgo", &vst3.Class{
		CID:          vst3.UID(0x6F72676F, 0x47617061, 0x696E0001, 0x00000001),
		Name:         "Forgo Go Gain",
		Param:        "Gain",
		DefaultParam: 0.5,
		New:          func() vst3.Processor { return goplugin.New(gain) },
	}))
}

func main() {}
