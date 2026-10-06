// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package goplugin is the Go side of the pluginval test plugins: a table
// of effect instances that the VST3 shim drives through each library's
// exported functions. Every instance keeps its runtime busy, both on the
// host's audio thread and in a goroutine of its own, so pluginval's tests
// run while several Go runtimes allocate and collect garbage at once.
package goplugin

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
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
	stop   chan struct{}
	done   chan struct{}
}

var (
	mu        sync.Mutex
	instances = map[int32]*instance{}
	nextID    int32

	// sink keeps churn's garbage reachable for a moment so the
	// collector has real work.
	sink atomic.Pointer[[][]byte]
)

// New creates an instance running effect and returns its handle.
func New(effect Effect) int32 {
	in := &instance{effect: effect, stop: make(chan struct{}), done: make(chan struct{})}
	go in.churn()
	mu.Lock()
	defer mu.Unlock()
	nextID++
	instances[nextID] = in
	return nextID
}

// Free stops the instance's goroutine and forgets it. Unknown handles
// are ignored.
func Free(id int32) {
	mu.Lock()
	in := instances[id]
	delete(instances, id)
	mu.Unlock()
	if in == nil {
		return
	}
	close(in.stop)
	<-in.done
}

// Process runs the instance's effect over nch channels of n samples,
// reading in and writing out (which may alias). It copies each channel
// into a fresh Go slice first and forces a collection every gcEvery
// blocks, so the runtime is allocating and collecting on the host's
// audio thread. It returns false for an unknown handle.
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

// churn allocates in the background until the instance is freed, so this
// runtime's collector runs while other runtimes are working.
func (in *instance) churn() {
	defer close(in.done)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-in.stop:
			return
		case <-tick.C:
		}
		garbage := make([][]byte, 256)
		for j := range garbage {
			garbage[j] = make([]byte, 1024+j)
		}
		sink.Store(&garbage)
	}
}
