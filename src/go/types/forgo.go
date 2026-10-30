// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements forgo's //fgo:comptime const folding for go/types,
// so that tools built on it (forgopls, vet) see the same constants the
// compiler does. It is the go/ast counterpart of
// cmd/compile/internal/types2/forgo.go and the interpreter in
// cmd/compile/internal/forgo: the two walk different syntax trees and
// share everything else (values, Go's semantics for operations on them,
// natively evaluated library calls) through internal/fgoeval. Keep them
// in step.
package types

import (
	"go/ast"
	"go/constant"
	"go/token"
	"internal/fgoeval"
	"path/filepath"
	"strings"

	. "internal/types/errors"
)

// forgoEvalConstCall tries to evaluate init as a compile-time expression:
// a call to a //fgo:comptime function declared in this package, a call to
// one of forgo's native compile-time helpers (e.g. embed.ReadFile(...),
// json.Unmarshal[Config](...)), or a chain of field selection or indexing
// on top of one. It reports whether it handled init at all; x is only
// updated to a constant operand on success.
func forgoEvalConstCall(check *Checker, x *operand, init ast.Expr) bool {
	call := forgoRootCall(init)
	if call == nil {
		return false
	}
	in := forgoInterpreter(check)
	fun := ast.Unparen(call.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = ix.X
	case *ast.IndexListExpr:
		fun = ix.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		if _, ok := in.funcs[f.Name]; !ok {
			return false
		}
	case *ast.SelectorExpr:
		pkg, ok := f.X.(*ast.Ident)
		if !ok || !fgoeval.HasNative(pkg.Name, f.Sel.Name) {
			return false
		}
	default:
		return false
	}

	if cv, ok := in.cache[init]; ok {
		x.mode_ = constant_
		x.val = cv
		return true
	}
	v, err := in.evalTop(init)
	if err != nil {
		check.errorf(init, InvalidConstInit, "compile-time evaluation failed: %s", err)
		x.mode_ = invalid
		return true
	}
	cv, ok := fgoeval.ToConstant(v)
	if !ok {
		check.error(init, InvalidConstInit, "compile-time evaluation did not produce a usable constant value")
		x.mode_ = invalid
		return true
	}
	in.cache[init] = cv
	x.mode_ = constant_
	x.val = cv
	return true
}

// forgoRootCall finds the call under a chain of field selection and
// indexing, or returns nil.
func forgoRootCall(e ast.Expr) *ast.CallExpr {
	for {
		switch x := ast.Unparen(e).(type) {
		case *ast.CallExpr:
			return x
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		default:
			return nil
		}
	}
}

// forgoInterp evaluates //fgo:comptime functions, declared in a package's
// files with that directive in their doc comments.
type forgoInterp struct {
	fset  *token.FileSet
	funcs map[string]*ast.FuncDecl
	cache map[ast.Expr]constant.Value
}

func forgoInterpreter(check *Checker) *forgoInterp {
	if in := check.forgo; in != nil {
		return in
	}
	in := &forgoInterp{fset: check.fset, funcs: map[string]*ast.FuncDecl{}, cache: map[ast.Expr]constant.Value{}}
	for _, file := range check.files {
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Doc == nil {
				continue
			}
			for _, c := range fd.Doc.List {
				if strings.TrimSpace(c.Text) == "//fgo:comptime" {
					in.funcs[fd.Name.Name] = fd
				}
			}
		}
	}
	check.forgo = in
	return in
}

type forgoScope struct {
	parent *forgoScope
	vars   map[string]fgoeval.Value
	types  map[string]string
}

func newForgoScope(parent *forgoScope) *forgoScope {
	return &forgoScope{parent: parent, vars: map[string]fgoeval.Value{}, types: map[string]string{}}
}

func (s *forgoScope) get(name string) (fgoeval.Value, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if v, ok := cur.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func (s *forgoScope) define(name string, v fgoeval.Value, t string) {
	s.vars[name] = v
	if t != "" {
		s.types[name] = t
	} else {
		delete(s.types, name)
	}
}

func (s *forgoScope) typeOf(name string) string {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			return cur.types[name]
		}
	}
	return ""
}

func (s *forgoScope) assign(name string, v fgoeval.Value) bool {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			cur.vars[name] = v
			return true
		}
	}
	return false
}

type (
	forgoReturn   struct{ val fgoeval.Value }
	forgoBreak    struct{}
	forgoContinue struct{}
)

