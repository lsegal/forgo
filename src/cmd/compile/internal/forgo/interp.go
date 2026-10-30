// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package forgo implements compile-time execution ("comptime" functions) and
// AST macros for the forgo language, a fork of Go.
//
// A function marked with the //fgo:comptime pragma is ordinary Go that is
// additionally interpretable by this package, so it can be invoked directly
// inside a const declaration's initializer and its result folded into a
// constant at compile time (see cmd/compile/internal/types2's constDecl).
//
// A function marked with the //fgo:macro pragma receives the *unevaluated*
// syntax trees of its call-site arguments (as NodeVal) and returns a
// NodeVal-wrapped syntax tree that is spliced into the caller's AST in place
// of the macro call, before type checking runs (see
// cmd/compile/internal/noder's macro expansion pass). Inside a macro body,
// Quote(func(){ ... }) captures the enclosed statement/expression as an AST
// template without evaluating it, and Splice(x) marks a point in that
// template where the AST bound to local variable x should be substituted.
package forgo

import (
	"cmd/compile/internal/syntax"
	"fmt"
	"go/constant"
	"go/token"
	"internal/fgoeval"
	"path/filepath"
)

// What the interpreter shares with go/types' (see internal/fgoeval).
type (
	Value      = fgoeval.Value
	objectVal  = fgoeval.ObjectVal
	arrayVal   = fgoeval.ArrayVal
	runtimeErr = fgoeval.Error
)

var (
	fail           = fgoeval.Fail
	binaryOp       = fgoeval.BinaryOp
	convertTo      = fgoeval.Convert
	isIntType      = fgoeval.IsIntType
	isFloatType    = fgoeval.IsFloatType
	isBasicType    = fgoeval.IsBasicType
	defaultType    = fgoeval.DefaultType
	valueType      = fgoeval.ValueType
	scalarConstant = fgoeval.Scalar
	scalarString   = fgoeval.ScalarString
	scalarInt      = fgoeval.ScalarInt
	nativeValue    = fgoeval.Native
	ToConstant     = fgoeval.ToConstant
)

// HasNative reports whether pkg.fn is one of forgo's natively evaluated
// compile-time helpers (see fgoeval.HasNative).
func (in *Interp) HasNative(pkg, fn string) bool { return fgoeval.HasNative(pkg, fn) }

// nativeCall evaluates one of those helpers called at pos.
func (in *Interp) nativeCall(pos syntax.Pos, pkg, fn string, args []Value) (Value, bool) {
	dir := ""
	if base := pos.Base(); base != nil && base.Filename() != "" {
		dir = filepath.Dir(base.Filename())
	}
	return fgoeval.NativeCall(dir, pkg, fn, args)
}

// NodeVal wraps a syntax tree produced or manipulated by a macro.
type NodeVal struct {
	Node syntax.Node
}

// Interp evaluates //fgo:comptime and //fgo:macro function bodies.
type Interp struct {
	// Funcs holds every //fgo:comptime/macro function declared in the
	// package, by name, so comptime/macro bodies can call one another.
	Funcs map[string]*syntax.FuncDecl
}

type scope struct {
	parent *scope
	vars   map[string]Value
	types  map[string]string // the basic type each variable was declared with, if known
}

func newScope(parent *scope) *scope { return &scope{parent: parent, vars: map[string]Value{}} }

func (s *scope) get(name string) (Value, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if v, ok := cur.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func (s *scope) define(name string, v Value) { s.vars[name] = v }

// defineTyped defines name with basic type t ("" if not a basic type).
func (s *scope) defineTyped(name string, v Value, t string) {
	s.vars[name] = v
	if t != "" {
		if s.types == nil {
			s.types = map[string]string{}
		}
		s.types[name] = t
	} else if s.types != nil {
		delete(s.types, name)
	}
}

// typeOf returns the basic type name was declared with, or "".
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

type returnSignal struct{ val Value }

// breakSignal and continueSignal end the innermost loop's iteration.
type breakSignal struct{}
type continueSignal struct{}

// EvalMacro runs a //fgo:macro function, binding its parameters to the
// unevaluated argument syntax trees, and returns the syntax tree the macro
// expands to.
func (in *Interp) EvalMacro(fn *syntax.FuncDecl, argNodes []syntax.Node) (syntax.Node, error) {
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
		return nil, fmt.Errorf("macro %s did not return a quoted node", fn.Name.Value)
	}
	return nv.Node, nil
}

