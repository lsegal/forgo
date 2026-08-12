// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package forgo implements compile-time execution ("comptime" functions) and
// AST macros for the forgo language, a fork of Go.
//
// A function marked with the //forgo:comptime pragma is ordinary Go that is
// additionally interpretable by this package, so it can be invoked directly
// inside a const declaration's initializer and its result folded into a
// constant at compile time (see cmd/compile/internal/types2's constDecl).
//
// A function marked with the //forgo:macro pragma receives the *unevaluated*
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
	"os"
	"path/filepath"
	"strings"
)

// Value is either a go/constant.Value (comptime evaluation) or a NodeVal
// (macro evaluation).
type Value interface{}

// NodeVal wraps a syntax tree produced or manipulated by a macro.
type NodeVal struct {
	Node syntax.Node
}

// Interp evaluates //forgo:comptime and //forgo:macro function bodies.
type Interp struct {
	// Funcs holds every //forgo:comptime/macro function declared in the
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

// EvalComptime runs a //forgo:comptime function with the given already
// constant-folded arguments and returns its result as a constant.Value.
func (in *Interp) EvalComptime(fn *syntax.FuncDecl, args []constant.Value) (result constant.Value, err error) {
	vargs := make([]Value, len(args))
	for i, a := range args {
		vargs[i] = a
	}
	v, err := in.call(fn, vargs)
	if err != nil {
		return nil, err
	}
	cv, ok := v.(constant.Value)
	if !ok {
		return nil, fmt.Errorf("%s did not return a constant value", fn.Name.Value)
	}
	return cv, nil
}

// EvalMacro runs a //forgo:macro function, binding its parameters to the
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

// EvalConstExpr evaluates a self-contained constant expression (literals,
// parenthesization, unary/binary operators, and calls to other
// //forgo:comptime functions) with no surrounding variable scope. It is used
// to fold the arguments of a comptime call written directly in source, e.g.
// in a const declaration's initializer.
func (in *Interp) EvalConstExpr(e syntax.Expr) (result constant.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(runtimeErr); ok {
				err = re.err
				return
			}
			panic(r)
		}
	}()
	v := in.evalExpr(newScope(nil), e)
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
		next := constant.BinaryOp(cur.(constant.Value), tokenFor(a.Op), one)
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
	next := constant.BinaryOp(cur.(constant.Value), tokenFor(a.Op), rhs.(constant.Value))
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
	}
	fail("forgo: unsupported expression %T in comptime function", e)
	panic("unreachable")
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
		x := in.evalExpr(sc, op.X).(constant.Value)
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

	x := in.evalExpr(sc, op.X).(constant.Value)
	y := in.evalExpr(sc, op.Y).(constant.Value)

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

	if sel, ok := call.Fun.(*syntax.SelectorExpr); ok {
		if pkg, ok := sel.X.(*syntax.Name); ok {
			if v, handled := in.evalStdlibCall(sc, call.Pos(), pkg.Value, sel.Sel.Value, call.ArgList); handled {
				return v
			}
		}
	}

	name, ok := call.Fun.(*syntax.Name)
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

	fail("forgo: %s is not a //forgo:comptime function and cannot be called at compile time", name.Value)
	panic("unreachable")
}

// evalStdlibCall implements a small, fixed allow-list of standard library
// (and forgo-provided compile-time helper) functions natively, so comptime
// functions can format results the same way Nim's compileTime procs can use
// std/strformat. pos is the call site, used by the "embed" (comptime/embed)
// helpers to resolve relative file paths.
func (in *Interp) evalStdlibCall(sc *scope, pos syntax.Pos, pkg, fn string, argList []syntax.Expr) (Value, bool) {
	args := make([]any, len(argList))
	for i, a := range argList {
		args[i] = nativeValue(in.evalExpr(sc, a).(constant.Value))
	}
	return in.nativeCall(pos, pkg, fn, args)
}

// HasNative reports whether pkg.fn is one of forgo's natively evaluated
// compile-time helpers, without actually invoking it. It lets callers (see
// types2's forgoEvalConstCall) tell a real forgo native call apart from an
// arbitrary function call that just happens to not be constant, before
// committing to evaluating its arguments as constants.
func (in *Interp) HasNative(pkg, fn string) bool {
	switch pkg + "." + fn {
	case "fmt.Sprintf", "strconv.Itoa",
		"embed.ReadFile", "embed.ReadFileRange", "embed.Exists",
		"embed.IsDir", "embed.ReadDir", "embed.Getwd":
		return true
	}
	return false
}