var forgoFail = fgoeval.Fail

func (in *forgoInterp) evalTop(e ast.Expr) (v fgoeval.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(fgoeval.Error); ok {
				err = re.Err
				return
			}
			panic(r)
		}
	}()
	return in.eval(newForgoScope(nil), e), nil
}

func (in *forgoInterp) call(fn *ast.FuncDecl, args []fgoeval.Value) (val fgoeval.Value) {
	if fn.Body == nil {
		forgoFail("%s has no body", fn.Name.Name)
	}
	defer func() {
		if r := recover(); r != nil {
			rs, ok := r.(forgoReturn)
			if !ok {
				panic(r)
			}
			val = rs.val
			if res := fn.Type.Results; res != nil && len(res.List) == 1 {
				if cv, ok := val.(constant.Value); ok {
					val = fgoeval.Convert(cv, forgoTypeName(res.List[0].Type))
				}
			}
		}
	}()
	sc := newForgoScope(nil)
	i := 0
	for _, f := range fn.Type.Params.List {
		t := forgoTypeName(f.Type)
		names := f.Names
		if len(names) == 0 {
			i++
			continue
		}
		for _, n := range names {
			if i < len(args) {
				v := args[i]
				if cv, ok := v.(constant.Value); ok {
					v = fgoeval.Convert(cv, t)
				}
				sc.define(n.Name, v, t)
			}
			i++
		}
	}
	in.block(sc, fn.Body)
	return nil
}

func (in *forgoInterp) block(parent *forgoScope, b *ast.BlockStmt) {
	sc := newForgoScope(parent)
	for _, s := range b.List {
		in.stmt(sc, s)
	}
}

func (in *forgoInterp) stmt(sc *forgoScope, s ast.Stmt) {
	switch x := s.(type) {
	case *ast.BlockStmt:
		in.block(sc, x)
	case *ast.EmptyStmt:
	case *ast.ExprStmt:
		in.eval(sc, x.X)
	case *ast.DeclStmt:
		gd, ok := x.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR && gd.Tok != token.CONST {
			forgoFail("forgo: unsupported local declaration in comptime function")
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
		var v fgoeval.Value
		if len(x.Results) == 1 {
			v = in.eval(sc, x.Results[0])
		} else if len(x.Results) > 1 {
			forgoFail("forgo: multiple results are not supported in comptime functions")
		}
		panic(forgoReturn{v})
	case *ast.IfStmt:
		child := newForgoScope(sc)
		if x.Init != nil {
			in.stmt(child, x.Init)
		}
		if in.evalBool(child, x.Cond) {
			in.block(child, x.Body)
		} else if x.Else != nil {
			in.stmt(child, x.Else)
		}
	case *ast.ForStmt:
		child := newForgoScope(sc)
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
		in.rangeStmt(newForgoScope(sc), x)
	case *ast.BranchStmt:
		if x.Label != nil {
			forgoFail("forgo: labeled break and continue are not supported in comptime functions")
		}
		switch x.Tok {
		case token.BREAK:
			panic(forgoBreak{})
		case token.CONTINUE:
			panic(forgoContinue{})
		}
		forgoFail("forgo: unsupported branch statement in comptime function")
	default:
		forgoFail("forgo: unsupported statement %T in comptime function", s)
	}
}

func (in *forgoInterp) loopBody(sc *forgoScope, body *ast.BlockStmt) (broke bool) {
	defer func() {
		if r := recover(); r != nil {
			switch r.(type) {
			case forgoBreak:
				broke = true
			case forgoContinue:
			default:
				panic(r)
			}
		}
	}()
	in.block(sc, body)
	return false
}

func (in *forgoInterp) rangeStmt(sc *forgoScope, r *ast.RangeStmt) {
	set := func(e ast.Expr, v fgoeval.Value, t string) {
		if e == nil {
			return
		}
		n, ok := e.(*ast.Ident)
		if !ok {
			forgoFail("forgo: range variables must be plain names in comptime functions")
		}
		if n.Name == "_" {
			return
		}
		if r.Tok == token.DEFINE {
			sc.define(n.Name, v, t)
		} else if !sc.assign(n.Name, v) {
			forgoFail("forgo: undefined: %s", n.Name)
		}
	}
	iter := func(k fgoeval.Value, kt string, v fgoeval.Value, vt string) bool {
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
			forgoFail("forgo: cannot range over this value in comptime functions")
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
		forgoFail("forgo: cannot range over this value in comptime functions")
	}
}

// bind declares n with type expression typ (nil if none) and initializer
// init (nil for the zero value).
func (in *forgoInterp) bind(sc *forgoScope, n *ast.Ident, typ, init ast.Expr) {
	t := forgoTypeName(typ)
	if init == nil {
		sc.define(n.Name, forgoZero(typ), t)
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
		sc.define(n.Name, v, t)
	}
}

func (in *forgoInterp) assign(sc *forgoScope, a *ast.AssignStmt) {
	if len(a.Lhs) != 1 || len(a.Rhs) != 1 {
		if a.Tok == token.DEFINE || a.Tok == token.ASSIGN && len(a.Lhs) == len(a.Rhs) {
			vals := make([]fgoeval.Value, len(a.Rhs))
			for i, r := range a.Rhs {
				vals[i] = in.eval(sc, r)
			}
			for i, l := range a.Lhs {
				in.store(sc, l, vals[i], a.Tok == token.DEFINE, a.Rhs[i])
			}
			return
		}
		forgoFail("forgo: multi-value assignments are not supported in comptime functions")
	}
	lhs, rhs := a.Lhs[0], a.Rhs[0]
	switch a.Tok {
	case token.DEFINE:
		n, ok := lhs.(*ast.Ident)
		if !ok {
			forgoFail("forgo: cannot declare %T in comptime functions", lhs)
		}
		in.bind(sc, n, nil, rhs)
	case token.ASSIGN:
		in.store(sc, lhs, in.eval(sc, rhs), false, rhs)
	default:
		op := forgoAssignOp(a.Tok)
		in.update(sc, lhs, op, fgoeval.Scalar(in.eval(sc, rhs), "operand"))
	}
}

// store assigns v (the value of rhs) to lhs, a name or an element.
func (in *forgoInterp) store(sc *forgoScope, lhs ast.Expr, v fgoeval.Value, define bool, rhs ast.Expr) {
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
			sc.define(l.Name, v, t)
			return
		}
		if cv, ok := v.(constant.Value); ok {
			v = fgoeval.Convert(cv, sc.typeOf(l.Name))
		}
		if !sc.assign(l.Name, v) {
			forgoFail("forgo: undefined: %s", l.Name)
		}
	case *ast.IndexExpr:
		in.storeElem(sc, l, func(fgoeval.Value) fgoeval.Value { return v })
	default:
		forgoFail("forgo: only assignment to a name or an element is supported in comptime functions")
	}
}

