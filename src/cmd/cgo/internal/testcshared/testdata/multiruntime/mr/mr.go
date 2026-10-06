// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package mr holds the Go side of the multi-runtime c-shared test. Every
// library built from testdata/multiruntime links its own copy of this
// package, and with it its own Go runtime.
package mr

import (
	"runtime"
	"sync"
	"time"
)

type node struct {
	next *node
	buf  []byte
	tag  string
}

var (
	mu    sync.Mutex
	keep  []*node
	table = map[int]*node{}
	sink  int
)

// Work allocates enough to keep this runtime's garbage collector busy and
// forces a collection every few calls. variant selects a different mix of
// allocations so that two libraries do not run identical code.
func Work(n, variant int) int {
	sum := 0
	mu.Lock()
	for i := 0; i < n; i++ {
		nd := &node{buf: make([]byte, 512+i%4096), tag: string(rune('a' + i%26))}
		nd.buf[i%len(nd.buf)] = byte(i)
		if variant == 0 {
			keep = append(keep, nd)
			if len(keep) > 256 {
				keep = keep[:0]
			}
		} else {
			nd.next = table[i%64]
			table[i%64] = nd
			if i%128 == 0 {
				table = map[int]*node{}
			}
		}
		sum += int(nd.buf[i%len(nd.buf)]) + len(nd.tag)
	}
	mu.Unlock()
	if n%5 == 0 {
		runtime.GC()
	}
	return sum
}

// RecoverPanic panics and recovers inside this runtime and reports 1 when
// the recovered value is the one that was thrown.
func RecoverPanic() (r int) {
	defer func() {
		if v, ok := recover().(string); ok && v == "bounce" {
			r = 1
		}
	}()
	panic("bounce")
}

// RecoverNilDeref dereferences a nil pointer and reports 1 when this
// runtime turned the fault into a recoverable runtime.Error.
func RecoverNilDeref() (r int) {
	defer func() {
		if _, ok := recover().(runtime.Error); ok {
			r = 1
		}
	}()
	var p *node
	sink = len(p.tag)
	return 0
}

// StartSpin starts a goroutine running a loop with no calls in it, which
// only asynchronous preemption can stop.
func StartSpin() {
	go func() {
		x := 0
		for {
			x++
			if x == -1 {
				sink = x
			}
		}
	}()
	time.Sleep(10 * time.Millisecond)
}

// GC runs a full collection, which has to preempt the spinning goroutine.
func GC() int {
	runtime.GC()
	return 1
}

// An instance is one plugin instance: a host creates many of them from one
// library (the same effect on several tracks) and drives each one from
// whichever audio thread it likes, one call at a time.
type instance struct {
	calls  int
	buffer []float64
}

var (
	instMu    sync.Mutex
	instances = map[int32]*instance{}
	nextInst  int32
)

// NewInstance creates an instance and returns its handle.
func NewInstance() int32 {
	instMu.Lock()
	defer instMu.Unlock()
	nextInst++
	instances[nextInst] = &instance{}
	return nextInst
}

func lookupInstance(id int32) *instance {
	instMu.Lock()
	defer instMu.Unlock()
	return instances[id]
}

// Process runs one block of audio through instance id and returns how many
// blocks that instance has processed, or -1 if id is not a live instance.
func Process(id int32, frames int) int {
	in := lookupInstance(id)
	if in == nil {
		return -1
	}
	in.buffer = make([]float64, frames)
	for i := range in.buffer {
		in.buffer[i] = float64(i*int(id)) * 0.5
	}
	in.calls++
	if in.calls%50 == 0 {
		runtime.GC()
	}
	return in.calls
}

// DestroyInstance destroys instance id and reports whether it was live.
func DestroyInstance(id int32) bool {
	instMu.Lock()
	defer instMu.Unlock()
	_, ok := instances[id]
	delete(instances, id)
	return ok
}

// LiveInstances returns the number of instances not yet destroyed.
func LiveInstances() int {
	instMu.Lock()
	defer instMu.Unlock()
	return len(instances)
}
