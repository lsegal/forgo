// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goroot

import (
	"os"
	"path/filepath"
	"runtime"
)

// GoCommand returns the path of the go command installed in goroot.
//
// forgo's make.bash renames the go command it builds from bin/go to
// bin/forgo (so that putting bin on PATH does not shadow another Go
// installation), so GoCommand returns bin/go if it exists, otherwise
// bin/forgo if that exists, and otherwise bin/go.
func GoCommand(goroot string) string {
	var exeSuffix string
	if runtime.GOOS == "windows" {
		exeSuffix = ".exe"
	}
	goCmd := filepath.Join(goroot, "bin", "go"+exeSuffix)
	if _, err := os.Stat(goCmd); err == nil {
		return goCmd
	}
	forgoCmd := filepath.Join(goroot, "bin", "forgo"+exeSuffix)
	if _, err := os.Stat(forgoCmd); err == nil {
		return forgoCmd
	}
	return goCmd
}
