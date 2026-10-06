// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package goplugin is the Go side of the pluginval test plugins: a table
// of effect instances that the VST3 shim drives through each library's
// exported functions. Every instance keeps its runtime busy, both on the
// host's audio thread and in a worker goroutine of its own, so pluginval's
// tests run while several Go runtimes allocate and collect garbage at once.
package goplugin

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// An Effect maps one input sample to one output sample. param is the
// plugin's single parameter, normalized to [0, 1].
type Effect func(x, param float32) float32

// gcEvery is how many processed blocks pass between forced collections.
const gcEvery = 32

type instance struct {
	effect Effect
	blocks int

	// Process publishes each block to the worker goroutine through
	// latest and seq.
	latest atomic.Pointer[[]float32]
	seq    atomic.Uint64
	stop   atomic.Bool
	done   chan struct{}
}

var (
	mu        sync.Mutex
	instances = map[int32]*instance{}
	nextID    int32

	// sink keeps the worker's garbage reachable for a moment so the
	// collector has real work.
	sink atomic.Pointer[[]float32]
)

// New creates an instance running effect and returns its handle.
func New(effect Effect) int32 {
	in := &instance{effect: effect, done: make(chan struct{})}
	go in.analyze()
	mu.Lock()
	defer mu.Unlock()
	nextID++
	instances[nextID] = in
	return nextID
}

// Free stops the instance's worker and forgets it. Unknown handles
// are ignored.
func Free(id int32) {
	mu.Lock()
	in := instances[id]
	delete(instances, id)
	mu.Unlock()
	if in == nil {
		return
	}
	in.stop.Store(true)
	<-in.done
}

// Process runs the instance's effect over nch channels of n samples,
// reading in and writing out (which may alias). It copies each channel
// into a fresh Go slice first and forces a collection every gcEvery
// blocks, so the runtime is allocating and collecting on the host's
// audio thread. It hands the block to the instance's worker. It returns
// false for an unknown handle.
func Process(id int32, in, out unsafe.Pointer, nch, n int32, param float32) bool {
	mu.Lock()
	inst := instances[id]
	mu.Unlock()
	if inst == nil {
		return false
	}
	ins := unsafe.Slice((**float32)(in), nch)
	outs := unsafe.Slice((**float32)(out), nch)
	for c := range ins {
		src := unsafe.Slice(ins[c], n)
		dst := unsafe.Slice(outs[c], n)
		buf := make([]float32, n)
		copy(buf, src)
		for i, x := range buf {
			dst[i] = flush(inst.effect(x, param))
		}
		if c == 0 {
			inst.latest.Store(&buf)
			inst.seq.Add(1)
		}
	}
	// One audio thread drives an instance at a time, so blocks needs no
	// lock.
	inst.blocks++
	if inst.blocks%gcEvery == 0 {
		runtime.GC()
	}
	return true
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
func (in *instance) analyze() {
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
