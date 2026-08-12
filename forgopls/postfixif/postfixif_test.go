// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package postfixif_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"forgopls/postfixif"
)

func Test(t *testing.T) {
	analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), postfixif.Analyzer, "a")
}
