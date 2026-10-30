// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package forgo

import (
	"go/constant"
	"strings"
	"testing"

	"cmd/compile/internal/syntax"
)

// evalConst parses src, a file whose functions are all comptime, and
// evaluates the initializer of its const named result.
func evalConst(t *testing.T, src string) (Value, error) {
	t.Helper()
	f, err := syntax.Parse(syntax.NewFileBase("x.go"), strings.NewReader("package p\n"+src), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	in := &Interp{Funcs: map[string]*syntax.FuncDecl{}}
	var init syntax.Expr
	for _, d := range f.DeclList {
		switch d := d.(type) {
		case *syntax.FuncDecl:
			in.Funcs[d.Name.Value] = d
		case *syntax.ConstDecl:
			if d.NameList[0].Value == "result" {
				init = d.Values
			}
		}
	}
	if init == nil {
		t.Fatal("no const result")
	}
	return in.EvalExprValue(init)
}

func TestComptime(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"integer division truncates", `
func mid(lo, hi int) int { return (lo + hi) / 2 }
const result = mid(0, 3)`, "1"},
		{"untyped division truncates", `
func f() int { x := 7; return x / 2 }
const result = f()`, "3"},
		{"float division", `
func f() float64 { x := 7.0; return x / 2 }
const result = f()`, "3.5"},
		{"uint64 xorshift wraps", `
func next(s uint64) uint64 {
	s ^= s << 13
	s ^= s >> 7
	s ^= s << 17
	return s
}
const result = next(next(5))`, "5764964994168997191"},
		{"int8 wraps", `
func f() int8 { var x int8 = 127; x++; return x }
const result = f()`, "-128"},
		{"byte conversion", `
func f() byte { return byte(300) }
const result = f()`, "44"},
		{"recursion over halves", `
func count(lo, hi int) int {
	if hi-lo <= 1 {
		return hi - lo
	}
	mid := (lo + hi) / 2
	return count(lo, mid) + count(mid, hi)
}
const result = count(0, 1001)`, "1001"},
		{"string slicing and indexing", `
func f(s string) string { return s[2:4] + string(s[0]) }
const result = f("kalomi")`, `"lok"`},
		{"range, break, continue", `
func f() int {
	n := 0
	for i := range 10 {
		if i%2 == 1 {
			continue
		}
		if i > 6 {
			break
		}
		n += i
	}
	for _, c := range "abc" {
		n += int(c)
	}
	return n
}
const result = f()`, "306"},
		{"slices", `
func words(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, "w"+string(rune('a'+i)))
	}
	out[0] = "first"
	return append(out[:2], out[n-1:]...)
}
const result = words(5)`, `{"first", "wb", "we"}`},
		{"make and len", `
func f() int {
	s := make([]int, 3)
	s[1] = 7
	s[2] += 2
	return len(s) + s[1] + s[2]
}
const result = f()`, "12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := evalConst(t, tt.src)
			if err != nil {
				t.Fatal(err)
			}
			cv, ok := ToConstant(v)
			if !ok {
				t.Fatalf("not a constant: %v", v)
			}
			if got := show(cv); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// show prints v, listing a composite's elements.
func show(v constant.Value) string {
	if v.Kind() == constant.Composite {
		var parts []string
		for _, e := range constant.CompositeElems(v) {
			parts = append(parts, show(e))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	if v.Kind() == constant.Float {
		return v.String()
	}
	return v.ExactString()
}
