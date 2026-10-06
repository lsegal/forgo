// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build test_run

// cgo must walk forgo's postfix if, throw and ? (issue #39), including the
// C references inside them.

package main

/*
#include <errno.h>

#define FORGO_LIMIT 100

static int halve(int x) {
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
)

var errTooBig = errors.New("too big")

func halve(x C.int) (n int, err error) {
	var r C.int
	throw errTooBig if x > C.FORGO_LIMIT
	throw fmt.Errorf("negative: %d", C.int(x)) if x < 0
	r, err = C.halve(x) if x != 0
	return int(r), err
}

func quarter(x int) (n int, err error) {
	h := halve(C.int(x))?
	n = halve(C.int(h))?
	return
}

func main() {
	for _, x := range []int{12, 0, 6, 200, -4} {
		n, err := quarter(x)
		if err == syscall.Errno(C.EINVAL) {
			err = errors.New("EINVAL")
		}
		fmt.Printf("quarter(%d) = %d, %v\n", x, n, err)
	}
}
