// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package forgo

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestExpandMacros(t *testing.T) {
	const src = `package p

//fgo:macro
func twice(x Node) Node {
	return Quote(func() {
		Splice(x) + Splice(x)
	})
}

//fgo:macro
func inc(x Node) Node {
	return Quote(func() {
		Splice(x)++
	})
}

func F(n int) int {
	inc(n)
	if twice(n) > 2 {
		return twice(twice(n))
	}
	return 0
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if err := ExpandMacros(fset, []*ast.File{f}); err != nil {
		t.Fatal(err)
	}
	if len(f.Decls) != 1 {
		t.Fatalf("got %d declarations, want only F (macros removed)", len(f.Decls))
	}
	var buf strings.Builder
	if err := format.Node(&buf, fset, f.Decls[0].(*ast.FuncDecl).Body); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(buf.String()), " ")
	want := "{ n++ if n+n > 2 { return n + n + (n + n) } return 0 }"
	if got != want {
		t.Errorf("expanded body:\n%s\nwant:\n%s", got, want)
	}
}

func TestExpandMacrosError(t *testing.T) {
	const src = `package p

//fgo:macro
func bad(x Node) Node {
	return 1
}

func F() { bad(1) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	err = ExpandMacros(fset, []*ast.File{f})
	if err == nil || !strings.Contains(err.Error(), "p.go:8:12: macro bad: macro bad did not return a quoted node") {
		t.Errorf("got error %v, want a macro evaluation error", err)
	}
}
