// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package forgo implements forgo's compile-time execution ("comptime"
// functions) and AST macros over go/ast, for go/types and the tools built
// on it (such as cmd/vet and forgopls). It is a port of
// cmd/compile/internal/forgo, which does the same over
// cmd/compile/internal/syntax for the compiler; keep the two in sync.
// Everything but walking the syntax tree (values, Go's semantics for
// operations on them, the natively evaluated library calls) is shared
// with it in internal/fgoeval.
//
// A function marked with the //fgo:comptime directive is ordinary Go that
// is additionally interpretable by this package, so it can be invoked
// directly inside a const declaration's initializer and its result folded
// into a constant (see go/types' constDecl).
//
// A function marked with the //fgo:macro directive receives the
// *unevaluated* syntax trees of its call-site arguments (as NodeVal) and
// returns a NodeVal-wrapped syntax tree that is spliced into the caller's
// AST in place of the macro call, before type checking runs (see
// ExpandMacros). Inside a macro body, Quote(func(){ ... }) captures the
// enclosed statement/expression as an AST template without evaluating it,
// and Splice(x) marks a point in that template where the AST bound to local
// variable x should be substituted.
package forgo

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"internal/fgoeval"
	"path/filepath"
	"strings"
)

// Value is a go/constant.Value (scalar comptime evaluation), an objectVal
// or arrayVal (composite comptime evaluation, e.g. a struct/map or
// slice/array literal, or the result of json.Unmarshal), or a NodeVal
// (macro evaluation).
// Value is a go/constant.Value (scalar comptime evaluation), a struct,
// map or slice value (composite comptime evaluation), or a NodeVal (macro
// evaluation).
type Value = fgoeval.Value

// NodeVal wraps a syntax tree produced or manipulated by a macro.
type NodeVal struct {
	Node ast.Node
}

// Interp evaluates //fgo:comptime and //fgo:macro function bodies.
type Interp struct {
	// Funcs holds every //fgo:comptime/macro function declared in the
	// package, by name, so comptime/macro bodies can call one another.
	Funcs map[string]*ast.FuncDecl

	// Fset resolves positions to file names, so the "embed"
	// (comptime/embed) helpers can read files relative to the source
	// file containing the call. If nil, paths are relative to the
	// current directory.
	Fset *token.FileSet
}

// IsComptime reports whether fd is a function declaration carrying the
// //fgo:comptime directive.
func IsComptime(fd *ast.FuncDecl) bool { return hasDirective(fd, "//fgo:comptime") }

// IsMacro reports whether fd is a function declaration carrying the
// //fgo:macro directive.
func IsMacro(fd *ast.FuncDecl) bool { return hasDirective(fd, "//fgo:macro") }

func hasDirective(fd *ast.FuncDecl, directive string) bool {
	if fd.Doc == nil {
		return false
	}
	for _, c := range fd.Doc.List {
		if strings.TrimRight(c.Text, " \t\r") == directive {
			return true
		}
	}
	return false
}

type scope struct {
	parent *scope
	vars   map[string]Value
	types  map[string]string
}

func newScope(parent *scope) *scope {
	return &scope{parent: parent, vars: map[string]Value{}, types: map[string]string{}}
}

func (s *scope) get(name string) (Value, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if v, ok := cur.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func (s *scope) define(name string, v Value) { s.defineTyped(name, v, "") }

// defineTyped defines name with basic type t ("" if not a basic type).
func (s *scope) defineTyped(name string, v Value, t string) {
	s.vars[name] = v
	if t != "" {
		s.types[name] = t
	} else {
		delete(s.types, name)
	}
}

func (s *scope) typeOf(name string) string {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			return cur.types[name]
		}
	}
	return ""
}

func (s *scope) assign(name string, v Value) bool {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			cur.vars[name] = v
			return true
		}
	}
	return false
}

type (
	returnSignal   struct{ val Value }
	breakSignal    struct{}
	continueSignal struct{}
)

var fail = fgoeval.Fail

