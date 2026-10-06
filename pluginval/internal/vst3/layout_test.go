// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

import (
	"testing"
	"unsafe"
)

// TestLayout checks the ABI structs against the sizes and offsets that
// Steinberg's C API gives them on 64-bit targets.
func TestLayout(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want uintptr
	}{
		{"sizeof(TUID)", unsafe.Sizeof(TUID{}), 16},
		{"sizeof(PFactoryInfo)", unsafe.Sizeof(FactoryInfo{}), 452},
		{"sizeof(PClassInfo)", unsafe.Sizeof(ClassInfo{}), 116},
		{"sizeof(PClassInfo2)", unsafe.Sizeof(ClassInfo2{}), 440},
		{"sizeof(BusInfo)", unsafe.Sizeof(busInfo{}), 276},
		{"sizeof(ProcessSetup)", unsafe.Sizeof(processSetup{}), 24},
		{"ProcessSetup.sampleRate", unsafe.Offsetof(processSetup{}.SampleRate), 16},
		{"sizeof(AudioBusBuffers)", unsafe.Sizeof(audioBusBuffers{}), 24},
		{"AudioBusBuffers.silenceFlags", unsafe.Offsetof(audioBusBuffers{}.SilenceFlags), 8},
		{"AudioBusBuffers.channelBuffers32", unsafe.Offsetof(audioBusBuffers{}.Buffers), 16},
		{"sizeof(ProcessData)", unsafe.Sizeof(processData{}), 80},
		{"ProcessData.inputs", unsafe.Offsetof(processData{}.Inputs), 24},
		{"ProcessData.inputParameterChanges", unsafe.Offsetof(processData{}.InputParamChanges), 40},
		{"sizeof(ParameterInfo)", unsafe.Sizeof(parameterInfo{}), 792},
		{"ParameterInfo.defaultNormalizedValue", unsafe.Offsetof(parameterInfo{}.DefaultValue), 776},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestUID checks IIDs against their byte order in the C API's
// SMTG_INLINE_UID.
func TestUID(t *testing.T) {
	want := TUID{0xE8, 0x31, 0xFF, 0x31, 0xF2, 0xD5, 0x43, 0x01, 0x92, 0x8E, 0xBB, 0xEE, 0x25, 0x69, 0x78, 0x02}
	if comCompatible {
		want = TUID{0x31, 0xFF, 0x31, 0xE8, 0xD5, 0xF2, 0x01, 0x43, 0x92, 0x8E, 0xBB, 0xEE, 0x25, 0x69, 0x78, 0x02}
	}
	if iidIComponent != want {
		t.Errorf("IComponent IID = % x, want % x", iidIComponent, want)
	}
}

func TestString128(t *testing.T) {
	var s String128
	setString128(&s, "Stereo In")
	if got := s.String(); got != "Stereo In" {
		t.Errorf("String128 round trip = %q", got)
	}
}
