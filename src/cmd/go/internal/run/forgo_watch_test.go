// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package run

import (
	"bufio"
	"context"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// watchReloadSource is the program TestWatchReloadUsesTypes runs. render is
// the function the test edits: GEN is replaced by the body the program starts
// with, and then by the new body while it is running. check and main never
// change, so they keep running the code of the original build.
const watchReloadSource = `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"time"
)

type payload struct {
	OK   bool   ` + "`json:\"ok\"`" + `
	Note string ` + "`json:\"note\"`" + `
}

var hostType = reflect.TypeOf(payload{})

// Keeps encoding/json imported while render does not use it.
var _ = json.Marshal

// check is never reloaded: it asserts a value boxed by whatever generation
// of render is running against the original build's type.
func check(v any) string {
	p, ok := v.(payload)
	return fmt.Sprintf("assert=%v sametype=%v note=%s", ok, reflect.TypeOf(v) == hostType, p.Note)
}

func render(i int) bool {
	GEN
}

func main() {
	for i := 0; i < 600; i++ {
		if render(i) {
			os.Exit(0)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("no reload happened")
	os.Exit(3)
}
`

// watchReloadGen1 is the body every case reloads into. It resolves type
// offsets relative to type descriptors (the struct converted to an
// interface, encoding/json walking its fields through reflection, fmt
// formatting it) and calls fresh copies of library code that does the same.
const watchReloadGen1 = `_ = json.NewEncoder(os.Stdout).Encode(payload{OK: true, Note: fmt.Sprint("gen1 ", i*1000, len(os.Args))})
	fmt.Printf("%+v\n", payload{OK: true, Note: "fmt"})
	fmt.Println(check(payload{OK: true, Note: "boxed"}))
	return true`

// TestWatchReloadUsesTypes reloads a function whose new code uses type
// descriptors (lsegal/forgo#50).
func TestWatchReloadUsesTypes(t *testing.T) {
	switch {
	case runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "darwin",
		runtime.GOARCH != "amd64" && !(runtime.GOARCH == "arm64" && runtime.GOOS == "darwin"):
		t.Skipf("hot reload is not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveGoBuild(t)
	if testing.Short() {
		t.Skip("builds and runs programs")
	}

	for _, tc := range []struct {
		name string
		gen0 string
	}{
		{
			// The program from the issue. The hot image used to carry its
			// own, empty copy of the runtime's module list, which the fresh
			// copies of library code in it walked: the first type offset
			// they resolved crashed the program.
			name: "json",
			gen0: `_ = json.NewEncoder(os.Stdout).Encode(payload{OK: true, Note: fmt.Sprint("gen0 ", i)})
	return false`,
		},
		{
			// Nothing in the original build calls reflect.Type's
			// Implements, so its linker pointed that itab slot at
			// runtime.unreachableMethod, and encoding/json calls it on its
			// first use. The reload has to fill the slot in.
			name: "deadmethod",
			gen0: `fmt.Println("gen0", i)
	return false`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testWatchReload(t, tc.gen0)
		})
	}
}

func testWatchReload(t *testing.T, gen0 string) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module reload\n\ngo 1.25\n")
	write("main.go", strings.Replace(watchReloadSource, "GEN", gen0, 1))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, testenv.GoToolPath(t), "run", "--watch", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// The program starts printing before the watcher has taken its first
	// look at the source tree, so an edit made right away could already be
	// part of what it considers unchanged. Save the new source every five
	// seconds the old code keeps running instead; the watcher compares
	// modification times, so each save is a change until one is reloaded.
	var out []string
	saved := time.Time{}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		out = append(out, line)
		if strings.Contains(line, "gen0 ") && time.Since(saved) > 5*time.Second {
			write("main.go", strings.Replace(watchReloadSource, "GEN", watchReloadGen1, 1))
			saved = time.Now()
		}
	}
	err = cmd.Wait()
	got := strings.Join(out, "\n")
	if err != nil {
		t.Fatalf("forgo run --watch: %v\nstdout:\n%s\nstderr:\n%s", err, got, stderr.String())
	}
	for _, want := range []string{
		`{"ok":true,"note":"gen1 `,
		`{OK:true Note:fmt}`,
		`assert=true sametype=true note=boxed`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q\nstdout:\n%s\nstderr:\n%s", want, got, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "forgo: reloaded") {
		t.Errorf("stderr does not report a reload:\n%s", stderr.String())
	}
}
