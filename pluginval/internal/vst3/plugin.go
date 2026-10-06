// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// A Processor is the audio processing of one plugin instance.
type Processor interface {
	// Process reads in and writes out, which have the same number of
	// channels and samples and may share buffers. param is the plugin's
	// parameter, normalized to [0, 1].
	Process(in, out [][]float32, param float32)
	// Close releases the processor.
	Close()
}

// A Class is an audio effect with one stereo or mono bus in each
// direction and one automatable parameter. Its instances are single
// components: the component is also its own edit controller.
type Class struct {
	CID          TUID
	Name         string
	Param        string  // parameter title
	DefaultParam float64 // normalized default value
	// New creates the processor of an instance. It is called when the
	// host initializes the instance.
	New func() Processor
}

// paramID is the ID of a class's only parameter.
const paramID = 0

// stateSize is the size of an instance's state: its parameter as a
// little-endian float32.
const stateSize = 4

type instance struct {
	class *Class
	param atomic.Uint64 // math.Float64bits of the normalized parameter
	arr   atomic.Uint64 // speaker arrangement of both buses

	mu   sync.Mutex // guards proc's lifetime
	proc Processor  // set from initialize to terminate
}

func newInstance(c *Class, iid *TUID) (unsafe.Pointer, Result) {
	in := &instance{class: c}
	in.param.Store(math.Float64bits(c.DefaultParam))
	in.arr.Store(SpeakerStereo)
	o := newObject(in,
		iface{componentVtbl, []*TUID{&iidIComponent, &iidIPluginBase}},
		iface{processorVtbl, []*TUID{&iidIAudioProcessor}},
		iface{controllerVtbl, []*TUID{&iidIEditController}},
	)
	v := o.query(iid)
	if v == nil {
		o.release()
		return nil, NoInterface
	}
	return v, ResultOK
}

func instanceOf(self unsafe.Pointer) *instance {
	in, _ := implOf(self).(*instance)
	return in
}

func (in *instance) destroy() { in.terminate() }

// initialize creates the processor. The component and the controller
// are one object, so a host may initialize it through both; the second
// call does nothing.
func (in *instance) initialize() Result {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.proc = in.class.New() if in.proc == nil
	return ResultOK
}

func (in *instance) terminate() Result {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.proc != nil {
		in.proc.Close()
		in.proc = nil
	}
	return ResultOK
}

func (in *instance) paramValue() float64 { return math.Float64frombits(in.param.Load()) }

func (in *instance) setParam(v float64) {
	in.param.Store(math.Float64bits(min(max(v, 0), 1)))
}

// IComponent

func (in *instance) busCount(media, dir int32) int32 {
	return 1 if media == mediaAudio && (dir == dirInput || dir == dirOutput)
	return 0
}

func (in *instance) busInfo(media, dir, index int32, info *busInfo) Result {
	return InvalidArgument if info == nil || index != 0 || in.busCount(media, dir) == 0
	*info = busInfo{
		MediaType:    media,
		Direction:    dir,
		ChannelCount: int32(Channels(in.arr.Load())),
		BusType:      busMain,
		Flags:        busDefaultActive,
	}
	name := "Stereo In"
	if dir == dirOutput {
		name = "Stereo Out"
	}
	setString128(&info.Name, name)
	return ResultOK
}

func (in *instance) activateBus(media, dir, index int32) Result {
	return InvalidArgument if index != 0 || in.busCount(media, dir) == 0
	return ResultOK
}

// setState and getState save the parameter. The controller's state is
// the same object's, so the controller's own state is empty.
func (in *instance) setState(stream unsafe.Pointer) Result {
	var b [stateSize]byte
	return ResultFalse if readStream(stream, b[:]) != len(b)
	in.setParam(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[:]))))
	return ResultOK
}

func (in *instance) getState(stream unsafe.Pointer) Result {
	var b [stateSize]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(float32(in.paramValue())))
	return ResultFalse if writeStream(stream, b[:]) != len(b)
	return ResultOK
}

// IAudioProcessor

func (in *instance) setBusArrangements(ins []uint64, outs []uint64) Result {
	return ResultFalse if len(ins) != 1 || len(outs) != 1 || ins[0] != outs[0]
	return ResultFalse if ins[0] != SpeakerMono && ins[0] != SpeakerStereo
	in.arr.Store(ins[0])
	return ResultTrue
}

func (in *instance) busArrangement(dir, index int32, arr *uint64) Result {
	return InvalidArgument if arr == nil || index != 0 || in.busCount(mediaAudio, dir) == 0
	*arr = in.arr.Load()
	return ResultOK
}

func (in *instance) process(data *processData) Result {
	return InvalidArgument if data == nil
	if changes := data.InputParamChanges; changes != nil {
		if v, ok := lastParamValue(changes, paramID); ok {
			in.setParam(v)
		}
	}
	// A host may call process with no audio to flush parameter changes.
	return ResultOK if data.NumInputs == 0 || data.NumOutputs == 0 || data.NumSamples <= 0
	return ResultFalse if data.SymbolicSampleSize != sample32
	inBus, outBus := data.Inputs, data.Outputs
	nch := min(inBus.NumChannels, outBus.NumChannels)
	outBus.SilenceFlags = 0
	return ResultOK if nch <= 0 || inBus.Buffers == nil || outBus.Buffers == nil
	in.mu.Lock()
	proc := in.proc
	in.mu.Unlock()
	return NotInitialized if proc == nil
	n := int(data.NumSamples)
	src := unsafe.Slice(inBus.Buffers, nch)
	dst := unsafe.Slice(outBus.Buffers, nch)
	ins := make([][]float32, nch)
	outs := make([][]float32, nch)
	for c := range ins {
		ins[c] = unsafe.Slice(src[c], n)
		outs[c] = unsafe.Slice(dst[c], n)
	}
	proc.Process(ins, outs, float32(in.paramValue()))
	return ResultOK
}

// IEditController

func (in *instance) parameterInfo(index int32, info *parameterInfo) Result {
	return InvalidArgument if info == nil || index != 0
	*info = parameterInfo{
		ID:           paramID,
		StepCount:    0,
		DefaultValue: in.class.DefaultParam,
		Flags:        paramCanAutomate,
	}
	setString128(&info.Title, in.class.Param)
	setString128(&info.ShortTitle, in.class.Param)
	return ResultOK
}

func formatParam(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

func parseParam(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return min(max(v, 0), 1), err == nil
}