// update applies x op= y to lhs, a name or an element.
func (in *forgoInterp) update(sc *forgoScope, lhs ast.Expr, op token.Token, y constant.Value) {
	switch l := lhs.(type) {
	case *ast.Ident:
		cur, ok := sc.get(l.Name)
		if !ok {
			forgoFail("forgo: undefined: %s", l.Name)
		}
		sc.assign(l.Name, fgoeval.BinaryOp(fgoeval.Scalar(cur, l.Name), op, y, sc.typeOf(l.Name)))
	case *ast.IndexExpr:
		in.storeElem(sc, l, func(cur fgoeval.Value) fgoeval.Value {
			return fgoeval.BinaryOp(fgoeval.Scalar(cur, "element"), op, y, fgoeval.ValueType(cur))
		})
	default:
		forgoFail("forgo: only assignment to a name or an element is supported in comptime functions")
	}
}

// storeElem sets an element of a slice or a key of a map to f of its
// current value.
func (in *forgoInterp) storeElem(sc *forgoScope, ix *ast.IndexExpr, f func(fgoeval.Value) fgoeval.Value) {
	base := in.eval(sc, ix.X)
	key := in.eval(sc, ix.Index)
	switch b := base.(type) {
	case fgoeval.ArrayVal:
		i := fgoeval.ScalarInt(key, "index expression")
		if i < 0 || i >= int64(len(b.Elems)) {
			forgoFail("forgo: index %d out of range in comptime evaluation (length %d)", i, len(b.Elems))
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
		forgoFail("forgo: cannot assign to an element of this value in comptime evaluation")
	}
}

func (in *forgoInterp) evalBool(sc *forgoScope, e ast.Expr) bool {
	cv, ok := in.eval(sc, e).(constant.Value)
	if !ok || cv.Kind() != constant.Bool {
		forgoFail("forgo: condition is not a boolean constant")
	}
	return constant.BoolVal(cv)
}

func (in *forgoInterp) eval(sc *forgoScope, e ast.Expr) fgoeval.Value {
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
		forgoFail("forgo: undefined: %s", x.Name)
	case *ast.BasicLit:
		v := constant.MakeFromLiteral(x.Value, x.Kind, 0)
		if v.Kind() == constant.Unknown {
			forgoFail("forgo: invalid literal %q in comptime function", x.Value)
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
		forgoFail("forgo: unsupported unary operator in comptime function")
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
			forgoFail("forgo: cannot select field %q from a non-struct/map value in comptime evaluation", x.Sel.Name)
		}
		v, ok := obj.Fields[x.Sel.Name]
		if !ok {
			forgoFail("forgo: no field %q in comptime evaluation", x.Sel.Name)
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
					forgoFail("forgo: index %d out of range in comptime evaluation (length %d)", i, len(s))
				}
				return constant.MakeInt64(int64(s[i]))
			}
		case fgoeval.ArrayVal:
			i := fgoeval.ScalarInt(in.eval(sc, x.Index), "index expression")
			if i < 0 || i >= int64(len(b.Elems)) {
				forgoFail("forgo: index %d out of range in comptime evaluation (length %d)", i, len(b.Elems))
			}
			return b.Elems[i]
		case fgoeval.ObjectVal:
			k := fgoeval.ScalarString(in.eval(sc, x.Index), "index expression")
			v, ok := b.Fields[k]
			if !ok {
				forgoFail("forgo: no key %q in comptime evaluation", k)
			}
			return v
		}
		forgoFail("forgo: cannot index a scalar value in comptime evaluation")
	}
	forgoFail("forgo: unsupported expression %T in comptime function", e)
	panic("unreachable")
}

