// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Drive is a pluginval test plugin: a VST3 module, built as a c-shared
// library, whose effect is a tanh soft clipper, with its parameter setting
// the drive from 1x to 10x.
package main

import "C"

import (
	"math"

	"forgo.dev/pluginval/internal/goplugin"
	"forgo.dev/pluginval/internal/vst3"
)

func drive(x, param float32) float32 {
	k := 1 + 9*float64(param)
	return float32(math.Tanh(k*float64(x)) / math.Tanh(k))
}

func init() {
	vst3.SetFactory(vst3.NewFactory("forgo", "https://github.com/lsegal/forgo", &vst3.Class{
		CID:          vst3.UID(0x6F72676F, 0x47617061, 0x696E0001, 0x00000002),
		Name:         "Forgo Go Drive",
		Param:        "Drive",
		DefaultParam: 0.5,
		New:          func() vst3.Processor { return goplugin.New(drive) },
	}))
}

func main() {}
