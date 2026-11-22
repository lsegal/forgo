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

// shouter is boxed by the original build, so its type descriptor, method
// table included, is in the running program, but nothing there calls Shout:
// its linker wrote -1 for Shout's method table entries.
type shouter struct{ word string }

func (s shouter) Shout(n int8) [2]string {
	return [2]string{fmt.Sprint(s.word, n), "shouted"}
}

var boxedShouter any = shouter{"hey"}

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
	if boxedShouter == nil {
		os.Exit(4)
	}
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

// watchReloadWant is what watchReloadGen1 prints.
var watchReloadWant = []string{
	`{"ok":true,"note":"gen1 `,
	`{OK:true Note:fmt}`,
	`assert=true sametype=true note=boxed`,
}

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
		gen1 string   // defaults to watchReloadGen1
		want []string // defaults to watchReloadWant
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
		{
			// The new code converts a value the original build boxed to an
			// interface type that is new, so the runtime builds the itab,
			// resolving Shout through the method table of shouter's type
			// descriptor. The reload has to fill in that table's entries
			// (lsegal/forgo#60).
			name: "newiface",
			gen0: `fmt.Println("gen0", i)
	return false`,
			gen1: `s, ok := boxedShouter.(interface{ Shout(int8) [2]string })
	if ok {
		r := s.Shout(7)
		fmt.Println("iface", r[0], r[1])
	} else {
		fmt.Println("iface missing")
	}
	return true`,
			want: []string{"iface hey7 shouted"},
		},
		{
			// The same method table entries, reached through reflect.
			name: "reflectmethod",
			gen0: `fmt.Println("gen0", i)
	return false`,
			gen1: `m := reflect.ValueOf(boxedShouter).MethodByName("Shout")
	r := m.Call([]reflect.Value{reflect.ValueOf(int8(9))})[0].Interface().([2]string)
	fmt.Println("reflect", m.Type(), r[0], r[1])
	return true`,
			want: []string{"reflect func(int8) [2]string hey9 shouted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen1, want := tc.gen1, tc.want
			if gen1 == "" {
				gen1, want = watchReloadGen1, watchReloadWant
			}
			testWatchReload(t, tc.gen0, gen1, want)
		})
	}
}

func testWatchReload(t *testing.T, gen0, gen1 string, want []string) {
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
			write("main.go", strings.Replace(watchReloadSource, "GEN", gen1, 1))
			saved = time.Now()
		}
	}
	err = cmd.Wait()
	got := strings.Join(out, "\n")
	if err != nil {
		t.Fatalf("forgo run --watch: %v\nstdout:\n%s\nstderr:\n%s", err, got, stderr.String())
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output does not contain %q\nstdout:\n%s\nstderr:\n%s", w, got, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "forgo: reloaded") {
		t.Errorf("stderr does not report a reload:\n%s", stderr.String())
	}
}