// compositeLit evaluates a composite literal: unkeyed elements make a
// slice, keyed ones a struct or map (see the compiler's evalCompositeLit).
func (in *forgoInterp) compositeLit(sc *forgoScope, lit *ast.CompositeLit) fgoeval.Value {
	keyed := 0
	for _, e := range lit.Elts {
		if _, ok := e.(*ast.KeyValueExpr); ok {
			keyed++
		}
	}
	if keyed == 0 {
		elems := make([]fgoeval.Value, len(lit.Elts))
		for i, e := range lit.Elts {
			elems[i] = in.eval(sc, e)
		}
		return fgoeval.ArrayVal{Elems: elems}
	}
	if keyed != len(lit.Elts) {
		forgoFail("forgo: mixed keyed and unkeyed composite literal elements are not supported in comptime evaluation")
	}
	obj := fgoeval.ObjectVal{Fields: map[string]fgoeval.Value{}}
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

func (in *forgoInterp) slice(sc *forgoScope, x *ast.SliceExpr) fgoeval.Value {
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
			forgoFail("forgo: slice bounds [%d:%d] out of range in comptime evaluation (length %d)", lo, hi, len(s))
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
			forgoFail("forgo: slice bounds [%d:%d:%d] out of range in comptime evaluation (capacity %d)", lo, hi, max, n)
		}
		return fgoeval.ArrayVal{Elems: b.Elems[lo:hi:max]}
	}
	forgoFail("forgo: cannot slice this value in comptime evaluation")
	panic("unreachable")
}

func (in *forgoInterp) evalCall(sc *forgoScope, call *ast.CallExpr) fgoeval.Value {
	fun := ast.Unparen(call.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = ix.X
	case *ast.IndexListExpr:
		fun = ix.X
	}
	if sel, ok := fun.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok && fgoeval.HasNative(pkg.Name, sel.Sel.Name) {
			args := make([]fgoeval.Value, len(call.Args))
			for i, a := range call.Args {
				args[i] = in.eval(sc, a)
			}
			dir := filepath.Dir(in.fset.Position(call.Pos()).Filename)
			v, _ := fgoeval.NativeCall(dir, pkg.Name, sel.Sel.Name, args)
			return v
		}
	}
	if at, ok := fun.(*ast.ArrayType); ok && at.Len == nil && len(call.Args) == 1 {
		// []byte(s)
		if n, ok := at.Elt.(*ast.Ident); ok && (n.Name == "byte" || n.Name == "uint8") {
			s := fgoeval.ScalarString(in.eval(sc, call.Args[0]), "[]byte conversion")
			elems := make([]fgoeval.Value, len(s))
			for i := range len(s) {
				elems[i] = constant.MakeInt64(int64(s[i]))
			}
			return fgoeval.ArrayVal{Elems: elems}
		}
	}
	name, ok := fun.(*ast.Ident)
	if !ok {
		forgoFail("forgo: unsupported function call in comptime function")
	}
	if _, shadowed := sc.get(name.Name); !shadowed {
		if v, ok := in.builtin(sc, name.Name, call); ok {
			return v
		}
	}
	fn, ok := in.funcs[name.Name]
	if !ok {
		forgoFail("forgo: %s is not a //fgo:comptime function and cannot be called at compile time", name.Name)
	}
	args := make([]fgoeval.Value, len(call.Args))
	for i, a := range call.Args {
		args[i] = in.eval(sc, a)
	}
	return in.call(fn, args)
}

