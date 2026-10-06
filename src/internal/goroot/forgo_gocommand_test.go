// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goroot_test

import (
	"internal/goroot"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGoCommand(t *testing.T) {
	var exeSuffix string
	if runtime.GOOS == "windows" {
		exeSuffix = ".exe"
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	goCmd := filepath.Join(bin, "go"+exeSuffix)
	forgoCmd := filepath.Join(bin, "forgo"+exeSuffix)

	if got := goroot.GoCommand(dir); got != goCmd {
		t.Errorf("GoCommand with an empty bin = %q, want %q", got, goCmd)
	}

	if err := os.WriteFile(forgoCmd, nil, 0o777); err != nil {
		t.Fatal(err)
	}
	if got := goroot.GoCommand(dir); got != forgoCmd {
		t.Errorf("GoCommand with only bin/forgo = %q, want %q", got, forgoCmd)
	}

	if err := os.WriteFile(goCmd, nil, 0o777); err != nil {
		t.Fatal(err)
	}
	if got := goroot.GoCommand(dir); got != goCmd {
		t.Errorf("GoCommand with bin/go and bin/forgo = %q, want %q", got, goCmd)
	}
}
