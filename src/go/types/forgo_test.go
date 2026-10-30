// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types_test

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	. "go/types"
	"testing"
)

// TestForgoComptime checks that go/types folds //fgo:comptime calls in
// const declarations, as the compiler does.
func TestForgoComptime(t *testing.T) {
	const src = `package p

//fgo:comptime
func xorshift(s uint64) uint64 {
	s ^= s << 13
	s ^= s >> 7
	s ^= s << 17
	return s
}

//fgo:comptime
func words(n int) []string {
	var out []string
	s := uint64(5)
	for i := range n {
		s = xorshift(s)
		out = append(out, "w"+string(rune('a'+s%26))+string(rune('0'+i/2)))
	}
	return out
}

//fgo:comptime
func label(xs []string) string { return xs[1][:2] }

type Point struct{ X, Y int }

//fgo:comptime
func origin() Point { return Point{X: 3 / 2, Y: -1} }

const (
	ws    = words(3)
	n     = len(ws)
	first = ws[0]
	lbl   = label(words(2))
	two   = xorshift(xorshift(5))
	ox    = origin().X
	o     = origin()
	oy    = o.Y
)

var arr [n]int
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var conf Config
	info := &Info{Defs: map[*ast.Ident]Object{}}
	pkg, err := conf.Check("p", fset, []*ast.File{f}, info)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"first": `"wf0"`,
		"lbl":   `"wd"`,
		"two":   "5764964994168997191",
		"ox":    "1",
		"oy":    "-1",
	}
	for name, w := range want {
		c, ok := pkg.Scope().Lookup(name).(*Const)
		if !ok {
			t.Fatalf("%s is not a constant", name)
		}
		if got := c.Val().ExactString(); got != w {
			t.Errorf("%s = %s, want %s", name, got, w)
		}
	}
	if c := pkg.Scope().Lookup("ws").(*Const); c.Val().Kind() != constant.Composite || len(constant.CompositeElems(c.Val())) != 3 {
		t.Errorf("ws = %v, want a 3-element composite", c.Val())
	}
}