// EvalNativeConst evaluates a call to one of forgo's native compile-time
// helpers (see nativeCall) written directly as a const initializer, e.g.
// `const data = embed.ReadFile("x.txt")`. Callers should check HasNative
// first; EvalNativeConst returns an error if pkg.fn isn't actually native.
func (in *Interp) EvalNativeConst(pos syntax.Pos, pkg, fn string, args []constant.Value) (result constant.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(runtimeErr); ok {
				err = re.err
				return
			}
			panic(r)
		}
	}()
	nargs := make([]any, len(args))
	for i, a := range args {
		nargs[i] = nativeValue(a)
	}
	v, handled := in.nativeCall(pos, pkg, fn, nargs)
	if !handled {
		return nil, fmt.Errorf("%s.%s is not a comptime-callable function", pkg, fn)
	}
	return v.(constant.Value), nil
}

// nativeCall is the shared implementation behind evalStdlibCall (nested
// calls inside a //forgo:comptime function body) and EvalNativeConst (calls
// written directly as a const initializer).
func (in *Interp) nativeCall(pos syntax.Pos, pkg, fn string, args []any) (Value, bool) {
	switch pkg + "." + fn {
	case "fmt.Sprintf":
		if len(args) == 0 {
			fail("forgo: fmt.Sprintf requires a format string")
		}
		format, ok := args[0].(string)
		if !ok {
			fail("forgo: fmt.Sprintf format must be a string")
		}
		return constant.MakeString(fmt.Sprintf(format, args[1:]...)), true

	case "strconv.Itoa":
		if len(args) != 1 {
			fail("forgo: strconv.Itoa takes exactly one argument")
		}
		n, ok := args[0].(int64)
		if !ok {
			fail("forgo: strconv.Itoa requires an integer argument")
		}
		return constant.MakeString(fmt.Sprintf("%d", n)), true

	case "embed.ReadFile":
		path := nativeStringArg(args, 0, "embed.ReadFile")
		data, err := os.ReadFile(resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.ReadFile(%q): %s", path, err)
		}
		return constant.MakeString(string(data)), true

	case "embed.ReadFileRange":
		if len(args) != 3 {
			fail("forgo: embed.ReadFileRange takes exactly 3 arguments")
		}
		path := nativeStringArg(args, 0, "embed.ReadFileRange")
		offset := nativeIntArg(args, 1, "embed.ReadFileRange")
		length := nativeIntArg(args, 2, "embed.ReadFileRange")
		data, err := os.ReadFile(resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.ReadFileRange(%q): %s", path, err)
		}
		if offset < 0 || length < 0 || offset+length > int64(len(data)) {
			fail("forgo: embed.ReadFileRange(%q): range [%d:%d] out of bounds for %d-byte file", path, offset, offset+length, len(data))
		}
		return constant.MakeString(string(data[offset : offset+length])), true

	case "embed.Exists":
		path := nativeStringArg(args, 0, "embed.Exists")
		_, err := os.Stat(resolveEmbedPath(pos, path))
		return constant.MakeBool(err == nil), true

	case "embed.IsDir":
		path := nativeStringArg(args, 0, "embed.IsDir")
		info, err := os.Stat(resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.IsDir(%q): %s", path, err)
		}
		return constant.MakeBool(info.IsDir()), true

	case "embed.ReadDir":
		path := nativeStringArg(args, 0, "embed.ReadDir")
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

func nativeStringArg(args []any, i int, name string) string {
	if i >= len(args) {
		fail("forgo: %s requires a string argument", name)
	}
	s, ok := args[i].(string)
	if !ok {
		fail("forgo: %s requires a string argument", name)
	}
	return s
}

func nativeIntArg(args []any, i int, name string) int64 {
	if i >= len(args) {
		fail("forgo: %s requires an integer argument", name)
	}
	n, ok := args[i].(int64)
	if !ok {
		fail("forgo: %s requires an integer argument", name)
	}
	return n
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
