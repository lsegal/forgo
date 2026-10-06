// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cgotest

// cgo must walk forgo's postfix if, throw and ? (issue #39), including the
// C references inside them.

/*
#include <errno.h>

#define FORGO_LIMIT 100

static int forgoHalve(int x) {
	if (x % 2 != 0) {
		errno = EINVAL;
		return -1;
	}
	return x / 2;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

var errForgoTooBig = errors.New("too big")

func forgoHalve(x C.int) (r C.int, err error) {
	throw errForgoTooBig if x > C.FORGO_LIMIT
	throw fmt.Errorf("negative: %d", C.int(x)) if x < 0
	r, err = C.forgoHalve(x) if x != 0
	return
}

func forgoQuarter(x int) (n int, err error) {
	h := forgoHalve(C.int(x))?
	n = int(forgoHalve(C.int(h))?)
	return
}

func testForgoSyntax(t *testing.T) {
	if n, err := forgoQuarter(12); n != 3 || err != nil {
		t.Errorf("forgoQuarter(12) = %d, %v; want 3, nil", n, err)
	}
	if n, err := forgoQuarter(0); n != 0 || err != nil {
		t.Errorf("forgoQuarter(0) = %d, %v; want 0, nil", n, err)
	}
	if _, err := forgoQuarter(6); err != syscall.Errno(C.EINVAL) {
		t.Errorf("forgoQuarter(6) error = %v; want EINVAL", err)
	}
	if _, err := forgoQuarter(200); err != errForgoTooBig {
		t.Errorf("forgoQuarter(200) error = %v; want %v", err, errForgoTooBig)
	}
	if _, err := forgoQuarter(-4); err == nil || err.Error() != "negative: -4" {
		t.Errorf("forgoQuarter(-4) error = %v; want negative: -4", err)
	}
}
