// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoCSources checks that pluginval/ stays pure Go: no C or C++
// sources or headers, no CMake build, and no #include in a cgo preamble.
func TestNoCSources(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "build" && filepath.Dir(path) == root {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".m", ".mm", ".cmake":
			t.Errorf("%s: C or C++ source", path)
		}
		if d.Name() == "CMakeLists.txt" {
			t.Errorf("%s: CMake build", path)
		}
		if filepath.Ext(path) == ".go" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#include") {
					t.Errorf("%s:%d: #include", path, i+1)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