// EvalExprValue evaluates a self-contained expression (literals,
// parenthesization, unary/binary operators, composite literals, and calls
// to other //fgo:comptime functions or forgo's native compile-time
// helpers, optionally followed by field selection/indexing) with no
// surrounding variable scope, returning whatever Value it produces: a
// scalar (constant.Value), or a struct/slice/map composite value. It's
// used to fold a compile-time expression written directly in source, e.g.
// a const declaration's initializer.
func (in *Interp) EvalExprValue(e syntax.Expr) (result Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(runtimeErr); ok {
				err = re.Err
				return
			}
			panic(r)
		}
	}()
	return in.evalExpr(newScope(nil), e), nil
}

// EvalConstExpr is like EvalExprValue, but additionally requires the
// result to be a scalar constant (e.g. because it's an argument being
// folded for a call that only accepts scalars).
func (in *Interp) EvalConstExpr(e syntax.Expr) (constant.Value, error) {
	v, err := in.EvalExprValue(e)
	if err != nil {
		return nil, err
	}
	cv, ok := v.(constant.Value)
	if !ok {
		return nil, fmt.Errorf("forgo: expression did not evaluate to a constant")
	}
	return cv, nil
}

func (in *Interp) call(fn *syntax.FuncDecl, args []Value) (val Value, err error) {
	if fn.Body == nil {
		return nil, fmt.Errorf("%s has no body", fn.Name.Value)
	}
	defer func() {
		if r := recover(); r != nil {
			if rs, ok := r.(returnSignal); ok {
				val, err = rs.val, nil
				return
			}
			if e, ok := r.(runtimeErr); ok {
				err = e.Err
				return
			}
			panic(r)
		}
	}()

	defer func() {
		// The result takes the declared result type.
		if len(fn.Type.ResultList) == 1 && err == nil {
			if cv, ok := val.(constant.Value); ok {
				val = convertTo(cv, typeName(fn.Type.ResultList[0].Type))
			}
		}
	}()

	sc := newScope(nil)
	i := 0
	for _, p := range fn.Type.ParamList {
		if p.Name == nil {
			i++
			continue
		}
		if i < len(args) {
			t := typeName(p.Type)
			v := args[i]
			if cv, ok := v.(constant.Value); ok {
				v = convertTo(cv, t)
			}
			sc.defineTyped(p.Name.Value, v, t)
		}
		i++
	}
	in.execBlock(sc, fn.Body)
	return nil, nil
}

// ---------------------------------------------------------------------------
// Statement execution

func (in *Interp) execBlock(parent *scope, b *syntax.BlockStmt) {
	sc := newScope(parent)
	for _, s := range b.List {
		in.execStmt(sc, s)
	}
}

func (in *Interp) execStmt(sc *scope, s syntax.Stmt) {
	switch x := s.(type) {
	case *syntax.BlockStmt:
		in.execBlock(sc, x)

	case *syntax.EmptyStmt:
		// no-op

	case *syntax.ExprStmt:
		in.evalExpr(sc, x.X)

	case *syntax.DeclStmt:
		for _, d := range x.DeclList {
			in.execLocalDecl(sc, d)
		}

	case *syntax.AssignStmt:
		in.execAssign(sc, x)

	case *syntax.ReturnStmt:
		var v Value
		if x.Results != nil {
			v = in.evalExpr(sc, x.Results)
		}
		panic(returnSignal{v})

	case *syntax.IfStmt:
		child := newScope(sc)
		if x.Init != nil {
			in.execStmt(child, x.Init)
		}
		if in.evalBool(child, x.Cond) {
			in.execBlock(child, x.Then)
		} else if x.Else != nil {
			in.execStmt(child, x.Else)
		}

	case *syntax.ForStmt:
		child := newScope(sc)
		if r, isRange := x.Init.(*syntax.RangeClause); isRange {
			in.execRange(child, r, x.Body)
			return
		}
		if x.Init != nil {
			in.execStmt(child, x.Init)
		}
		for x.Cond == nil || in.evalBool(child, x.Cond) {
			if in.loopBody(child, x.Body) {
				break
			}
			if x.Post != nil {
				in.execStmt(child, x.Post)
			}
		}

	case *syntax.BranchStmt:
		if x.Label != nil {
			fail("forgo: labeled break and continue are not supported in comptime functions")
		}
		switch x.Tok {
		case syntax.Break:
			panic(breakSignal{})
		case syntax.Continue:
			panic(continueSignal{})
		}
		fail("forgo: unsupported branch statement in comptime function")

	default:
		fail("forgo: unsupported statement %T in comptime function", s)
	}
}