// EvalMacro runs a //fgo:macro function, binding its parameters to the
// unevaluated argument syntax trees, and returns the syntax tree the macro
// expands to.
func (in *Interp) EvalMacro(fn *ast.FuncDecl, argNodes []ast.Node) (ast.Node, error) {
	vargs := make([]Value, len(argNodes))
	for i, n := range argNodes {
		vargs[i] = NodeVal{Node: n}
	}
	v, err := in.call(fn, vargs)
	if err != nil {
		return nil, err
	}
	nv, ok := v.(NodeVal)
	if !ok {
		return nil, fmt.Errorf("macro %s did not return a quoted node", fn.Name.Name)
	}
	return nv.Node, nil
}

// EvalExprValue evaluates a self-contained expression (literals,
// operators, composite literals, and calls to //fgo:comptime functions or
// forgo's native compile-time helpers, optionally followed by field
// selection or indexing), returning whatever Value it produces: a scalar
// (constant.Value), or a struct, slice or map value. It folds a const
// declaration's initializer.
func (in *Interp) EvalExprValue(e ast.Expr) (result Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(fgoeval.Error); ok {
				err = re.Err
				return
			}
			panic(r)
		}
	}()
	return in.eval(newScope(nil), e), nil
}

// HasNative reports whether pkg.fn is one of forgo's natively evaluated
// compile-time helpers.
func (in *Interp) HasNative(pkg, fn string) bool { return fgoeval.HasNative(pkg, fn) }

// ToConstant converts a Value EvalExprValue produced into a constant.
func ToConstant(v Value) (constant.Value, bool) { return fgoeval.ToConstant(v) }

// call runs fn with args, returning its result.
func (in *Interp) call(fn *ast.FuncDecl, args []Value) (val Value, err error) {
	if fn.Body == nil {
		return nil, fmt.Errorf("%s has no body", fn.Name.Name)
	}
	defer func() {
		if r := recover(); r != nil {
			switch r := r.(type) {
			case returnSignal:
				val = r.val
				if res := fn.Type.Results; res != nil && len(res.List) == 1 {
					if cv, ok := val.(constant.Value); ok {
						val = fgoeval.Convert(cv, typeName(res.List[0].Type))
					}
				}
			case fgoeval.Error:
				err = r.Err
			default:
				panic(r)
			}
		}
	}()
	sc := newScope(nil)
	i := 0
	for _, f := range fn.Type.Params.List {
		t := typeName(f.Type)
		if len(f.Names) == 0 {
			i++
			continue
		}
		for _, n := range f.Names {
			if i < len(args) {
				v := args[i]
				if cv, ok := v.(constant.Value); ok {
					v = fgoeval.Convert(cv, t)
				}
				sc.defineTyped(n.Name, v, t)
			}
			i++
		}
	}
	in.block(sc, fn.Body)
	return nil, nil
}

func (in *Interp) block(parent *scope, b *ast.BlockStmt) {
	sc := newScope(parent)
	for _, s := range b.List {
		in.stmt(sc, s)
	}
}

