// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package x86_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDarwinAMD64TLSG checks that darwin/amd64 code reaches g through the
// per-runtime offset in runtime.tls_g rather than the fixed Apple-reserved
// slot at GS:0x30. With the fixed slot, every Go runtime loaded into one
// process (such as several c-shared libraries) shares a single g.
func TestDarwinAMD64TLSG(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module tlsg\n"), 0666); err != nil {
		t.Fatal(err)
	}
	src := "package main\n\nfunc main() { println(\"hello\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0666); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "tlsg")
	build := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe)
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	objdump := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", exe)
	out, err := objdump.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump failed: %v\n%s", err, out)
	}

	var sawTLSG, sawGS bool
	for line := range strings.Lines(string(out)) {
		switch {
		case strings.Contains(line, "GS:0x30"), strings.Contains(line, "GS:0, "):
			t.Errorf("g accessed at a fixed TLS offset instead of through runtime.tls_g:\n%s", line)
		case strings.Contains(line, "MOVQ runtime.tls_g(SB), "):
			sawTLSG = true
		case strings.Contains(line, "GS:0("):
			sawGS = true
		}
	}
	if !sawTLSG || !sawGS {
		t.Errorf("no g load through runtime.tls_g found (load offset: %v, GS-relative access: %v)", sawTLSG, sawGS)
	}
	if !strings.Contains(string(out), "TEXT runtime.tlsinit(SB)") {
		t.Errorf("runtime.tlsinit missing from darwin/amd64 binary")
	}
}
