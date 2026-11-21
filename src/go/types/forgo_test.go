// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types_test

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
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

// TestForgoArgCompositeLit checks that an untyped composite literal call
// argument {...} or &{...} records the type it takes from its parameter,
// which is what the compiler's backend uses to build the literal.
func TestForgoArgCompositeLit(t *testing.T) {
	const src = `
package p

type S struct{ A int }

func val(S)  {}
func ptr(*S) {}

func _() {
	val({A: 1})
	ptr({A: 2})
	ptr(&{A: 3})
}
`
	info := &Info{Types: make(map[ast.Expr]TypeAndValue)}
	mustTypecheck(src, nil, info)

	want := map[string]string{"1": "p.S", "2": "*p.S", "3": "p.S"}
	got := make(map[string]string)
	for e, tv := range info.Types {
		lit, _ := e.(*ast.CompositeLit)
		if lit == nil || len(lit.Elts) != 1 {
			continue
		}
		v := lit.Elts[0].(*ast.KeyValueExpr).Value.(*ast.BasicLit).Value
		got[v] = tv.Type.String()
	}
	for v, typ := range want {
		if got[v] != typ {
			t.Errorf("type of literal {A: %s} = %q, want %q", v, got[v], typ)
		}
	}
}

// TestForgoComptimeSemantics checks Go's integer semantics, slices and
// composite constants in go/types' comptime folding.
func TestForgoComptimeSemantics(t *testing.T) {
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

// TestForgoEnumInfo checks what go/types records for enum code, which
// go/types-based tools rely on: a variant selector's Sel uses a function
// (the constructor) or a variable (a unit variant) that is shared by
// every use and positioned at the variant's declaration, a constructor
// call has the enum type, and a type switch case has the type of the
// switch variable in that case.
func TestForgoEnumInfo(t *testing.T) {
	const src = `
package p

type Shape enum {
	Circle(radius float64)
	Empty
}

type Option[T any] enum { Some(T); None }

func f(s Shape) {
	c := Shape.Circle(1)
	e := Shape.Empty
	o := Option.Some("x")
	_, _, _ = c, e, o
	switch v := s.(type) {
	case Shape.Circle:
		_ = v.radius
	case Shape.Empty:
		_ = v
	}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &Info{
		Types: make(map[ast.Expr]TypeAndValue),
		Defs:  make(map[*ast.Ident]Object),
		Uses:  make(map[*ast.Ident]Object),
	}
	if _, err := (&Config{}).Check("p", fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}

	uses := make(map[string][]Object) // variant name -> objects used
	for id, obj := range info.Uses {
		switch id.Name {
		case "Circle", "Empty", "Some":
			uses[id.Name] = append(uses[id.Name], obj)
		}
	}
	for _, name := range []string{"Circle", "Empty", "Some"} {
		objs := uses[name]
		if len(objs) == 0 {
			t.Errorf("no use of %s recorded", name)
			continue
		}
		for _, obj := range objs[1:] {
			if obj != objs[0] {
				t.Errorf("uses of %s record different objects %v and %v", name, objs[0], obj)
			}
		}
		if line := fset.Position(objs[0].Pos()).Line; line != map[string]int{"Circle": 5, "Empty": 6, "Some": 9}[name] {
			t.Errorf("%s is declared at line %d", name, line)
		}
	}
	if fn, ok := uses["Circle"][0].(*Func); !ok || fn.Type().String() != "func(radius float64) p.Shape" {
		t.Errorf("Circle uses %v, want the constructor func(radius float64) p.Shape", uses["Circle"][0])
	}
	if v, ok := uses["Empty"][0].(*Var); !ok || v.Type().String() != "p.Shape" {
		t.Errorf("Empty uses %v, want a variable of type p.Shape", uses["Empty"][0])
	}
	if fn, ok := uses["Some"][0].(*Func); !ok || fn.Type().(*Signature).TypeParams().Len() != 1 {
		t.Errorf("Some uses %v, want a generic constructor", uses["Some"][0])
	}

	want := map[string]string{
		`Shape.Circle(1)`:   "p.Shape",
		`Shape.Empty`:       "p.Shape",
		`Option.Some("x")`:  "p.Option[string]",
		`v.radius`:          "float64",
		`Shape.Circle`:      "func(radius float64) p.Shape",
		`case Shape.Circle`: "struct{radius float64}",
		`case Shape.Empty`:  "p.Shape",
	}
	got := make(map[string]string)
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CaseClause:
			for _, e := range n.List {
				tv := info.Types[e]
				if !tv.IsType() {
					t.Errorf("case %s is not recorded as a type", ExprString(e))
				}
				got["case "+ExprString(e)] = tv.Type.String()
			}
			return true
		case ast.Expr:
			if tv, ok := info.Types[n]; ok {
				if _, seen := got[ExprString(n)]; !seen {
					got[ExprString(n)] = tv.Type.String()
				}
			}
		}
		return true
	})
	for expr, typ := range want {
		if got[expr] != typ {
			t.Errorf("type of %s = %q, want %q", expr, got[expr], typ)
		}
	}
}