func (in *Interp) stmt(sc *scope, s ast.Stmt) {
	switch x := s.(type) {
	case *ast.BlockStmt:
		in.block(sc, x)
	case *ast.EmptyStmt:
	case *ast.ExprStmt:
		in.eval(sc, x.X)
	case *ast.DeclStmt:
		gd, ok := x.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR && gd.Tok != token.CONST {
			fail("forgo: unsupported local declaration in comptime function")
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				var init ast.Expr
				if i < len(vs.Values) {
					init = vs.Values[i]
				}
				in.bind(sc, n, vs.Type, init)
			}
		}
	case *ast.AssignStmt:
		in.assign(sc, x)
	case *ast.IncDecStmt:
		op := token.ADD
		if x.Tok == token.DEC {
			op = token.SUB
		}
		in.update(sc, x.X, op, constant.MakeInt64(1))
	case *ast.ReturnStmt:
		var v Value
		if len(x.Results) == 1 {
			v = in.eval(sc, x.Results[0])
		} else if len(x.Results) > 1 {
			fail("forgo: multiple results are not supported in comptime functions")
		}
		panic(returnSignal{v})
	case *ast.IfStmt:
		child := newScope(sc)
		if x.Init != nil {
			in.stmt(child, x.Init)
		}
		if in.evalBool(child, x.Cond) {
			in.block(child, x.Body)
		} else if x.Else != nil {
			in.stmt(child, x.Else)
		}
	case *ast.ForStmt:
		child := newScope(sc)
		if x.Init != nil {
			in.stmt(child, x.Init)
		}
		for x.Cond == nil || in.evalBool(child, x.Cond) {
			if in.loopBody(child, x.Body) {
				break
			}
			if x.Post != nil {
				in.stmt(child, x.Post)
			}
		}
	case *ast.RangeStmt:
		in.rangeStmt(newScope(sc), x)
	case *ast.BranchStmt:
		if x.Label != nil {
			fail("forgo: labeled break and continue are not supported in comptime functions")
		}
		switch x.Tok {
		case token.BREAK:
			panic(breakSignal{})
		case token.CONTINUE:
			panic(continueSignal{})
		}
		fail("forgo: unsupported branch statement in comptime function")
	default:
		fail("forgo: unsupported statement %T in comptime function", s)
	}
}

func (in *Interp) loopBody(sc *scope, body *ast.BlockStmt) (broke bool) {
	defer func() {
		if r := recover(); r != nil {
			switch r.(type) {
			case breakSignal:
				broke = true
			case continueSignal:
			default:
				panic(r)
			}
		}
	}()
	in.block(sc, body)
	return false
}

func (in *Interp) rangeStmt(sc *scope, r *ast.RangeStmt) {
	set := func(e ast.Expr, v Value, t string) {
		if e == nil {
			return
		}
		n, ok := e.(*ast.Ident)
		if !ok {
			fail("forgo: range variables must be plain names in comptime functions")
		}
		if n.Name == "_" {
			return
		}
		if r.Tok == token.DEFINE {
			sc.defineTyped(n.Name, v, t)
		} else if !sc.assign(n.Name, v) {
			fail("forgo: undefined: %s", n.Name)
		}
	}
	iter := func(k Value, kt string, v Value, vt string) bool {
		set(r.Key, k, kt)
		set(r.Value, v, vt)
		return in.loopBody(sc, r.Body)
	}
	switch x := in.eval(sc, r.X).(type) {
	case constant.Value:
		switch x.Kind() {
		case constant.Int:
			n := fgoeval.ScalarInt(x, "range")
			for i := int64(0); i < n; i++ {
				if iter(constant.MakeInt64(i), "int", nil, "") {
					return
				}
			}
		case constant.String:
			for i, c := range constant.StringVal(x) {
				if iter(constant.MakeInt64(int64(i)), "int", constant.MakeInt64(int64(c)), "rune") {
					return
				}
			}
		default:
			fail("forgo: cannot range over this value in comptime functions")
		}
	case fgoeval.ArrayVal:
		for i, e := range x.Elems {
			if iter(constant.MakeInt64(int64(i)), "int", e, fgoeval.ValueType(e)) {
				return
			}
		}
	case fgoeval.ObjectVal:
		for _, k := range x.Keys {
			if iter(constant.MakeString(k), "string", x.Fields[k], fgoeval.ValueType(x.Fields[k])) {
				return
			}
		}
	default:
		fail("forgo: cannot range over this value in comptime functions")
	}
}

// bind declares n with type expression typ (nil if none) and initializer
// init (nil for the zero value).
func (in *Interp) bind(sc *scope, n *ast.Ident, typ, init ast.Expr) {
	t := typeName(typ)
	if init == nil {
		sc.defineTyped(n.Name, zeroValue(typ), t)
		return
	}
	v := in.eval(sc, init)
	if typ == nil {
		t = in.typeOf(sc, init)
	}
	if cv, ok := v.(constant.Value); ok {
		if t == "" {
			t = fgoeval.DefaultType(cv)
		}
		v = fgoeval.Convert(cv, t)
	}
	if n.Name != "_" {
		sc.defineTyped(n.Name, v, t)
	}
}

