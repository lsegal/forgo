// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types_test

import (
	"go/constant"
	"internal/testenv"
	"strings"
	"testing"

	. "go/types"
)

// TestForgoComptimeConst checks that go/types folds //fgo:comptime calls in
// const initializers the way the compiler's types2 does, including
// composite (struct/slice) results and field/index access on them.
func TestForgoComptimeConst(t *testing.T) {
	const src = `
package p

type Config struct {
	Name  string
	Ports []int
}

//fgo:comptime
func square(n int) int {
	r := 0
	for i := 0; i < n; i++ {
		r += n
	}
	return r
}

//fgo:comptime
func config(name string) Config {
	return Config{Name: name, Ports: []int{80, square(3)}}
}

const nine = square(3)
const cfg = config("web")
const name = cfg.Name
const port = cfg.Ports[1]

var a [nine]byte
var b [cfg.Ports[1] + 1]byte
`
	pkg := mustTypecheck(src, nil, nil)

	for _, test := range []struct {
		name string
		want string
	}{
		{"nine", "9"},
		{"name", `"web"`},
		{"port", "9"},
	} {
		c, ok := pkg.Scope().Lookup(test.name).(*Const)
		if !ok {
			t.Errorf("%s is not a constant", test.name)
			continue
		}
		if got := c.Val().ExactString(); got != test.want {
			t.Errorf("%s = %s, want %s", test.name, got, test.want)
		}
	}
	if c := pkg.Scope().Lookup("cfg").(*Const); c.Val().Kind() != constant.Composite {
		t.Errorf("cfg has kind %v, want Composite", c.Val().Kind())
	}
	for name, want := range map[string]string{"a": "[9]byte", "b": "[10]byte"} {
		if got := pkg.Scope().Lookup(name).Type().String(); got != want {
			t.Errorf("type of %s = %s, want %s", name, got, want)
		}
	}
}

// TestForgoComptimeConstError checks that a failing compile-time
// evaluation is reported rather than silently producing a constant.
func TestForgoComptimeConstError(t *testing.T) {
	const src = `
package p

//fgo:comptime
func at(i int) int {
	return []int{1, 2}[i]
}

const x = at(5)
`
	_, err := typecheck(src, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "compile-time evaluation failed") {
		t.Errorf("got error %v, want a compile-time evaluation failure", err)
	}
}

// TestForgoThrow checks that throw "..." counts as a use of the file's
// "errors" import, since it lowers to errors.New, and that throw and
// postfix if are handled by the terminating-statement analysis.
func TestForgoThrow(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	const src = `
package p

import "errors"

func f(s string) (err error) {
	throw "empty" if s == ""
	return
}

// A trailing throw terminates the function like a return.
func g() (n int, err error) {
	throw "always"
}
`
	mustTypecheck(src, nil, nil)

	// A postfix-if break leaves the loop, so h is missing a return.
	const breakIf = `
package p

func h() (n int) {
	for {
		break if n > 3
		n++
	}
}
`
	_, err := typecheck(breakIf, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "missing return") {
		t.Errorf("got error %v, want missing return", err)
	}

	const noImport = `
package p

func f() (err error) {
	throw "empty"
}
`
	_, err = typecheck(noImport, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `requires this file to already import "errors"`) {
		t.Errorf("got error %v, want a missing errors import error", err)
	}
}
