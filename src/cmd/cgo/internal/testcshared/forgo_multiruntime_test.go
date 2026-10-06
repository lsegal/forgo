// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cshared_test

import (
	"context"
	"debug/elf"
	"debug/macho"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMultiRuntime loads two c-shared libraries, each with its own Go
// runtime, into one C host process, the way plugin hosts load plugins, and
// checks that the runtimes do not interfere with each other. See
// testdata/multiruntime/host.c for what each mode does.
func TestMultiRuntime(t *testing.T) {
	globalSkip(t)
	testenv.MustHaveGoBuild(t)
	testenv.MustHaveCGO(t)
	testenv.MustHaveBuildMode(t, "c-shared")
	testenv.MustHaveExec(t)

	dir := t.TempDir()
	ext := ".so"
	switch GOOS {
	case "windows":
		ext = ".dll"
	case "darwin", "ios":
		ext = ".dylib"
	}
	liba := filepath.Join(dir, "liba"+ext)
	libb := filepath.Join(dir, "libb"+ext)
	run(t, nil, testenv.GoToolPath(t), "build", "-buildmode=c-shared", "-o", liba, "./multiruntime/liba")
	run(t, nil, testenv.GoToolPath(t), "build", "-buildmode=c-shared", "-o", libb, "./multiruntime/libb")
	// The same library under a second path is a second runtime built from
	// identical code, as when a host loads both the VST2 and the VST3 build
	// of one plugin.
	liba2 := filepath.Join(dir, "liba2"+ext)
	copyFile(t, liba2, liba)

	host := filepath.Join(dir, "host"+exeSuffix)
	ccArgs := []string{"-o", host, filepath.Join("multiruntime", "host.c")}
	if GOOS != "windows" {
		ccArgs = append(ccArgs, "-pthread")
		if GOOS != "freebsd" && GOOS != "openbsd" && GOOS != "netbsd" {
			ccArgs = append(ccArgs, "-ldl")
		}
	}
	runCC(t, ccArgs...)

	t.Run("ExportedSymbols", func(t *testing.T) {
		checkNoGoSymbolsExported(t, liba)
	})

	pairs := []struct{ name, a, b string }{
		{"Different", liba, libb},
		{"SameSource", liba, liba2},
	}
	modes := []string{"stress", "instances", "nested", "fault", "preempt", "hostsig", "notify", "notifyhost", "sigpipe", "rlimit"}
	unixOnly := map[string]bool{"hostsig": true, "notify": true, "notifyhost": true, "sigpipe": true, "rlimit": true}
	for _, p := range pairs {
		for _, mode := range modes {
			if unixOnly[mode] && GOOS == "windows" {
				continue
			}
			t.Run(p.name+"/"+mode, func(t *testing.T) {
				runMultiRuntimeHost(t, nil, host, mode, p.a, p.b)
			})
		}
	}
	// A Ctrl+Break console event reaches every runtime that called Notify.
	if GOOS == "windows" {
		for _, p := range pairs {
			t.Run(p.name+"/ctrlbreak", func(t *testing.T) {
				runMultiRuntimeHost(t, nil, host, "ctrlbreak", p.a, p.b)
			})
		}
	}
	// Every instance of a single plugin shares one library image and one
	// runtime, however many times the host opens it.
	t.Run("OneLibrary/instances", func(t *testing.T) {
		runMultiRuntimeHost(t, nil, host, "instances", liba, liba)
	})
	if GOOS != "windows" {
		t.Run("RTLDGlobal/stress", func(t *testing.T) {
			runMultiRuntimeHost(t, []string{"MULTIRUNTIME_RTLD_GLOBAL=1"}, host, "stress", liba, libb)
		})
	}
	// An unrecovered panic or fault in one library ends the process with
	// that library's crash report alone, in both directions and whatever
	// GOTRACEBACK asks for.
	for _, mode := range []string{"crash", "crashfault"} {
		for _, tb := range []string{"all", "crash"} {
			if tb == "crash" && GOOS == "windows" {
				// The crash setting hands the fault to Windows Error
				// Reporting, which may wait for a user.
				continue
			}
			for _, dir := range []struct{ name, a, b, own, other string }{
				{"AB", liba, libb, "parkedInLibA", "parkedInLibB"},
				{"BA", libb, liba, "parkedInLibB", "parkedInLibA"},
			} {
				t.Run(mode+"/"+tb+"/"+dir.name, func(t *testing.T) {
					checkMultiRuntimeCrash(t, host, mode, tb, dir.a, dir.b, dir.own, dir.other)
				})
			}
		}
	}
	// golang/go#65050 crashed intermittently, so run its shape repeatedly.
	t.Run("Upstream65050", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			runMultiRuntimeHost(t, nil, host, "upstream", liba, liba2)
		}
	})
}

