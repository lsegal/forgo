// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package runtime_test

import (
	"internal/race"
	"runtime"
	"testing"
)

// TestForgoSTWCallbackExit checks that stopping the world does not hang when
// a C thread returns from its last call into Go just as the stop begins
// (lsegal/forgo#46). The race is narrow, so the test does not fail every
// time without the fix.
func TestForgoSTWCallbackExit(t *testing.T) {
	t.Parallel()
	switch runtime.GOOS {
	case "windows", "plan9":
		t.Skipf("no pthreads on %s", runtime.GOOS)
	}
	if runtime.GOOS == "freebsd" && race.Enabled {
		t.Skipf("race + cgo freebsd not supported. See https://go.dev/issue/73788.")
	}
	got := runTestProg(t, "testprogcgo", "ForgoSTWCallbackExit")
	if want := "OK\n"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