func (in *Interp) assign(sc *scope, a *ast.AssignStmt) {
	if len(a.Lhs) != 1 || len(a.Rhs) != 1 {
		if a.Tok == token.DEFINE || a.Tok == token.ASSIGN && len(a.Lhs) == len(a.Rhs) {
			vals := make([]Value, len(a.Rhs))
			for i, r := range a.Rhs {
				vals[i] = in.eval(sc, r)
			}
			for i, l := range a.Lhs {
				in.store(sc, l, vals[i], a.Tok == token.DEFINE, a.Rhs[i])
			}
			return
		}
		fail("forgo: multi-value assignments are not supported in comptime functions")
	}
	lhs, rhs := a.Lhs[0], a.Rhs[0]
	switch a.Tok {
	case token.DEFINE:
		n, ok := lhs.(*ast.Ident)
		if !ok {
			fail("forgo: cannot declare %T in comptime functions", lhs)
		}
		in.bind(sc, n, nil, rhs)
	case token.ASSIGN:
		in.store(sc, lhs, in.eval(sc, rhs), false, rhs)
	default:
		op := assignOp(a.Tok)
		in.update(sc, lhs, op, fgoeval.Scalar(in.eval(sc, rhs), "operand"))
	}
}

// store assigns v (the value of rhs) to lhs, a name or an element.
func (in *Interp) store(sc *scope, lhs ast.Expr, v Value, define bool, rhs ast.Expr) {
	switch l := lhs.(type) {
	case *ast.Ident:
		if l.Name == "_" {
			return
		}
		if define {
			t := in.typeOf(sc, rhs)
			if cv, ok := v.(constant.Value); ok {
				if t == "" {
					t = fgoeval.DefaultType(cv)
				}
				v = fgoeval.Convert(cv, t)
			}
			sc.defineTyped(l.Name, v, t)
			return
		}
		if cv, ok := v.(constant.Value); ok {
			v = fgoeval.Convert(cv, sc.typeOf(l.Name))
		}
		if !sc.assign(l.Name, v) {
			fail("forgo: undefined: %s", l.Name)
		}
	case *ast.IndexExpr:
		in.storeElem(sc, l, func(Value) Value { return v })
	default:
		fail("forgo: only assignment to a name or an element is supported in comptime functions")
	}
}

// update applies x op= y to lhs, a name or an element.
func (in *Interp) update(sc *scope, lhs ast.Expr, op token.Token, y constant.Value) {
	switch l := lhs.(type) {
	case *ast.Ident:
		cur, ok := sc.get(l.Name)
		if !ok {
			fail("forgo: undefined: %s", l.Name)
		}
		sc.assign(l.Name, fgoeval.BinaryOp(fgoeval.Scalar(cur, l.Name), op, y, sc.typeOf(l.Name)))
	case *ast.IndexExpr:
		in.storeElem(sc, l, func(cur Value) Value {
			return fgoeval.BinaryOp(fgoeval.Scalar(cur, "element"), op, y, fgoeval.ValueType(cur))
		})
	default:
		fail("forgo: only assignment to a name or an element is supported in comptime functions")
	}
}

// storeElem sets an element of a slice or a key of a map to f of its
// current value.
func (in *Interp) storeElem(sc *scope, ix *ast.IndexExpr, f func(Value) Value) {
	base := in.eval(sc, ix.X)
	key := in.eval(sc, ix.Index)
	switch b := base.(type) {
	case fgoeval.ArrayVal:
		i := fgoeval.ScalarInt(key, "index expression")
		if i < 0 || i >= int64(len(b.Elems)) {
			fail("forgo: index %d out of range in comptime evaluation (length %d)", i, len(b.Elems))
		}
		b.Elems[i] = f(b.Elems[i])
	case fgoeval.ObjectVal:
		k := fgoeval.ScalarString(key, "index expression")
		if _, ok := b.Fields[k]; !ok {
			b.Keys = append(b.Keys, k)
			if n, ok := ix.X.(*ast.Ident); ok {
				sc.assign(n.Name, b)
			}
		}
		b.Fields[k] = f(b.Fields[k])
	default:
		fail("forgo: cannot assign to an element of this value in comptime evaluation")
	}
}

