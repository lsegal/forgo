// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package objabi

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Two forgo toolchains synced to the same Go release must not share tool
// IDs, or the go command reuses cache entries one compiler wrote with the
// other (go.dev/issue/33091 for GOEXPERIMENT; forgo #59 for forgo itself).
func TestToolVersionForgo(t *testing.T) {
	const release = "go1.27.4"
	older := toolVersion("compile", true, release, "simd", "0.7.4", "a/b")
	newer := toolVersion("compile", true, release, "simd", "0.7.5", "a/b")
	if older == newer {
		t.Errorf("toolchains differing only in forgo version share tool ID %q", older)
	}
	if want := "compile version go1.27.4 X:simd forgo:0.7.4 contentID=b"; older != want {
		t.Errorf("tool ID = %q, want %q", older, want)
	}

	rebuilt := toolVersion("compile", true, release, "simd", "0.7.4", "a/c")
	if older == rebuilt {
		t.Errorf("toolchains differing only in content ID share tool ID %q", older)
	}

	// The same tool built by a different toolchain has a different action
	// ID but the same content, and must keep its tool ID; otherwise make.bash
	// finds the final toolchain stale.
	if rebuiltBy := toolVersion("compile", true, release, "simd", "0.7.4", "z/b"); older != rebuiltBy {
		t.Errorf("tool ID depends on the action ID: %q vs %q", older, rebuiltBy)
	}

	// Without a FORGO_VERSION or a build ID the ID is upstream's.
	if got, want := toolVersion("link", true, release, "", "", ""), "link version go1.27.4"; got != want {
		t.Errorf("tool ID = %q, want %q", got, want)
	}

	// On a devel version the go command reads the build ID from the
	// last field, so it must stay last.
	devel := strings.Fields(toolVersion("compile", true, "devel go1.28-abc", "", "0.7.4", "a/b"))
	if last := devel[len(devel)-1]; last != "buildID=a/b" {
		t.Errorf("devel tool ID ends in %q, want buildID=a/b", last)
	}

	// Plain -V is unchanged.
	if got, want := toolVersion("compile", false, release, "", "0.7.4", "a/b"), "compile version go1.27.4"; got != want {
		t.Errorf("-V = %q, want %q", got, want)
	}
}

// The installed compiler reports the FORGO_VERSION it was built with.
func TestCompileToolIDForgoVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(testenv.GOROOT(t), "FORGO_VERSION"))
	if err != nil {
		t.Skipf("GOROOT has no FORGO_VERSION: %v", err)
	}
	out, err := testenv.Command(t, testenv.GoToolPath(t), "tool", "compile", "-V=full").CombinedOutput()
	if err != nil {
		t.Fatalf("go tool compile -V=full: %v\n%s", err, out)
	}
	want := "forgo:" + strings.TrimSpace(string(data))
	if !slices.Contains(strings.Fields(string(out)), want) {
		t.Errorf("go tool compile -V=full = %q, want a %q field", out, want)
	}
}
