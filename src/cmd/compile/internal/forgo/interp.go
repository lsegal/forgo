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
	stdjson "encoding/json"
	"fmt"
	"go/constant"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Value is a go/constant.Value (scalar comptime evaluation), an objectVal
// or arrayVal (composite comptime evaluation, e.g. a struct/map or
// slice/array literal, or the result of json.Unmarshal), or a NodeVal
// (macro evaluation).
type Value interface{}

// objectVal is a struct- or map[string]T-shaped composite value: a struct
// or map composite literal evaluated at compile time, or a JSON object
// produced by comptime/json's Unmarshal. Field access (x.Field) and string
// indexing (x["field"]) both read from fields, keyed by struct field name
// (for a struct literal), map key (for a map literal), or JSON object key
// (for an Unmarshal result).
type objectVal struct {
	keys   []string // preserves literal/decode order, for json.Marshal output
	fields map[string]Value
}

// arrayVal is a slice-, array-, or JSON-array-shaped composite value: a
// slice/array composite literal evaluated at compile time, or a JSON array
// produced by comptime/json's Unmarshal.
type arrayVal struct {
	elems []Value
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
				err = re.err
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
				err = e.err
				return
			}
			panic(r)
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
			sc.define(p.Name.Value, args[i])
		}
		i++
	}
	in.execBlock(sc, fn.Body)
	return nil, nil
}

type runtimeErr struct{ err error }

func fail(format string, a ...any) {
	panic(runtimeErr{fmt.Errorf(format, a...)})
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
		if x.Init != nil {
			if _, isRange := x.Init.(*syntax.RangeClause); isRange {
				fail("forgo: range-form for loops are not supported in comptime functions")
			}
			in.execStmt(child, x.Init)
		}
		for x.Cond == nil || in.evalBool(child, x.Cond) {
			in.execBlock(child, x.Body)
			if x.Post != nil {
				in.execStmt(child, x.Post)
			}
		}

	default:
		fail("forgo: unsupported statement %T in comptime function", s)
	}
}

func (in *Interp) execLocalDecl(sc *scope, d syntax.Decl) {
	switch x := d.(type) {
	case *syntax.VarDecl:
		in.bindNames(sc, x.NameList, x.Values)
	case *syntax.ConstDecl:
		in.bindNames(sc, x.NameList, x.Values)
	default:
		fail("forgo: unsupported local declaration %T in comptime function", d)
	}
}

func (in *Interp) bindNames(sc *scope, names []*syntax.Name, values syntax.Expr) {
	if values == nil {
		for _, n := range names {
			sc.define(n.Value, constant.MakeUnknown())
		}
		return
	}
	if lst, ok := values.(*syntax.ListExpr); ok {
		for i, n := range names {
			if n.Value == "_" || i >= len(lst.ElemList) {
				continue
			}
			sc.define(n.Value, in.evalExpr(sc, lst.ElemList[i]))
		}
		return
	}
	if len(names) != 1 {
		fail("forgo: multi-value declarations are not supported in comptime functions")
	}
	sc.define(names[0].Value, in.evalExpr(sc, values))
}