func (in *Interp) evalBool(sc *scope, e ast.Expr) bool {
	cv, ok := in.eval(sc, e).(constant.Value)
	if !ok || cv.Kind() != constant.Bool {
		fail("forgo: condition is not a boolean constant")
	}
	return constant.BoolVal(cv)
}

func (in *Interp) eval(sc *scope, e ast.Expr) Value {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return in.eval(sc, x.X)
	case *ast.Ident:
		switch x.Name {
		case "true":
			return constant.MakeBool(true)
		case "false":
			return constant.MakeBool(false)
		}
		if v, ok := sc.get(x.Name); ok {
			return v
		}
		fail("forgo: undefined: %s", x.Name)
	case *ast.BasicLit:
		v := constant.MakeFromLiteral(x.Value, x.Kind, 0)
		if v.Kind() == constant.Unknown {
			fail("forgo: invalid literal %q in comptime function", x.Value)
		}
		return v
	case *ast.UnaryExpr:
		v := fgoeval.Scalar(in.eval(sc, x.X), "unary operand")
		t := in.typeOf(sc, x.X)
		switch x.Op {
		case token.SUB, token.XOR:
			return fgoeval.Convert(constant.UnaryOp(x.Op, v, 0), t)
		case token.ADD:
			return v
		case token.NOT:
			return constant.MakeBool(!constant.BoolVal(v))
		}
		fail("forgo: unsupported unary operator in comptime function")
	case *ast.BinaryExpr:
		switch x.Op {
		case token.LAND:
			return constant.MakeBool(in.evalBool(sc, x.X) && in.evalBool(sc, x.Y))
		case token.LOR:
			return constant.MakeBool(in.evalBool(sc, x.X) || in.evalBool(sc, x.Y))
		}
		a := fgoeval.Scalar(in.eval(sc, x.X), "operand")
		b := fgoeval.Scalar(in.eval(sc, x.Y), "operand")
		switch x.Op {
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
			return constant.MakeBool(constant.Compare(a, x.Op, b))
		}
		return fgoeval.BinaryOp(a, x.Op, b, in.typeOf(sc, x))
	case *ast.CallExpr:
		return in.evalCall(sc, x)
	case *ast.CompositeLit:
		return in.compositeLit(sc, x)
	case *ast.SelectorExpr:
		obj, ok := in.eval(sc, x.X).(fgoeval.ObjectVal)
		if !ok {
			fail("forgo: cannot select field %q from a non-struct/map value in comptime evaluation", x.Sel.Name)
		}
		v, ok := obj.Fields[x.Sel.Name]
		if !ok {
			fail("forgo: no field %q in comptime evaluation", x.Sel.Name)
		}
		return v
	case *ast.SliceExpr:
		return in.slice(sc, x)
	case *ast.IndexExpr:
		switch b := in.eval(sc, x.X).(type) {
		case constant.Value:
			if b.Kind() == constant.String {
				s := constant.StringVal(b)
				i := fgoeval.ScalarInt(in.eval(sc, x.Index), "index expression")
				if i < 0 || i >= int64(len(s)) {
					fail("forgo: index %d out of range in comptime evaluation (length %d)", i, len(s))
				}
				return constant.MakeInt64(int64(s[i]))
			}
		case fgoeval.ArrayVal:
			i := fgoeval.ScalarInt(in.eval(sc, x.Index), "index expression")
			if i < 0 || i >= int64(len(b.Elems)) {
				fail("forgo: index %d out of range in comptime evaluation (length %d)", i, len(b.Elems))
			}
			return b.Elems[i]
		case fgoeval.ObjectVal:
			k := fgoeval.ScalarString(in.eval(sc, x.Index), "index expression")
			v, ok := b.Fields[k]
			if !ok {
				fail("forgo: no key %q in comptime evaluation", k)
			}
			return v
		}
		fail("forgo: cannot index a scalar value in comptime evaluation")
	}
	fail("forgo: unsupported expression %T in comptime function", e)
	panic("unreachable")
}