// loopBody runs one iteration of a loop body, reporting whether it ran a
// break.
func (in *Interp) loopBody(sc *scope, body *syntax.BlockStmt) (broke bool) {
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
	in.execBlock(sc, body)
	return false
}

// execRange runs a range-form for loop over an int, a string (by rune), a
// slice or array, or a map or struct value (in key order).
func (in *Interp) execRange(sc *scope, r *syntax.RangeClause, body *syntax.BlockStmt) {
	var keyName, valName syntax.Expr
	switch lhs := r.Lhs.(type) {
	case nil:
	case *syntax.ListExpr:
		keyName = lhs.ElemList[0]
		if len(lhs.ElemList) > 1 {
			valName = lhs.ElemList[1]
		}
	default:
		keyName = lhs
	}
	set := func(e syntax.Expr, v Value, t string) {
		n, ok := e.(*syntax.Name)
		if e == nil || ok && n.Value == "_" {
			return
		}
		if !ok {
			fail("forgo: range variables must be plain names in comptime functions")
		}
		if r.Def {
			sc.defineTyped(n.Value, v, t)
		} else if !sc.assign(n.Value, v) {
			fail("forgo: undefined: %s", n.Value)
		}
	}
	iter := func(k Value, kt string, v Value, vt string) bool {
		set(keyName, k, kt)
		set(valName, v, vt)
		return in.loopBody(sc, body)
	}
	switch x := in.evalExpr(sc, r.X).(type) {
	case constant.Value:
		switch x.Kind() {
		case constant.Int:
			n := scalarInt(x, "range")
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
	case arrayVal:
		for i, e := range x.Elems {
			if iter(constant.MakeInt64(int64(i)), "int", e, valueType(e)) {
				return
			}
		}
	case objectVal:
		for _, k := range x.Keys {
			if iter(constant.MakeString(k), "string", x.Fields[k], valueType(x.Fields[k])) {
				return
			}
		}
	default:
		fail("forgo: cannot range over this value in comptime functions")
	}
}

func (in *Interp) execLocalDecl(sc *scope, d syntax.Decl) {
	switch x := d.(type) {
	case *syntax.VarDecl:
		in.bindNames(sc, x.NameList, x.Type, x.Values)
	case *syntax.ConstDecl:
		in.bindNames(sc, x.NameList, x.Type, x.Values)
	default:
		fail("forgo: unsupported local declaration %T in comptime function", d)
	}
}

func (in *Interp) bindNames(sc *scope, names []*syntax.Name, typ syntax.Expr, values syntax.Expr) {
	t := typeName(typ)
	if values == nil {
		for _, n := range names {
			sc.defineTyped(n.Value, zeroValue(typ), t)
		}
		return
	}
	bind := func(n *syntax.Name, e syntax.Expr) {
		v := in.evalExpr(sc, e)
		vt := t
		if typ == nil {
			vt = in.typeOf(sc, e)
		}
		if cv, ok := v.(constant.Value); ok {
			if vt == "" {
				vt = defaultType(cv)
			}
			v = convertTo(cv, vt)
		}
		sc.defineTyped(n.Value, v, vt)
	}
	if lst, ok := values.(*syntax.ListExpr); ok {
		for i, n := range names {
			if n.Value == "_" || i >= len(lst.ElemList) {
				continue
			}
			bind(n, lst.ElemList[i])
		}
		return
	}
	if len(names) != 1 {
		fail("forgo: multi-value declarations are not supported in comptime functions")
	}
	bind(names[0], values)
}

func (in *Interp) execAssign(sc *scope, a *syntax.AssignStmt) {
	if ix, ok := a.Lhs.(*syntax.IndexExpr); ok {
		in.execIndexAssign(sc, a, ix)
		return
	}
	name, ok := a.Lhs.(*syntax.Name)
	if !ok {
		fail("forgo: only assignment to a name or an element is supported in comptime functions")
	}
	if name.Value == "_" {
		in.evalExpr(sc, a.Rhs)
		return
	}

	if a.Op == syntax.Def {
		in.bindNames(sc, []*syntax.Name{name}, nil, a.Rhs)
		return
	}

	t := sc.typeOf(name.Value)
	if a.Rhs == nil {
		// i++ / i--
		cur, ok := sc.get(name.Value)
		if !ok {
			fail("forgo: undefined: %s", name.Value)
		}
		next := binaryOp(scalarConstant(cur, name.Value), tokenFor(a.Op), constant.MakeInt64(1), t)
		if !sc.assign(name.Value, next) {
			fail("forgo: undefined: %s", name.Value)
		}
		return
	}

	rhs := in.evalExpr(sc, a.Rhs)
	if a.Op == 0 {
		if cv, ok := rhs.(constant.Value); ok {
			rhs = convertTo(cv, t)
		}
		if !sc.assign(name.Value, rhs) {
			fail("forgo: undefined: %s", name.Value)
		}
		return
	}

	cur, ok := sc.get(name.Value)
	if !ok {
		fail("forgo: undefined: %s", name.Value)
	}
	next := binaryOp(scalarConstant(cur, name.Value), tokenFor(a.Op), scalarConstant(rhs, name.Value), t)
	if !sc.assign(name.Value, next) {
		fail("forgo: undefined: %s", name.Value)
	}
}

// execIndexAssign assigns an element of a slice or a key of a map
// (s[i] = v, m[k] += v).
func (in *Interp) execIndexAssign(sc *scope, a *syntax.AssignStmt, ix *syntax.IndexExpr) {
	base := in.evalExpr(sc, ix.X)
	key := in.evalExpr(sc, ix.Index)
	var cur Value
	var store func(Value)
	switch b := base.(type) {
	case arrayVal:
		i := scalarInt(key, "index expression")
		if i < 0 || i >= int64(len(b.Elems)) {
			fail("forgo: index %d out of range in comptime evaluation (length %d)", i, len(b.Elems))
		}
		cur, store = b.Elems[i], func(v Value) { b.Elems[i] = v }
	case objectVal:
		k := scalarString(key, "index expression")
		cur = b.Fields[k]
		store = func(v Value) {
			if _, ok := b.Fields[k]; !ok {
				b.Keys = append(b.Keys, k)
				if n, ok := ix.X.(*syntax.Name); ok {
					sc.assign(n.Value, b)
				}
			}
			b.Fields[k] = v
		}
	default:
		fail("forgo: cannot assign to an element of this value in comptime evaluation")
	}
	switch {
	case a.Rhs == nil:
		store(binaryOp(scalarConstant(cur, "element"), tokenFor(a.Op), constant.MakeInt64(1), valueType(cur)))
	case a.Op == 0:
		store(in.evalExpr(sc, a.Rhs))
	default:
		store(binaryOp(scalarConstant(cur, "element"), tokenFor(a.Op), scalarConstant(in.evalExpr(sc, a.Rhs), "element"), valueType(cur)))
	}
}

// ---------------------------------------------------------------------------
// Expression evaluation (comptime / constant-value mode)

func (in *Interp) evalBool(sc *scope, e syntax.Expr) bool {
	v := in.evalExpr(sc, e)
	cv, ok := v.(constant.Value)
	if !ok || cv.Kind() != constant.Bool {
		fail("forgo: condition is not a boolean constant")
	}
	return constant.BoolVal(cv)
}

func (in *Interp) evalExpr(sc *scope, e syntax.Expr) Value {
	switch x := e.(type) {
	case *syntax.ParenExpr:
		return in.evalExpr(sc, x.X)

	case *syntax.Name:
		switch x.Value {
		case "true":
			return constant.MakeBool(true)
		case "false":
			return constant.MakeBool(false)
		}
		if v, ok := sc.get(x.Value); ok {
			return v
		}
		fail("forgo: undefined: %s", x.Value)

	case *syntax.BasicLit:
		return literalValue(x)

	case *syntax.Operation:
		return in.evalOperation(sc, x)

	case *syntax.CallExpr:
		return in.evalCall(sc, x)

	case *syntax.CompositeLit:
		return in.evalCompositeLit(sc, x)

	case *syntax.SelectorExpr:
		base := in.evalExpr(sc, x.X)
		obj, ok := base.(objectVal)
		if !ok {
			fail("forgo: cannot select field %q from a non-struct/map value in comptime evaluation", x.Sel.Value)
		}
		v, ok := obj.Fields[x.Sel.Value]
		if !ok {
			fail("forgo: no field %q in comptime evaluation", x.Sel.Value)
		}
		return v

	case *syntax.SliceExpr:
		return in.evalSlice(sc, x)

	case *syntax.IndexExpr:
		base := in.evalExpr(sc, x.X)
		switch b := base.(type) {
		case constant.Value:
			if b.Kind() == constant.String {
				str := constant.StringVal(b)
				idx := scalarInt(in.evalExpr(sc, x.Index), "index expression")
				if idx < 0 || idx >= int64(len(str)) {
					fail("forgo: index %d out of range in comptime evaluation (length %d)", idx, len(str))
				}
				return constant.MakeInt64(int64(str[idx]))
			}
		case arrayVal:
			idx := scalarInt(in.evalExpr(sc, x.Index), "index expression")
			if idx < 0 || idx >= int64(len(b.Elems)) {
				fail("forgo: index %d out of range in comptime evaluation (length %d)", idx, len(b.Elems))
			}
			return b.Elems[idx]
		case objectVal:
			key := scalarString(in.evalExpr(sc, x.Index), "index expression")
			v, ok := b.Fields[key]
			if !ok {
				fail("forgo: no key %q in comptime evaluation", key)
			}
			return v
		}
		fail("forgo: cannot index a scalar value in comptime evaluation")
	}
	fail("forgo: unsupported expression %T in comptime function", e)
	panic("unreachable")
}

// evalCompositeLit evaluates a struct/map/slice/array composite literal
// (e.g. Config{Name: "x", Port: 8080} or []int{1, 2, 3}) into an objectVal
// or arrayVal. It has no access to real type information (the interpreter
// works purely from syntax), so it infers the shape structurally: keyed
// elements become an objectVal (struct field name or map key -> value),
// unkeyed elements become an arrayVal.
func (in *Interp) evalCompositeLit(sc *scope, lit *syntax.CompositeLit) Value {
	if lit.NKeys == 0 {
		elems := make([]Value, len(lit.ElemList))
		for i, e := range lit.ElemList {
			elems[i] = in.evalExpr(sc, e)
		}
		return arrayVal{Elems: elems}
	}
	if lit.NKeys != len(lit.ElemList) {
		fail("forgo: mixed keyed and unkeyed composite literal elements are not supported in comptime evaluation")
	}
	obj := objectVal{Fields: map[string]Value{}}
	for _, e := range lit.ElemList {
		kv, ok := e.(*syntax.KeyValueExpr)
		if !ok {
			fail("forgo: unsupported composite literal element %T in comptime evaluation", e)
		}
		key := in.compositeLitKey(sc, kv.Key)
		if _, dup := obj.Fields[key]; !dup {
			obj.Keys = append(obj.Keys, key)
		}
		obj.Fields[key] = in.evalExpr(sc, kv.Value)
	}
	return obj
}

// compositeLitKey evaluates a composite literal element's key. A struct
// literal's key is a bare field name (Config{Name: "x"}); a map literal's
// key is an ordinary expression (map[string]int{someKey: 1}). Since the
// interpreter has no type information to disambiguate the two forms the
// way the real type checker does, it uses a heuristic: a bare identifier
// that isn't bound to a local variable is treated as a struct field name;
// anything else (including a bound identifier) is evaluated as an
// expression and must produce a string.
func (in *Interp) compositeLitKey(sc *scope, k syntax.Expr) string {
	if name, ok := k.(*syntax.Name); ok {
		if _, bound := sc.get(name.Value); !bound {
			return name.Value
		}
	}
	key := scalarString(in.evalExpr(sc, k), "composite literal key")
	return key
}

func literalValue(lit *syntax.BasicLit) constant.Value {
	var tok token.Token
	switch lit.Kind {
	case syntax.IntLit:
		tok = token.INT
	case syntax.FloatLit:
		tok = token.FLOAT
	case syntax.RuneLit:
		tok = token.CHAR
	case syntax.StringLit:
		tok = token.STRING
	default:
		fail("forgo: unsupported literal kind in comptime function")
	}
	v := constant.MakeFromLiteral(lit.Value, tok, 0)
	if v.Kind() == constant.Unknown {
		fail("forgo: invalid literal %q in comptime function", lit.Value)
	}
	return v
}

func (in *Interp) evalOperation(sc *scope, op *syntax.Operation) Value {
	if op.Y == nil {
		x := scalarConstant(in.evalExpr(sc, op.X), "unary operand")
		t := in.typeOf(sc, op.X)
		switch op.Op {
		case syntax.Sub:
			return convertTo(constant.UnaryOp(token.SUB, x, 0), t)
		case syntax.Add:
			return x
		case syntax.Not:
			return constant.MakeBool(!constant.BoolVal(x))
		case syntax.Xor:
			return convertTo(constant.UnaryOp(token.XOR, x, 0), t)
		}
		fail("forgo: unsupported unary operator in comptime function")
	}

	if op.Op == syntax.AndAnd {
		if !in.evalBool(sc, op.X) {
			return constant.MakeBool(false)
		}
		return constant.MakeBool(in.evalBool(sc, op.Y))
	}
	if op.Op == syntax.OrOr {
		if in.evalBool(sc, op.X) {
			return constant.MakeBool(true)
		}
		return constant.MakeBool(in.evalBool(sc, op.Y))
	}

	x := scalarConstant(in.evalExpr(sc, op.X), "operand")
	y := scalarConstant(in.evalExpr(sc, op.Y), "operand")

	switch op.Op {
	case syntax.Eql, syntax.Neq, syntax.Lss, syntax.Leq, syntax.Gtr, syntax.Geq:
		return constant.MakeBool(constant.Compare(x, tokenFor(op.Op), y))
	}
	return binaryOp(x, tokenFor(op.Op), y, in.typeOf(sc, op))
}

// typeName returns the name of basic type expression e, or "".
func typeName(e syntax.Expr) string {
	if n, ok := e.(*syntax.Name); ok && isBasicType(n.Value) {
		return n.Value
	}
	return ""
}

// zeroValue returns the zero value of type expression typ.
func zeroValue(typ syntax.Expr) Value {
	switch t := typ.(type) {
	case *syntax.SliceType:
		return arrayVal{}
	case *syntax.MapType:
		return objectVal{Fields: map[string]Value{}}
	case *syntax.Name:
		switch {
		case isIntType(t.Value):
			return constant.MakeInt64(0)
		case isFloatType(t.Value):
			return constant.MakeFloat64(0)
		case t.Value == "string":
			return constant.MakeString("")
		case t.Value == "bool":
			return constant.MakeBool(false)
		}
	}
	return constant.MakeUnknown()
}

// typeOf infers the basic type of expression e from the declared types of
// the variables and functions it uses: "" for an untyped constant, or a
// type the interpreter does not track.
func (in *Interp) typeOf(sc *scope, e syntax.Expr) string {
	switch x := e.(type) {
	case *syntax.ParenExpr:
		return in.typeOf(sc, x.X)
	case *syntax.Name:
		return sc.typeOf(x.Value)
	case *syntax.BasicLit:
		return ""
	case *syntax.Operation:
		if x.Y == nil {
			if x.Op == syntax.Not {
				return "bool"
			}
			return in.typeOf(sc, x.X)
		}
		switch x.Op {
		case syntax.Eql, syntax.Neq, syntax.Lss, syntax.Leq, syntax.Gtr, syntax.Geq, syntax.AndAnd, syntax.OrOr:
			return "bool"
		case syntax.Shl, syntax.Shr:
			return in.typeOf(sc, x.X)
		}
		if t := in.typeOf(sc, x.X); t != "" {
			return t
		}
		return in.typeOf(sc, x.Y)
	case *syntax.IndexExpr:
		if v, ok := in.evalNoFail(sc, x.X).(constant.Value); ok && v.Kind() == constant.String {
			return "byte"
		}
	case *syntax.SliceExpr:
		return in.typeOf(sc, x.X)
	case *syntax.CallExpr:
		if n, ok := x.Fun.(*syntax.Name); ok {
			if isBasicType(n.Value) {
				return n.Value
			}
			switch n.Value {
			case "len", "cap":
				return "int"
			}
			if fn, ok := in.Funcs[n.Value]; ok && len(fn.Type.ResultList) == 1 {
				return typeName(fn.Type.ResultList[0].Type)
			}
		}
	}
	return ""
}

// evalNoFail is evalExpr, returning nil where that would fail.
func (in *Interp) evalNoFail(sc *scope, e syntax.Expr) (v Value) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtimeErr); !ok {
				panic(r)
			}
			v = nil
		}
	}()
	if n, ok := e.(*syntax.Name); ok {
		v, _ = sc.get(n.Value)
		return v
	}
	return in.evalExpr(sc, e)
}