// runMultiRuntimeHost runs the host in mode and fails the test unless it
// prints PASS. A hang, such as a stop-the-world that can never preempt a
// goroutine, fails after a timeout instead of stalling the whole test.
func runMultiRuntimeHost(t *testing.T, env []string, host, mode, a, b string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, mode, a, b)
	cmd.Env = append(os.Environ(), env...)
	// A console control event the host sends itself must not reach the
	// test, which shares its console.
	setNewProcessGroup(cmd)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s %s: timed out (a runtime is hung)\n%s", host, mode, out)
	}
	if s := strings.TrimSpace(string(out)); err == nil && strings.HasPrefix(s, "SKIP: ") {
		t.Skip(strings.TrimPrefix(s, "SKIP: "))
	}
	if err != nil || strings.TrimSpace(string(out)) != "PASS" {
		t.Fatalf("%s %s: %v\n%s", host, mode, err, out)
	}
}

// checkMultiRuntimeCrash runs the host in a crash mode, which makes library
// a panic or fault without recovering while library b is busy, and checks
// that the process died with a's crash report: a's goroutines, which
// include one named own, and none of b's, which include one named other.
func checkMultiRuntimeCrash(t *testing.T, host, mode, traceback, a, b, own, other string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, mode, a, b)
	cmd.Env = append(os.Environ(), "GOTRACEBACK="+traceback)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s %s: timed out\n%s", host, mode, out)
	}
	if err == nil {
		t.Fatalf("%s %s: exited successfully, want a crash\n%s", host, mode, out)
	}
	report := string(out)
	want := "panic: multiruntime: unrecovered panic"
	if mode == "crashfault" {
		want = "panic: runtime error: invalid memory address or nil pointer dereference"
	}
	for _, s := range []string{want, "goroutine ", "main.Crash", own} {
		if !strings.Contains(report, s) {
			t.Errorf("crash report does not contain %q", s)
		}
	}
	if strings.Contains(report, other) {
		t.Errorf("crash report contains %q from the other library", other)
	}
	if n := strings.Count(report, "panic: "); n != 1 {
		t.Errorf("crash report has %d panics, want 1", n)
	}
	if t.Failed() {
		t.Logf("%s %s:\n%s", host, mode, report)
	}
}

// checkNoGoSymbolsExported checks that a c-shared library exports none of
// its Go symbols, which would let the dynamic linker bind one runtime's
// references to another runtime's copy. Go symbol names always contain a
// dot (runtime.mheap_, main.main); //export functions and the cgo C helpers
// never do.
func checkNoGoSymbolsExported(t *testing.T, lib string) {
	var names []string
	switch GOOS {
	case "linux", "android", "freebsd", "netbsd", "openbsd", "dragonfly", "solaris", "illumos":
		f, err := elf.Open(lib)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		syms, err := f.DynamicSymbols()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range syms {
			if s.Section != elf.SHN_UNDEF && elf.ST_BIND(s.Info) != elf.STB_LOCAL {
				names = append(names, s.Name)
			}
		}
	case "darwin", "ios":
		f, err := macho.Open(lib)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		const nExt, nTypeMask, nSect = 0x01, 0x0e, 0x0e
		for _, s := range f.Symtab.Syms {
			if s.Type&nExt != 0 && s.Type&nTypeMask == nSect {
				names = append(names, s.Name)
			}
		}
	default:
		t.Skipf("no exported symbol check on %s", GOOS)
	}
	if len(names) == 0 {
		t.Fatalf("%s exports no symbols", lib)
	}
	found := false
	for _, name := range names {
		if strings.Contains(name, ".") {
			t.Errorf("%s exports Go symbol %q", lib, name)
		}
		if strings.TrimPrefix(name, "_") == "Bounce" {
			found = true
		}
	}
	if !found {
		t.Errorf("%s does not export Bounce; exports %v", lib, names)
	}
}
