// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !plan9 && !windows

package main

// A stop-the-world that begins while a C thread's last call into Go is
// returning must not wait for that thread to call into Go again: it never
// will (lsegal/forgo#46).

/*
extern void ForgoSTWThreads(void);
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"time"
)

func init() {
	register("ForgoSTWCallbackExit", ForgoSTWCallbackExit)
}

var forgoSTWSink []byte

//export ForgoSTWCallback
func ForgoSTWCallback() {
	for i := 0; i < 10; i++ {
		forgoSTWSink = make([]byte, 1024)
	}
}

func ForgoSTWCallbackExit() {
	// ReadMemStats stops the world, as often as it can.
	go func() {
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
		}
	}()
	// A stuck stop lasts until sysmon happens to wake up, which can take
	// until the next timer fires.
	for start := time.Now(); time.Since(start) < 3*time.Second; {
		done := make(chan bool)
		go func() {
			C.ForgoSTWThreads()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			fmt.Println("stop-the-world stuck behind a C thread that left Go")
			os.Exit(1)
		}
	}
	// Stopping the world takes a few milliseconds even on a busy machine.
	samples := []metrics.Sample{
		{Name: "/sched/pauses/stopping/gc:seconds"},
		{Name: "/sched/pauses/stopping/other:seconds"},
	}
	metrics.Read(samples)
	for _, s := range samples {
		h := s.Value.Float64Histogram()
		for i, n := range h.Counts {
			if n > 0 && h.Buckets[i] >= 2 {
				fmt.Printf("stopping the world took %gs or more (%s)\n", h.Buckets[i], s.Name)
				os.Exit(1)
			}
		}
	}
	fmt.Println("OK")
}