// evalSlice evaluates s[i:j] (or s[i:j:k]) of a string or slice.
func (in *Interp) evalSlice(sc *scope, x *syntax.SliceExpr) Value {
	base := in.evalExpr(sc, x.X)
	var n int64
	switch b := base.(type) {
	case constant.Value:
		if b.Kind() != constant.String {
			fail("forgo: cannot slice a non-string scalar in comptime evaluation")
		}
		n = int64(len(constant.StringVal(b)))
	case arrayVal:
		n = int64(cap(b.Elems))
	default:
		fail("forgo: cannot slice this value in comptime evaluation")
	}
	idx := [3]int64{0, -1, -1}
	for i, e := range x.Index {
		if e != nil {
			idx[i] = scalarInt(in.evalExpr(sc, e), "slice index")
		}
	}
	lo, hi, max := idx[0], idx[1], idx[2]
	if str, ok := base.(constant.Value); ok {
		if hi < 0 {
			hi = n
		}
		if lo < 0 || hi < lo || hi > n {
			fail("forgo: slice bounds [%d:%d] out of range in comptime evaluation (length %d)", lo, hi, n)
		}
		return constant.MakeString(constant.StringVal(str)[lo:hi])
	}
	elems := base.(arrayVal).Elems
	if hi < 0 {
		hi = int64(len(elems))
	}
	if max < 0 {
		max = n
	}
	if lo < 0 || hi < lo || max < hi || max > n {
		fail("forgo: slice bounds [%d:%d:%d] out of range in comptime evaluation (capacity %d)", lo, hi, max, n)
	}
	return arrayVal{Elems: elems[lo:hi:max]}
}

