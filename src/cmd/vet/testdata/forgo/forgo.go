// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains tests for vet on forgo syntax: postfix if,
// throw, and the ? error-propagation operator.

package forgo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

func PostfixIf(s string) int {
	return 0 if s == ""
	fmt.Printf("%d\n", s) if len(s) > 10 // ERROR "Printf format %d has arg s of wrong type string"
	for i := range s {
		break if i > 3
		continue if i == 1
	}
	return len(s)
}

func Throw(s string) (n int, err error) {
	throw errors.New("empty") if s == ""
	throw "too long" if len(s) > 100
	if s == "x" {
		throw fmt.Errorf("bad input %q", s)
		println() // ERROR "unreachable code"
	}
	return len(s), nil
}

func Try(s string) (n int, err error) {
	n = strconv.Atoi(s)?
	_ = Throw(s)?
	if strconv.ParseBool(s)? {
		n++
	}
	f := func() (err error) {
		_ = strconv.Atoi(s)?
		return nil
	}
	f()?
	return
}

func LostCancelThrow(s string) (err error) {
	ctx, cancel := context.WithCancel(context.Background()) // ERROR "the cancel function is not used on all paths \(possible context leak\)"
	throw "empty" if s == ""                                // ERROR "this return statement may be reached without using the cancel var defined on line 52"
	defer cancel()
	_ = ctx
	return nil
}

func LostCancelTry(s string) (n int, err error) {
	ctx, cancel := context.WithCancel(context.Background()) // ERROR "the cancel function is not used on all paths \(possible context leak\)"
	n = strconv.Atoi(s)?                                    // ERROR "this return statement may be reached without using the cancel var defined on line 60"
	defer cancel()
	_ = ctx
	return
}
