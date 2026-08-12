// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package version

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// forgoVersion returns forgo's own version (independent of the Go release
// it's synced against), read from the FORGO_VERSION file at the root of
// the running toolchain's GOROOT -- the same file install.sh compares
// against an installed toolchain's version. It reports false if the file
// can't be read (e.g. a GOROOT that isn't a forgo toolchain at all).
func forgoVersion() (string, bool) {
	data, err := os.ReadFile(filepath.Join(runtime.GOROOT(), "FORGO_VERSION"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}