// evalBuiltin evaluates a call to len, cap, append or make, or a
// conversion to a basic type, reporting whether call was one.
func (in *Interp) evalBuiltin(sc *scope, name string, call *syntax.CallExpr) (Value, bool) {
	if isBasicType(name) && len(call.ArgList) == 1 {
		v := scalarConstant(in.evalExpr(sc, call.ArgList[0]), name+" conversion")
		return convertTo(v, name), true
	}
	switch name {
	case "len", "cap":
		if len(call.ArgList) != 1 {
			fail("forgo: %s takes one argument", name)
		}
		switch v := in.evalExpr(sc, call.ArgList[0]).(type) {
		case constant.Value:
			if v.Kind() == constant.String {
				return constant.MakeInt64(int64(len(constant.StringVal(v)))), true
			}
		case arrayVal:
			if name == "cap" {
				return constant.MakeInt64(int64(cap(v.Elems))), true
			}
			return constant.MakeInt64(int64(len(v.Elems))), true
		case objectVal:
			return constant.MakeInt64(int64(len(v.Fields))), true
		}
		fail("forgo: invalid argument to %s in comptime evaluation", name)
	case "append":
		if len(call.ArgList) == 0 {
			fail("forgo: append needs a slice")
		}
		var elems []Value
		switch v := in.evalExpr(sc, call.ArgList[0]).(type) {
		case arrayVal:
			elems = v.Elems
		case constant.Value:
			if v.Kind() != constant.Unknown {
				fail("forgo: first argument to append must be a slice")
			}
		default:
			fail("forgo: first argument to append must be a slice")
		}
		rest := call.ArgList[1:]
		if call.HasDots {
			switch v := in.evalExpr(sc, rest[0]).(type) {
			case arrayVal:
				return arrayVal{Elems: append(elems, v.Elems...)}, true
			case constant.Value:
				if v.Kind() == constant.String {
					for _, c := range []byte(constant.StringVal(v)) {
						elems = append(elems, constant.MakeInt64(int64(c)))
					}
					return arrayVal{Elems: elems}, true
				}
			}
			fail("forgo: cannot append this value in comptime evaluation")
		}
		for _, a := range rest {
			elems = append(elems, in.evalExpr(sc, a))
		}
		return arrayVal{Elems: elems}, true
	case "make":
		if len(call.ArgList) < 2 {
			fail("forgo: make needs a slice type and a length in comptime evaluation")
		}
		st, ok := call.ArgList[0].(*syntax.SliceType)
		if !ok {
			fail("forgo: only slices can be made in comptime evaluation")
		}
		n := scalarInt(in.evalExpr(sc, call.ArgList[1]), "make")
		c := n
		if len(call.ArgList) > 2 {
			c = scalarInt(in.evalExpr(sc, call.ArgList[2]), "make")
		}
		if n < 0 || c < n || c > 1<<24 {
			fail("forgo: invalid make length or capacity in comptime evaluation")
		}
		elems := make([]Value, n, c)
		for i := range elems {
			elems[i] = zeroValue(st.Elem)
		}
		return arrayVal{Elems: elems}, true
	}
	return nil, false
}