func (in *Interp) execAssign(sc *scope, a *syntax.AssignStmt) {
	name, ok := a.Lhs.(*syntax.Name)
	if !ok {
		fail("forgo: only simple identifier assignment is supported in comptime functions")
	}

	if a.Op == syntax.Def {
		sc.define(name.Value, in.evalExpr(sc, a.Rhs))
		return
	}

	if a.Rhs == nil {
		// i++ / i--
		cur, ok := sc.get(name.Value)
		if !ok {
			fail("forgo: undefined: %s", name.Value)
		}
		one := constant.MakeInt64(1)
		next := constant.BinaryOp(scalarConstant(cur, name.Value), tokenFor(a.Op), one)
		if !sc.assign(name.Value, next) {
			fail("forgo: undefined: %s", name.Value)
		}
		return
	}

	rhs := in.evalExpr(sc, a.Rhs)
	if a.Op == 0 {
		if !sc.assign(name.Value, rhs) {
			fail("forgo: undefined: %s", name.Value)
		}
		return
	}

	cur, ok := sc.get(name.Value)
	if !ok {
		fail("forgo: undefined: %s", name.Value)
	}
	next := constant.BinaryOp(scalarConstant(cur, name.Value), tokenFor(a.Op), scalarConstant(rhs, name.Value))
	if !sc.assign(name.Value, next) {
		fail("forgo: undefined: %s", name.Value)
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
		v, ok := obj.fields[x.Sel.Value]
		if !ok {
			fail("forgo: no field %q in comptime evaluation", x.Sel.Value)
		}
		return v

	case *syntax.IndexExpr:
		base := in.evalExpr(sc, x.X)
		switch b := base.(type) {
		case arrayVal:
			idx := scalarInt(in.evalExpr(sc, x.Index), "index expression")
			if idx < 0 || idx >= int64(len(b.elems)) {
				fail("forgo: index %d out of range in comptime evaluation (length %d)", idx, len(b.elems))
			}
			return b.elems[idx]
		case objectVal:
			key := scalarString(in.evalExpr(sc, x.Index), "index expression")
			v, ok := b.fields[key]
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
		return arrayVal{elems: elems}
	}
	if lit.NKeys != len(lit.ElemList) {
		fail("forgo: mixed keyed and unkeyed composite literal elements are not supported in comptime evaluation")
	}
	obj := objectVal{fields: map[string]Value{}}
	for _, e := range lit.ElemList {
		kv, ok := e.(*syntax.KeyValueExpr)
		if !ok {
			fail("forgo: unsupported composite literal element %T in comptime evaluation", e)
		}
		key := in.compositeLitKey(sc, kv.Key)
		if _, dup := obj.fields[key]; !dup {
			obj.keys = append(obj.keys, key)
		}
		obj.fields[key] = in.evalExpr(sc, kv.Value)
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
		switch op.Op {
		case syntax.Sub:
			return constant.UnaryOp(token.SUB, x, 0)
		case syntax.Add:
			return constant.UnaryOp(token.ADD, x, 0)
		case syntax.Not:
			return constant.MakeBool(!constant.BoolVal(x))
		case syntax.Xor:
			return constant.UnaryOp(token.XOR, x, 0)
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
	return constant.BinaryOp(x, tokenFor(op.Op), y)
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

	name, ok := fun.(*syntax.Name)
	if !ok {
		fail("forgo: unsupported function call in comptime function")
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

// HasNative reports whether pkg.fn is one of forgo's natively evaluated
// compile-time helpers, without actually invoking it. It lets callers (see
// types2's forgoEvalConstCall) tell a real forgo native call apart from an
// arbitrary function call that just happens to not be constant, before
// committing to evaluating it.
func (in *Interp) HasNative(pkg, fn string) bool {
	switch pkg + "." + fn {
	case "fmt.Sprintf", "strconv.Itoa",
		"embed.ReadFile", "embed.ReadFileRange", "embed.Exists",
		"embed.IsDir", "embed.ReadDir", "embed.Getwd",
		"json.Marshal", "json.Unmarshal":
		return true
	}
	return false
}

// nativeCall implements the fixed allow-list of package-qualified
// functions forgo evaluates natively at compile time, either as calls
// inside a //fgo:comptime function body (see evalStdlibCall) or, via a
// chain of field/index access, written directly as a const initializer
// (see types2's forgoEvalConstCall). pos is the call site, used by the
// "embed" (comptime/embed) helpers to resolve relative file paths.
func (in *Interp) nativeCall(pos syntax.Pos, pkg, fn string, args []Value) (Value, bool) {
	switch pkg + "." + fn {
	case "fmt.Sprintf":
		if len(args) == 0 {
			fail("forgo: fmt.Sprintf requires a format string")
		}
		format := scalarString(args[0], "fmt.Sprintf")
		rest := make([]any, len(args)-1)
		for i, a := range args[1:] {
			rest[i] = nativeValue(scalarConstant(a, "fmt.Sprintf"))
		}
		return constant.MakeString(fmt.Sprintf(format, rest...)), true

	case "strconv.Itoa":
		if len(args) != 1 {
			fail("forgo: strconv.Itoa takes exactly one argument")
		}
		return constant.MakeString(fmt.Sprintf("%d", scalarInt(args[0], "strconv.Itoa"))), true

	case "embed.ReadFile":
		path := scalarString(args[0], "embed.ReadFile")
		data, err := os.ReadFile(resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.ReadFile(%q): %s", path, err)
		}
		return constant.MakeString(string(data)), true

	case "embed.ReadFileRange":
		if len(args) != 3 {
			fail("forgo: embed.ReadFileRange takes exactly 3 arguments")
		}
		path := scalarString(args[0], "embed.ReadFileRange")
		offset := scalarInt(args[1], "embed.ReadFileRange")
		length := scalarInt(args[2], "embed.ReadFileRange")
		data, err := os.ReadFile(resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.ReadFileRange(%q): %s", path, err)
		}
		if offset < 0 || length < 0 || offset+length > int64(len(data)) {
			fail("forgo: embed.ReadFileRange(%q): range [%d:%d] out of bounds for %d-byte file", path, offset, offset+length, len(data))
		}
		return constant.MakeString(string(data[offset : offset+length])), true

	case "embed.Exists":
		path := scalarString(args[0], "embed.Exists")
		_, err := os.Stat(resolveEmbedPath(pos, path))
		return constant.MakeBool(err == nil), true

	case "embed.IsDir":
		path := scalarString(args[0], "embed.IsDir")
		info, err := os.Stat(resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.IsDir(%q): %s", path, err)
		}
		return constant.MakeBool(info.IsDir()), true

	case "embed.ReadDir":
		path := scalarString(args[0], "embed.ReadDir")
		entries, err := os.ReadDir(resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.ReadDir(%q): %s", path, err)
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		return constant.MakeString(strings.Join(names, "\n")), true

	case "embed.Getwd":
		if len(args) != 0 {
			fail("forgo: embed.Getwd takes no arguments")
		}
		wd, err := os.Getwd()
		if err != nil {
			fail("forgo: embed.Getwd: %s", err)
		}
		return constant.MakeString(wd), true

	case "json.Marshal":
		if len(args) != 1 {
			fail("forgo: json.Marshal takes exactly one argument")
		}
		s, err := marshalJSONValue(args[0])
		if err != nil {
			fail("forgo: json.Marshal: %s", err)
		}
		return constant.MakeString(s), true

	case "json.Unmarshal":
		if len(args) != 1 {
			fail("forgo: json.Unmarshal takes exactly one argument")
		}
		s := scalarString(args[0], "json.Unmarshal")
		var raw any
		if err := stdjson.Unmarshal([]byte(s), &raw); err != nil {
			fail("forgo: json.Unmarshal: %s", err)
		}
		return valueFromJSON(raw), true
	}
	return nil, false
}

// resolveEmbedPath resolves a relative path passed to one of the "embed"
// (comptime/embed) helpers against the directory of the source file
// containing the call, so `embed.ReadFile("data.txt")` reads the file next
// to the .go/.fgo file it's written in, not relative to whatever directory
// the compiler happens to be invoked from.
func resolveEmbedPath(pos syntax.Pos, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	dir := "."
	if base := pos.Base(); base != nil && base.Filename() != "" {
		dir = filepath.Dir(base.Filename())
	}
	return filepath.Join(dir, path)
}

// marshalJSONValue renders v (a scalar, or an objectVal/arrayVal built by
// a composite literal or another comptime call) as JSON text. Object keys
// are emitted in their original literal/decode order (see objectVal),
// matching struct-field-order marshaling rather than alphabetically
// sorting map keys the way encoding/json does for map[string]T.
func marshalJSONValue(v Value) (string, error) {
	switch x := v.(type) {
	case constant.Value:
		b, err := stdjson.Marshal(nativeValue(x))
		if err != nil {
			return "", err
		}
		return string(b), nil

	case objectVal:
		var buf strings.Builder
		buf.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := stdjson.Marshal(k)
			if err != nil {
				return "", err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			s, err := marshalJSONValue(x.fields[k])
			if err != nil {
				return "", err
			}
			buf.WriteString(s)
		}
		buf.WriteByte('}')
		return buf.String(), nil

	case arrayVal:
		var buf strings.Builder
		buf.WriteByte('[')
		for i, e := range x.elems {
			if i > 0 {
				buf.WriteByte(',')
			}
			s, err := marshalJSONValue(e)
			if err != nil {
				return "", err
			}
			buf.WriteString(s)
		}
		buf.WriteByte(']')
		return buf.String(), nil
	}
	return "", fmt.Errorf("unsupported value of type %T", v)
}

// valueFromJSON converts a value decoded by encoding/json (into `any`)
// into the interpreter's own Value representation. Object keys are
// sorted, since Go map iteration order (and so encoding/json's decoded
// map[string]any) is random.
func valueFromJSON(x any) Value {
	switch v := x.(type) {
	case nil:
		return constant.MakeUnknown()
	case bool:
		return constant.MakeBool(v)
	case float64:
		// encoding/json decodes every JSON number into `any` as float64,
		// even whole numbers like 8080. Represent it as an Int constant
		// when it's exactly representable as one, so it matches a real
		// int-typed struct field (e.g. `.Port`) the way a plain integer
		// literal in source would -- an untyped Float constant assigned
		// to an int-typed const would otherwise panic the compiler.
		if i := int64(v); float64(i) == v {
			return constant.MakeInt64(i)
		}
		return constant.MakeFloat64(v)
	case string:
		return constant.MakeString(v)
	case []any:
		elems := make([]Value, len(v))
		for i, e := range v {
			elems[i] = valueFromJSON(e)
		}
		return arrayVal{elems: elems}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make(map[string]Value, len(v))
		for _, k := range keys {
			fields[k] = valueFromJSON(v[k])
		}
		return objectVal{keys: keys, fields: fields}
	}
	fail("forgo: unsupported JSON value of type %T", x)
	panic("unreachable")
}

// scalarConstant asserts that v is a scalar (int/float/string/bool)
// value, failing with a message naming ctx (the argument, operand, or
// variable it came from) otherwise.
func scalarConstant(v Value, ctx string) constant.Value {
	cv, ok := v.(constant.Value)
	if !ok {
		fail("forgo: %s must be a scalar (int/float/string/bool) value in comptime evaluation, not a struct/slice/map", ctx)
	}
	return cv
}

func scalarString(v Value, ctx string) string {
	s, ok := nativeValue(scalarConstant(v, ctx)).(string)
	if !ok {
		fail("forgo: %s requires a string argument", ctx)
	}
	return s
}

func scalarInt(v Value, ctx string) int64 {
	n, ok := nativeValue(scalarConstant(v, ctx)).(int64)
	if !ok {
		fail("forgo: %s requires an integer argument", ctx)
	}
	return n
}

// ToConstant converts v (whatever EvalExprValue produced: a scalar, or an
// objectVal/arrayVal built by a composite literal or a call like
// json.Unmarshal) into a real go/constant.Value, recursively, using
// go/constant's Composite kind for objectVal/arrayVal. It's used by
// types2's forgoEvalConstCall to fold a const initializer that evaluates
// to a struct/map/slice, not just a scalar. It reports false if v is
// something ToConstant doesn't know how to represent (in practice, this
// shouldn't happen: every Value EvalExprValue can produce is one of the
// cases below).
func ToConstant(v Value) (constant.Value, bool) {
	switch x := v.(type) {
	case constant.Value:
		return x, true
	case objectVal:
		fields := make(map[string]constant.Value, len(x.fields))
		for k, fv := range x.fields {
			cv, ok := ToConstant(fv)
			if !ok {
				return nil, false
			}
			fields[k] = cv
		}
		return constant.MakeCompositeStruct(x.keys, fields), true
	case arrayVal:
		elems := make([]constant.Value, len(x.elems))
		for i, ev := range x.elems {
			cv, ok := ToConstant(ev)
			if !ok {
				return nil, false
			}
			elems[i] = cv
		}
		return constant.MakeCompositeArray(elems), true
	}
	return nil, false
}

func nativeValue(v constant.Value) any {
	switch v.Kind() {
	case constant.Bool:
		return constant.BoolVal(v)
	case constant.String:
		return constant.StringVal(v)
	case constant.Int:
		n, exact := constant.Int64Val(v)
		if !exact {
			fail("forgo: integer constant too large for compile-time evaluation")
		}
		return n
	case constant.Float:
		f, _ := constant.Float64Val(v)
		return f
	}
	fail("forgo: unsupported constant kind in comptime function")
	panic("unreachable")
}