// compositeLit evaluates a composite literal: unkeyed elements make a
// slice, keyed ones a struct or map (see the compiler's evalCompositeLit).
func (in *Interp) compositeLit(sc *scope, lit *ast.CompositeLit) Value {
	keyed := 0
	for _, e := range lit.Elts {
		if _, ok := e.(*ast.KeyValueExpr); ok {
			keyed++
		}
	}
	if keyed == 0 {
		elems := make([]Value, len(lit.Elts))
		for i, e := range lit.Elts {
			elems[i] = in.eval(sc, e)
		}
		return fgoeval.ArrayVal{Elems: elems}
	}
	if keyed != len(lit.Elts) {
		fail("forgo: mixed keyed and unkeyed composite literal elements are not supported in comptime evaluation")
	}
	obj := fgoeval.ObjectVal{Fields: map[string]Value{}}
	for _, e := range lit.Elts {
		kv := e.(*ast.KeyValueExpr)
		var key string
		if n, ok := kv.Key.(*ast.Ident); ok {
			if _, bound := sc.get(n.Name); !bound {
				key = n.Name
			}
		}
		if key == "" {
			key = fgoeval.ScalarString(in.eval(sc, kv.Key), "composite literal key")
		}
		if _, dup := obj.Fields[key]; !dup {
			obj.Keys = append(obj.Keys, key)
		}
		obj.Fields[key] = in.eval(sc, kv.Value)
	}
	return obj
}

func (in *Interp) slice(sc *scope, x *ast.SliceExpr) Value {
	base := in.eval(sc, x.X)
	idx := [3]int64{0, -1, -1}
	for i, e := range []ast.Expr{x.Low, x.High, x.Max} {
		if e != nil {
			idx[i] = fgoeval.ScalarInt(in.eval(sc, e), "slice index")
		}
	}
	lo, hi, max := idx[0], idx[1], idx[2]
	switch b := base.(type) {
	case constant.Value:
		if b.Kind() != constant.String {
			break
		}
		s := constant.StringVal(b)
		if hi < 0 {
			hi = int64(len(s))
		}
		if lo < 0 || hi < lo || hi > int64(len(s)) {
			fail("forgo: slice bounds [%d:%d] out of range in comptime evaluation (length %d)", lo, hi, len(s))
		}
		return constant.MakeString(s[lo:hi])
	case fgoeval.ArrayVal:
		n := int64(cap(b.Elems))
		if hi < 0 {
			hi = int64(len(b.Elems))
		}
		if max < 0 {
			max = n
		}
		if lo < 0 || hi < lo || max < hi || max > n {
			fail("forgo: slice bounds [%d:%d:%d] out of range in comptime evaluation (capacity %d)", lo, hi, max, n)
		}
		return fgoeval.ArrayVal{Elems: b.Elems[lo:hi:max]}
	}
	fail("forgo: cannot slice this value in comptime evaluation")
	panic("unreachable")
}

