// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

import (
	"unicode/utf16"
	"unsafe"
)

// The VST3 ABI as Go types. The layouts follow Steinberg's C API
// (github.com/steinbergmedia/vst3_c_api), which is only read as a
// reference: nothing from it is included. VST3 uses each platform's
// natural alignment, which Go's struct layout matches on the 64-bit
// targets this package supports. layout_test.go checks the sizes.

// A TUID is a 16-byte interface or class ID.
type TUID [16]byte

// UID builds a TUID from the four 32-bit words that the SDK's
// INLINE_UID macro takes. On Windows, where VST3 is COM compatible, the
// first eight bytes are in GUID order; elsewhere every word is big
// endian.
func UID(l1, l2, l3, l4 uint32) TUID {
	var u TUID
	if comCompatible {
		u[0], u[1], u[2], u[3] = byte(l1), byte(l1>>8), byte(l1>>16), byte(l1>>24)
		u[4], u[5], u[6], u[7] = byte(l2>>16), byte(l2>>24), byte(l2), byte(l2>>8)
	} else {
		put32(u[0:], l1)
		put32(u[4:], l2)
	}
	put32(u[8:], l3)
	put32(u[12:], l4)
	return u
}

func put32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// Interface IDs.
var (
	iidFUnknown          = UID(0x00000000, 0x00000000, 0xC0000000, 0x00000046)
	iidIPluginBase       = UID(0x22888DDB, 0x156E45AE, 0x8358B348, 0x08190625)
	iidIPluginFactory    = UID(0x7A4D811C, 0x52114A1F, 0xAED9D2EE, 0x0B43BF9F)
	iidIPluginFactory2   = UID(0x0007B650, 0xF24B4C0B, 0xA464EDB9, 0xF00B2ABB)
	iidIComponent        = UID(0xE831FF31, 0xF2D54301, 0x928EBBEE, 0x25697802)
	iidIAudioProcessor   = UID(0x42043F99, 0xB7DA453C, 0xA569E79D, 0x9AAEC33D)
	iidIEditController   = UID(0xDCD7BBE3, 0x7742448D, 0xA874AACC, 0x979C759E)
	iidIParameterChanges = UID(0xA4779663, 0x0BB64A56, 0xB44384A8, 0x466FEB9D)
)

// A Result is a VST3 tresult. Its values are in result_*.go: Windows
// uses COM's HRESULTs.
type Result int32

// Categories and flags.
const (
	audioEffectClass = "Audio Module Class"
	manyInstances    = 0x7FFFFFFF
	factoryUnicode   = 1 << 4

	mediaAudio = 0
	mediaEvent = 1

	dirInput  = 0
	dirOutput = 1

	busMain          = 0
	busDefaultActive = 1 << 0

	paramCanAutomate = 1 << 0

	sample32 = 0
)

// Speaker arrangements.
const (
	SpeakerMono   uint64 = 1 << 19
	SpeakerStereo uint64 = 1<<0 | 1<<1
)

// Channels reports how many channels a speaker arrangement has.
func Channels(arr uint64) int {
	n := 0
	for ; arr != 0; arr &= arr - 1 {
		n++
	}
	return n
}

// String128 is a NUL-terminated UTF-16 string of at most 128 units.
type String128 [128]uint16

func setString128(s *String128, v string) {
	*s = String128{}
	copy(s[:len(s)-1], utf16.Encode([]rune(v)))
}

func (s *String128) String() string {
	n := 0
	for n < len(s) && s[n] != 0 {
		n++
	}
	return string(utf16.Decode(s[:n]))
}

func setChars(dst []byte, v string) {
	clear(dst)
	copy(dst[:len(dst)-1], v)
}

func chars(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// FactoryInfo is PFactoryInfo.
type FactoryInfo struct {
	Vendor [64]byte
	URL    [256]byte
	Email  [128]byte
	Flags  int32
}

// ClassInfo is PClassInfo.
type ClassInfo struct {
	CID         TUID
	Cardinality int32
	Category    [32]byte
	Name        [64]byte
}

// ClassInfo2 is PClassInfo2.
type ClassInfo2 struct {
	CID           TUID
	Cardinality   int32
	Category      [32]byte
	Name          [64]byte
	ClassFlags    uint32
	SubCategories [128]byte
	Vendor        [64]byte
	Version       [64]byte
	SDKVersion    [64]byte
}

// NameString returns the class name.
func (c *ClassInfo2) NameString() string { return chars(c.Name[:]) }

// SetName replaces the class name.
func (c *ClassInfo2) SetName(name string) { setChars(c.Name[:], name) }

// classInfo is the PClassInfo prefix of c.
func (c *ClassInfo2) classInfo() ClassInfo {
	return ClassInfo{CID: c.CID, Cardinality: c.Cardinality, Category: c.Category, Name: c.Name}
}

type busInfo struct {
	MediaType    int32
	Direction    int32
	ChannelCount int32
	Name         String128
	BusType      int32
	Flags        uint32
}

type processSetup struct {
	ProcessMode        int32
	SymbolicSampleSize int32
	MaxSamplesPerBlock int32
	SampleRate         float64
}

type audioBusBuffers struct {
	NumChannels  int32
	SilenceFlags uint64
	Buffers      **float32 // channelBuffers32; the union's 64-bit arm is unused
}

type processData struct {
	ProcessMode        int32
	SymbolicSampleSize int32
	NumSamples         int32
	NumInputs          int32
	NumOutputs         int32
	Inputs             *audioBusBuffers
	Outputs            *audioBusBuffers
	InputParamChanges  unsafe.Pointer // IParameterChanges*
	OutputParamChanges unsafe.Pointer
	InputEvents        unsafe.Pointer
	OutputEvents       unsafe.Pointer
	ProcessContext     unsafe.Pointer
}

type parameterInfo struct {
	ID           uint32
	Title        String128
	ShortTitle   String128
	Units        String128
	StepCount    int32
	DefaultValue float64
	UnitID       int32
	Flags        int32
}