func tokenFor(op syntax.Operator) token.Token {
	switch op {
	case syntax.Add:
		return token.ADD
	case syntax.Sub:
		return token.SUB
	case syntax.Mul:
		return token.MUL
	case syntax.Div:
		return token.QUO
	case syntax.Rem:
		return token.REM
	case syntax.And:
		return token.AND
	case syntax.Or:
		return token.OR
	case syntax.Xor:
		return token.XOR
	case syntax.AndNot:
		return token.AND_NOT
	case syntax.Shl:
		return token.SHL
	case syntax.Shr:
		return token.SHR
	case syntax.Eql:
		return token.EQL
	case syntax.Neq:
		return token.NEQ
	case syntax.Lss:
		return token.LSS
	case syntax.Leq:
		return token.LEQ
	case syntax.Gtr:
		return token.GTR
	case syntax.Geq:
		return token.GEQ
	}
	fail("forgo: unsupported operator in comptime function")
	panic("unreachable")
}

func (in *Interp) evalCall(sc *scope, call *syntax.CallExpr) Value {
	if name, ok := call.Fun.(*syntax.Name); ok && name.Value == "Quote" {
		return in.evalQuote(sc, call)
	}

	// Unwrap generic instantiation syntax (f[T](args...), or f[T1, T2](args...)
	// via T.(*syntax.ListExpr)). The interpreter has no type information, so
	// it ignores the type argument(s) entirely -- see comptime/json's
	// Unmarshal[T] for why that's safe here.
	fun := call.Fun
	if ix, ok := fun.(*syntax.IndexExpr); ok {
		fun = ix.X
	}

	if sel, ok := fun.(*syntax.SelectorExpr); ok {
		if pkg, ok := sel.X.(*syntax.Name); ok {
			if v, handled := in.evalStdlibCall(sc, call.Pos(), pkg.Value, sel.Sel.Value, call.ArgList); handled {
				return v
			}
		}
	}

	if st, ok := fun.(*syntax.SliceType); ok && len(call.ArgList) == 1 {
		// []byte(s)
		if n, ok := st.Elem.(*syntax.Name); ok && (n.Value == "byte" || n.Value == "uint8") {
			v := scalarString(in.evalExpr(sc, call.ArgList[0]), "[]byte conversion")
			elems := make([]Value, len(v))
			for i := range len(v) {
				elems[i] = constant.MakeInt64(int64(v[i]))
			}
			return arrayVal{Elems: elems}
		}
	}

	name, ok := fun.(*syntax.Name)
	if !ok {
		fail("forgo: unsupported function call in comptime function")
	}

	if _, shadowed := sc.get(name.Value); !shadowed {
		if v, ok := in.evalBuiltin(sc, name.Value, call); ok {
			return v
		}
	}

	if fn, ok := in.Funcs[name.Value]; ok {
		args := make([]Value, len(call.ArgList))
		for i, a := range call.ArgList {
			args[i] = in.evalExpr(sc, a)
		}
		v, err := in.call(fn, args)
		if err != nil {
			fail("%s", err.Error())
		}
		return v
	}

	fail("forgo: %s is not a //fgo:comptime function and cannot be called at compile time", name.Value)
	panic("unreachable")
}

// evalStdlibCall implements a small, fixed allow-list of standard library
// (and forgo-provided compile-time helper) functions natively, so comptime
// functions can format results the same way Nim's compileTime procs can use
// std/strformat. pos is the call site, used by the "embed" (comptime/embed)
// helpers to resolve relative file paths.
func (in *Interp) evalStdlibCall(sc *scope, pos syntax.Pos, pkg, fn string, argList []syntax.Expr) (Value, bool) {
	args := make([]Value, len(argList))
	for i, a := range argList {
		args[i] = in.evalExpr(sc, a)
	}
	return in.nativeCall(pos, pkg, fn, args)
}