func (in *Interp) evalCall(sc *scope, call *ast.CallExpr) Value {
	if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "Quote" {
		return in.evalQuote(sc, call)
	}
	fun := ast.Unparen(call.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = ix.X
	case *ast.IndexListExpr:
		fun = ix.X
	}
	if sel, ok := fun.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok && fgoeval.HasNative(pkg.Name, sel.Sel.Name) {
			args := make([]Value, len(call.Args))
			for i, a := range call.Args {
				args[i] = in.eval(sc, a)
			}
			dir := ""
			if in.Fset != nil {
				dir = filepath.Dir(in.Fset.Position(call.Pos()).Filename)
			}
			v, _ := fgoeval.NativeCall(dir, pkg.Name, sel.Sel.Name, args)
			return v
		}
	}
	if at, ok := fun.(*ast.ArrayType); ok && at.Len == nil && len(call.Args) == 1 {
		// []byte(s)
		if n, ok := at.Elt.(*ast.Ident); ok && (n.Name == "byte" || n.Name == "uint8") {
			s := fgoeval.ScalarString(in.eval(sc, call.Args[0]), "[]byte conversion")
			elems := make([]Value, len(s))
			for i := range len(s) {
				elems[i] = constant.MakeInt64(int64(s[i]))
			}
			return fgoeval.ArrayVal{Elems: elems}
		}
	}
	name, ok := fun.(*ast.Ident)
	if !ok {
		fail("forgo: unsupported function call in comptime function")
	}
	if _, shadowed := sc.get(name.Name); !shadowed {
		if v, ok := in.builtin(sc, name.Name, call); ok {
			return v
		}
	}
	fn, ok := in.Funcs[name.Name]
	if !ok {
		fail("forgo: %s is not a //fgo:comptime function and cannot be called at compile time", name.Name)
	}
	args := make([]Value, len(call.Args))
	for i, a := range call.Args {
		args[i] = in.eval(sc, a)
	}
	v, err := in.call(fn, args)
	if err != nil {
		fail("%s", err.Error())
	}
	return v
}

// builtin evaluates len, cap, append, make, or a conversion to a basic
// type, reporting whether call was one.
func (in *Interp) builtin(sc *scope, name string, call *ast.CallExpr) (Value, bool) {
	if fgoeval.IsBasicType(name) && len(call.Args) == 1 {
		return fgoeval.Convert(fgoeval.Scalar(in.eval(sc, call.Args[0]), name+" conversion"), name), true
	}
	switch name {
	case "len", "cap":
		if len(call.Args) != 1 {
			fail("forgo: %s takes one argument", name)
		}
		switch v := in.eval(sc, call.Args[0]).(type) {
		case constant.Value:
			if v.Kind() == constant.String {
				return constant.MakeInt64(int64(len(constant.StringVal(v)))), true
			}
		case fgoeval.ArrayVal:
			if name == "cap" {
				return constant.MakeInt64(int64(cap(v.Elems))), true
			}
			return constant.MakeInt64(int64(len(v.Elems))), true
		case fgoeval.ObjectVal:
			return constant.MakeInt64(int64(len(v.Fields))), true
		}
		fail("forgo: invalid argument to %s in comptime evaluation", name)
	case "append":
		if len(call.Args) == 0 {
			fail("forgo: append needs a slice")
		}
		var elems []Value
		switch v := in.eval(sc, call.Args[0]).(type) {
		case fgoeval.ArrayVal:
			elems = v.Elems
		case constant.Value:
			if v.Kind() != constant.Unknown {
				fail("forgo: first argument to append must be a slice")
			}
		default:
			fail("forgo: first argument to append must be a slice")
		}
		rest := call.Args[1:]
		if call.Ellipsis.IsValid() {
			switch v := in.eval(sc, rest[0]).(type) {
			case fgoeval.ArrayVal:
				return fgoeval.ArrayVal{Elems: append(elems, v.Elems...)}, true
			case constant.Value:
				if v.Kind() == constant.String {
					for _, c := range []byte(constant.StringVal(v)) {
						elems = append(elems, constant.MakeInt64(int64(c)))
					}
					return fgoeval.ArrayVal{Elems: elems}, true
				}
			}
			fail("forgo: cannot append this value in comptime evaluation")
		}
		for _, a := range rest {
			elems = append(elems, in.eval(sc, a))
		}
		return fgoeval.ArrayVal{Elems: elems}, true
	case "make":
		if len(call.Args) < 2 {
			fail("forgo: make needs a slice type and a length in comptime evaluation")
		}
		at, ok := call.Args[0].(*ast.ArrayType)
		if !ok || at.Len != nil {
			fail("forgo: only slices can be made in comptime evaluation")
		}
		n := fgoeval.ScalarInt(in.eval(sc, call.Args[1]), "make")
		c := n
		if len(call.Args) > 2 {
			c = fgoeval.ScalarInt(in.eval(sc, call.Args[2]), "make")
		}
		if n < 0 || c < n || c > 1<<24 {
			fail("forgo: invalid make length or capacity in comptime evaluation")
		}
		elems := make([]Value, n, c)
		for i := range elems {
			elems[i] = zeroValue(at.Elt)
		}
		return fgoeval.ArrayVal{Elems: elems}, true
	}
	return nil, false
}

