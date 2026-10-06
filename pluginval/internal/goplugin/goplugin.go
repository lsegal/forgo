// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package goplugin is the audio processing of the pluginval test plugins:
// effect instances that the plugins' VST3 components drive. Every instance
// keeps its runtime busy, both on the host's audio thread and in a worker
// goroutine of its own, so pluginval's tests run while several Go runtimes
// allocate and collect garbage at once.
package goplugin

import (
	"math"
	"runtime"
	"sync/atomic"
)

// An Effect maps one input sample to one output sample. param is the
// plugin's single parameter, normalized to [0, 1].
type Effect func(x, param float32) float32

// gcEvery is how many processed blocks pass between forced collections.
const gcEvery = 32

// An Instance runs an effect. Its methods implement vst3.Processor.
type Instance struct {
	effect Effect
	blocks int

	// Process publishes each block to the worker goroutine through
	// latest and seq.
	latest atomic.Pointer[[]float32]
	seq    atomic.Uint64
	stop   atomic.Bool
	done   chan struct{}
}

// sink keeps the worker's garbage reachable for a moment so the
// collector has real work.
var sink atomic.Pointer[[]float32]

// New creates an instance running effect and starts its worker.
func New(effect Effect) *Instance {
	in := &Instance{effect: effect, done: make(chan struct{})}
	go in.analyze()
	return in
}

// Close stops the instance's worker.
func (in *Instance) Close() {
	in.stop.Store(true)
	<-in.done
}

// Process runs the instance's effect over the channels of in, writing
// out (which may share buffers with in). It copies each channel into a
// fresh Go slice first and forces a collection every gcEvery blocks, so
// the runtime is allocating and collecting on the host's audio thread.
// It hands the block to the instance's worker.
func (in *Instance) Process(ins, outs [][]float32, param float32) {
	for c, src := range ins {
		dst := outs[c]
		buf := make([]float32, len(src))
		copy(buf, src)
		for i, x := range buf {
			dst[i] = flush(in.effect(x, param))
		}
		if c == 0 {
			in.latest.Store(&buf)
			in.seq.Add(1)
		}
	}
	// One audio thread drives an instance at a time, so blocks needs no
	// lock.
	in.blocks++
	if in.blocks%gcEvery == 0 {
		runtime.GC()
	}
}

// flush zeroes subnormal and non-finite samples, which pluginval rejects.
func flush(y float32) float32 {
	if math.Abs(float64(y)) < 1e-20 || math.IsNaN(float64(y)) || math.IsInf(float64(y), 0) {
		return 0
	}
	return y
}

// analyze is the instance's worker. It waits for each block the way
// real-time audio code hands work between threads, spinning on an atomic
// instead of blocking, then allocates a peak envelope of the block.
//
// The spin loop makes no calls, so it has no cooperative preemption point.
// A collection can only stop it by signalling its thread (asynchronous
// preemption), which needs the signal routing that lets a runtime loaded
// before another one still preempt its own goroutines (#5).
func (in *Instance) analyze() {
	defer close(in.done)
	var seen uint64
	for {
		for in.seq.Load() == seen && !in.stop.Load() {
		}
		if in.stop.Load() {
			return
		}
		seen = in.seq.Load()
		block := *in.latest.Load()
		peaks := make([]float32, 0, len(block)/16+1)
		for i := 0; i < len(block); i += 16 {
			var peak float32
			for _, x := range block[i:min(i+16, len(block))] {
				peak = max(peak, x, -x)
			}
			peaks = append(peaks, peak)
		}
		sink.Store(&peaks)
	}
}