// builtin evaluates len, cap, append, make, or a conversion to a basic
// type, reporting whether call was one.
func (in *forgoInterp) builtin(sc *forgoScope, name string, call *ast.CallExpr) (fgoeval.Value, bool) {
	if fgoeval.IsBasicType(name) && len(call.Args) == 1 {
		return fgoeval.Convert(fgoeval.Scalar(in.eval(sc, call.Args[0]), name+" conversion"), name), true
	}
	switch name {
	case "len", "cap":
		if len(call.Args) != 1 {
			forgoFail("forgo: %s takes one argument", name)
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
		forgoFail("forgo: invalid argument to %s in comptime evaluation", name)
	case "append":
		if len(call.Args) == 0 {
			forgoFail("forgo: append needs a slice")
		}
		var elems []fgoeval.Value
		switch v := in.eval(sc, call.Args[0]).(type) {
		case fgoeval.ArrayVal:
			elems = v.Elems
		case constant.Value:
			if v.Kind() != constant.Unknown {
				forgoFail("forgo: first argument to append must be a slice")
			}
		default:
			forgoFail("forgo: first argument to append must be a slice")
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
			forgoFail("forgo: cannot append this value in comptime evaluation")
		}
		for _, a := range rest {
			elems = append(elems, in.eval(sc, a))
		}
		return fgoeval.ArrayVal{Elems: elems}, true
	case "make":
		if len(call.Args) < 2 {
			forgoFail("forgo: make needs a slice type and a length in comptime evaluation")
		}
		at, ok := call.Args[0].(*ast.ArrayType)
		if !ok || at.Len != nil {
			forgoFail("forgo: only slices can be made in comptime evaluation")
		}
		n := fgoeval.ScalarInt(in.eval(sc, call.Args[1]), "make")
		c := n
		if len(call.Args) > 2 {
			c = fgoeval.ScalarInt(in.eval(sc, call.Args[2]), "make")
		}
		if n < 0 || c < n || c > 1<<24 {
			forgoFail("forgo: invalid make length or capacity in comptime evaluation")
		}
		elems := make([]fgoeval.Value, n, c)
		for i := range elems {
			elems[i] = forgoZero(at.Elt)
		}
		return fgoeval.ArrayVal{Elems: elems}, true
	}
	return nil, false
}

// typeOf infers the basic type of e (see the compiler's Interp.typeOf).
func (in *forgoInterp) typeOf(sc *forgoScope, e ast.Expr) string {
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
			if fn, ok := in.funcs[n.Name]; ok && fn.Type.Results != nil && len(fn.Type.Results.List) == 1 {
				return forgoTypeName(fn.Type.Results.List[0].Type)
			}
		}
	}
	return ""
}

func forgoTypeName(e ast.Expr) string {
	if n, ok := e.(*ast.Ident); ok && fgoeval.IsBasicType(n.Name) {
		return n.Name
	}
	return ""
}

func forgoZero(typ ast.Expr) fgoeval.Value {
	switch t := typ.(type) {
	case *ast.ArrayType:
		if t.Len == nil {
			return fgoeval.ArrayVal{}
		}
	case *ast.MapType:
		return fgoeval.ObjectVal{Fields: map[string]fgoeval.Value{}}
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

// forgoAssignOp returns the operator of assignment operator tok (+= etc.).
func forgoAssignOp(tok token.Token) token.Token {
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
	forgoFail("forgo: unsupported assignment operator %s in comptime function", tok)
	panic("unreachable")
}