// typeOf infers the basic type of e (see the compiler's Interp.typeOf).
func (in *Interp) typeOf(sc *scope, e ast.Expr) string {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return in.typeOf(sc, x.X)
	case *ast.Ident:
		return sc.typeOf(x.Name)
	case *ast.UnaryExpr:
		if x.Op == token.NOT {
			return "bool"
		}
		return in.typeOf(sc, x.X)
	case *ast.BinaryExpr:
		switch x.Op {
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ, token.LAND, token.LOR:
			return "bool"
		case token.SHL, token.SHR:
			return in.typeOf(sc, x.X)
		}
		if t := in.typeOf(sc, x.X); t != "" {
			return t
		}
		return in.typeOf(sc, x.Y)
	case *ast.IndexExpr:
		if n, ok := x.X.(*ast.Ident); ok {
			if v, ok := sc.get(n.Name); ok {
				if cv, ok := v.(constant.Value); ok && cv.Kind() == constant.String {
					return "byte"
				}
			}
		} else if lit, ok := x.X.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			return "byte"
		}
	case *ast.SliceExpr:
		return in.typeOf(sc, x.X)
	case *ast.CallExpr:
		if n, ok := ast.Unparen(x.Fun).(*ast.Ident); ok {
			if fgoeval.IsBasicType(n.Name) {
				return n.Name
			}
			switch n.Name {
			case "len", "cap":
				return "int"
			}
			if fn, ok := in.Funcs[n.Name]; ok && fn.Type.Results != nil && len(fn.Type.Results.List) == 1 {
				return typeName(fn.Type.Results.List[0].Type)
			}
		}
	}
	return ""
}

func typeName(e ast.Expr) string {
	if n, ok := e.(*ast.Ident); ok && fgoeval.IsBasicType(n.Name) {
		return n.Name
	}
	return ""
}

func zeroValue(typ ast.Expr) Value {
	switch t := typ.(type) {
	case *ast.ArrayType:
		if t.Len == nil {
			return fgoeval.ArrayVal{}
		}
	case *ast.MapType:
		return fgoeval.ObjectVal{Fields: map[string]Value{}}
	case *ast.Ident:
		switch {
		case fgoeval.IsIntType(t.Name):
			return constant.MakeInt64(0)
		case fgoeval.IsFloatType(t.Name):
			return constant.MakeFloat64(0)
		case t.Name == "string":
			return constant.MakeString("")
		case t.Name == "bool":
			return constant.MakeBool(false)
		}
	}
	return constant.MakeUnknown()
}

// assignOp returns the operator of assignment operator tok (+= etc.).
func assignOp(tok token.Token) token.Token {
	switch tok {
	case token.ADD_ASSIGN:
		return token.ADD
	case token.SUB_ASSIGN:
		return token.SUB
	case token.MUL_ASSIGN:
		return token.MUL
	case token.QUO_ASSIGN:
		return token.QUO
	case token.REM_ASSIGN:
		return token.REM
	case token.AND_ASSIGN:
		return token.AND
	case token.OR_ASSIGN:
		return token.OR
	case token.XOR_ASSIGN:
		return token.XOR
	case token.SHL_ASSIGN:
		return token.SHL
	case token.SHR_ASSIGN:
		return token.SHR
	case token.AND_NOT_ASSIGN:
		return token.AND_NOT
	}
	fail("forgo: unsupported assignment operator %s in comptime function", tok)
	panic("unreachable")
}
