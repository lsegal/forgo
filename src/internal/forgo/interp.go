// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package forgo implements forgo's compile-time execution ("comptime"
// functions) and AST macros over go/ast, for go/types and the tools built
// on it (such as cmd/vet). It is a port of cmd/compile/internal/forgo,
// which does the same over cmd/compile/internal/syntax for the compiler;
// keep the two in sync.
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
	stdjson "encoding/json"
	"fmt"
	"go/ast"
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
type Value any

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
// parenthesization, unary/binary operators, composite literals, and calls
// to other //fgo:comptime functions or forgo's native compile-time
// helpers, optionally followed by field selection/indexing) with no
// surrounding variable scope, returning whatever Value it produces: a
// scalar (constant.Value), or a struct/slice/map composite value. It's
// used to fold a compile-time expression written directly in source, e.g.
// a const declaration's initializer.
func (in *Interp) EvalExprValue(e ast.Expr) (result Value, err error) {
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

func (in *Interp) call(fn *ast.FuncDecl, args []Value) (val Value, err error) {
	if fn.Body == nil {
		return nil, fmt.Errorf("%s has no body", fn.Name.Name)
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
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, name := range field.Names {
			if i < len(args) {
				sc.define(name.Name, args[i])
			}
			i++
		}
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

func (in *Interp) execBlock(parent *scope, b *ast.BlockStmt) {
	sc := newScope(parent)
	for _, s := range b.List {
		in.execStmt(sc, s)
	}
}

func (in *Interp) execStmt(sc *scope, s ast.Stmt) {
	switch x := s.(type) {
	case *ast.BlockStmt:
		in.execBlock(sc, x)

	case *ast.EmptyStmt:
		// no-op

	case *ast.ExprStmt:
		in.evalExpr(sc, x.X)

	case *ast.DeclStmt:
		in.execLocalDecl(sc, x.Decl)

	case *ast.AssignStmt:
		in.execAssign(sc, x)

	case *ast.IncDecStmt:
		name, ok := x.X.(*ast.Ident)
		if !ok {
			fail("forgo: only simple identifier assignment is supported in comptime functions")
		}
		cur, ok := sc.get(name.Name)
		if !ok {
			fail("forgo: undefined: %s", name.Name)
		}
		op := token.ADD
		if x.Tok == token.DEC {
			op = token.SUB
		}
		next := constant.BinaryOp(scalarConstant(cur, name.Name), op, constant.MakeInt64(1))
		if !sc.assign(name.Name, next) {
			fail("forgo: undefined: %s", name.Name)
		}

	case *ast.ReturnStmt:
		var v Value
		switch len(x.Results) {
		case 0:
		case 1:
			v = in.evalExpr(sc, x.Results[0])
		default:
			fail("forgo: multi-value returns are not supported in comptime functions")
		}
		panic(returnSignal{v})

	case *ast.IfStmt:
		child := newScope(sc)
		if x.Init != nil {
			in.execStmt(child, x.Init)
		}
		if in.evalBool(child, x.Cond) {
			in.execBlock(child, x.Body)
		} else if x.Else != nil {
			in.execStmt(child, x.Else)
		}

	case *ast.RangeStmt:
		fail("forgo: range-form for loops are not supported in comptime functions")

	case *ast.ForStmt:
		child := newScope(sc)
		if x.Init != nil {
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

func (in *Interp) execLocalDecl(sc *scope, d ast.Decl) {
	gd, ok := d.(*ast.GenDecl)
	if !ok || (gd.Tok != token.VAR && gd.Tok != token.CONST) {
		fail("forgo: unsupported local declaration %T in comptime function", d)
	}
	for _, spec := range gd.Specs {
		vs := spec.(*ast.ValueSpec)
		in.bindNames(sc, vs.Names, vs.Values)
	}
}

func (in *Interp) bindNames(sc *scope, names []*ast.Ident, values []ast.Expr) {
	if len(values) == 0 {
		for _, n := range names {
			sc.define(n.Name, constant.MakeUnknown())
		}
		return
	}
	if len(values) == 1 && len(names) != 1 {
		fail("forgo: multi-value declarations are not supported in comptime functions")
	}
	for i, n := range names {
		if n.Name == "_" || i >= len(values) {
			continue
		}
		sc.define(n.Name, in.evalExpr(sc, values[i]))
	}
}

func (in *Interp) execAssign(sc *scope, a *ast.AssignStmt) {
	if len(a.Lhs) != 1 || len(a.Rhs) != 1 {
		fail("forgo: multi-value assignments are not supported in comptime functions")
	}
	name, ok := a.Lhs[0].(*ast.Ident)
	if !ok {
		fail("forgo: only simple identifier assignment is supported in comptime functions")
	}

	if a.Tok == token.DEFINE {
		sc.define(name.Name, in.evalExpr(sc, a.Rhs[0]))
		return
	}

	rhs := in.evalExpr(sc, a.Rhs[0])
	if a.Tok == token.ASSIGN {
		if !sc.assign(name.Name, rhs) {
			fail("forgo: undefined: %s", name.Name)
		}
		return
	}

	cur, ok := sc.get(name.Name)
	if !ok {
		fail("forgo: undefined: %s", name.Name)
	}
	next := constant.BinaryOp(scalarConstant(cur, name.Name), assignOp(a.Tok), scalarConstant(rhs, name.Name))
	if !sc.assign(name.Name, next) {
		fail("forgo: undefined: %s", name.Name)
	}
}

// assignOp returns the binary operator of an op= assignment token.
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
	case token.AND_NOT_ASSIGN:
		return token.AND_NOT
	case token.SHL_ASSIGN:
		return token.SHL
	case token.SHR_ASSIGN:
		return token.SHR
	}
	fail("forgo: unsupported operator in comptime function")
	panic("unreachable")
}

// ---------------------------------------------------------------------------
// Expression evaluation (comptime / constant-value mode)

func (in *Interp) evalBool(sc *scope, e ast.Expr) bool {
	v := in.evalExpr(sc, e)
	cv, ok := v.(constant.Value)
	if !ok || cv.Kind() != constant.Bool {
		fail("forgo: condition is not a boolean constant")
	}
	return constant.BoolVal(cv)
}

func (in *Interp) evalExpr(sc *scope, e ast.Expr) Value {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return in.evalExpr(sc, x.X)

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
		return literalValue(x)

	case *ast.UnaryExpr:
		return in.evalUnary(sc, x)

	case *ast.BinaryExpr:
		return in.evalBinary(sc, x)

	case *ast.CallExpr:
		return in.evalCall(sc, x)

	case *ast.CompositeLit:
		return in.evalCompositeLit(sc, x)

	case *ast.SelectorExpr:
		base := in.evalExpr(sc, x.X)
		obj, ok := base.(objectVal)
		if !ok {
			fail("forgo: cannot select field %q from a non-struct/map value in comptime evaluation", x.Sel.Name)
		}
		v, ok := obj.fields[x.Sel.Name]
		if !ok {
			fail("forgo: no field %q in comptime evaluation", x.Sel.Name)
		}
		return v

	case *ast.IndexExpr:
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
func (in *Interp) evalCompositeLit(sc *scope, lit *ast.CompositeLit) Value {
	nkeys := 0
	for _, e := range lit.Elts {
		if _, ok := e.(*ast.KeyValueExpr); ok {
			nkeys++
		}
	}
	if nkeys == 0 {
		elems := make([]Value, len(lit.Elts))
		for i, e := range lit.Elts {
			elems[i] = in.evalExpr(sc, e)
		}
		return arrayVal{elems: elems}
	}
	if nkeys != len(lit.Elts) {
		fail("forgo: mixed keyed and unkeyed composite literal elements are not supported in comptime evaluation")
	}
	obj := objectVal{fields: map[string]Value{}}
	for _, e := range lit.Elts {
		kv := e.(*ast.KeyValueExpr)
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
func (in *Interp) compositeLitKey(sc *scope, k ast.Expr) string {
	if name, ok := k.(*ast.Ident); ok {
		if _, bound := sc.get(name.Name); !bound {
			return name.Name
		}
	}
	return scalarString(in.evalExpr(sc, k), "composite literal key")
}

func literalValue(lit *ast.BasicLit) constant.Value {
	switch lit.Kind {
	case token.INT, token.FLOAT, token.CHAR, token.STRING:
	default:
		fail("forgo: unsupported literal kind in comptime function")
	}
	v := constant.MakeFromLiteral(lit.Value, lit.Kind, 0)
	if v.Kind() == constant.Unknown {
		fail("forgo: invalid literal %q in comptime function", lit.Value)
	}
	return v
}

func (in *Interp) evalUnary(sc *scope, op *ast.UnaryExpr) Value {
	x := scalarConstant(in.evalExpr(sc, op.X), "unary operand")
	switch op.Op {
	case token.SUB, token.ADD, token.XOR:
		return constant.UnaryOp(op.Op, x, 0)
	case token.NOT:
		return constant.MakeBool(!constant.BoolVal(x))
	}
	fail("forgo: unsupported unary operator in comptime function")
	panic("unreachable")
}

func (in *Interp) evalBinary(sc *scope, op *ast.BinaryExpr) Value {
	switch op.Op {
	case token.LAND:
		if !in.evalBool(sc, op.X) {
			return constant.MakeBool(false)
		}
		return constant.MakeBool(in.evalBool(sc, op.Y))
	case token.LOR:
		if in.evalBool(sc, op.X) {
			return constant.MakeBool(true)
		}
		return constant.MakeBool(in.evalBool(sc, op.Y))
	}

	x := scalarConstant(in.evalExpr(sc, op.X), "operand")
	y := scalarConstant(in.evalExpr(sc, op.Y), "operand")

	switch op.Op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return constant.MakeBool(constant.Compare(x, op.Op, y))
	case token.ADD, token.SUB, token.MUL, token.QUO, token.REM,
		token.AND, token.OR, token.XOR, token.AND_NOT:
		return constant.BinaryOp(x, op.Op, y)
	case token.SHL, token.SHR:
		n := scalarInt(y, "shift count")
		if n < 0 {
			fail("forgo: negative shift count in comptime function")
		}
		return constant.Shift(x, op.Op, uint(n))
	}
	fail("forgo: unsupported operator in comptime function")
	panic("unreachable")
}

func (in *Interp) evalCall(sc *scope, call *ast.CallExpr) Value {
	if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "Quote" {
		return in.evalQuote(sc, call)
	}

	// Unwrap generic instantiation syntax (f[T](args...), or
	// f[T1, T2](args...)). The interpreter has no type information, so it
	// ignores the type argument(s) entirely -- see comptime/json's
	// Unmarshal[T] for why that's safe here.
	fun := ast.Unparen(call.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = ix.X
	case *ast.IndexListExpr:
		fun = ix.X
	}

	if sel, ok := fun.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok {
			if v, handled := in.evalStdlibCall(sc, call.Pos(), pkg.Name, sel.Sel.Name, call.Args); handled {
				return v
			}
		}
	}

	name, ok := fun.(*ast.Ident)
	if !ok {
		fail("forgo: unsupported function call in comptime function")
	}

	if fn, ok := in.Funcs[name.Name]; ok {
		args := make([]Value, len(call.Args))
		for i, a := range call.Args {
			args[i] = in.evalExpr(sc, a)
		}
		v, err := in.call(fn, args)
		if err != nil {
			fail("%s", err.Error())
		}
		return v
	}

	fail("forgo: %s is not a //fgo:comptime function and cannot be called at compile time", name.Name)
	panic("unreachable")
}

// evalStdlibCall implements a small, fixed allow-list of standard library
// (and forgo-provided compile-time helper) functions natively, so comptime
// functions can format results the same way Nim's compileTime procs can use
// std/strformat. pos is the call site, used by the "embed" (comptime/embed)
// helpers to resolve relative file paths.
func (in *Interp) evalStdlibCall(sc *scope, pos token.Pos, pkg, fn string, argList []ast.Expr) (Value, bool) {
	args := make([]Value, len(argList))
	for i, a := range argList {
		args[i] = in.evalExpr(sc, a)
	}
	return in.nativeCall(pos, pkg, fn, args)
}

// HasNative reports whether pkg.fn is one of forgo's natively evaluated
// compile-time helpers, without actually invoking it. It lets callers (see
// go/types' forgoEvalConstCall) tell a real forgo native call apart from an
// arbitrary function call that just happens to not be constant, before
// committing to evaluating it.
func (in *Interp) HasNative(pkg, fn string) bool {
	switch pkg + "." + fn {
	case "fmt.Sprintf", "strconv.Itoa",
		"embed.ReadFile", "embed.ReadFileRange", "embed.Exists",
		"embed.IsDir", "embed.ReadDir", "embed.Getwd", "embed.Load",
		"json.Marshal", "json.Unmarshal":
		return true
	}
	return false
}

// nativeCall implements the fixed allow-list of package-qualified
// functions forgo evaluates natively at compile time, either as calls
// inside a //fgo:comptime function body (see evalStdlibCall) or, via a
// chain of field/index access, written directly as a const initializer
// (see go/types' forgoEvalConstCall). pos is the call site, used by the
// "embed" (comptime/embed) helpers to resolve relative file paths.
func (in *Interp) nativeCall(pos token.Pos, pkg, fn string, args []Value) (Value, bool) {
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
		data, err := os.ReadFile(in.resolveEmbedPath(pos, path))
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
		data, err := os.ReadFile(in.resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.ReadFileRange(%q): %s", path, err)
		}
		if offset < 0 || length < 0 || offset+length > int64(len(data)) {
			fail("forgo: embed.ReadFileRange(%q): range [%d:%d] out of bounds for %d-byte file", path, offset, offset+length, len(data))
		}
		return constant.MakeString(string(data[offset : offset+length])), true

	case "embed.Exists":
		path := scalarString(args[0], "embed.Exists")
		_, err := os.Stat(in.resolveEmbedPath(pos, path))
		return constant.MakeBool(err == nil), true

	case "embed.IsDir":
		path := scalarString(args[0], "embed.IsDir")
		info, err := os.Stat(in.resolveEmbedPath(pos, path))
		if err != nil {
			fail("forgo: embed.IsDir(%q): %s", path, err)
		}
		return constant.MakeBool(info.IsDir()), true

	case "embed.ReadDir":
		path := scalarString(args[0], "embed.ReadDir")
		entries, err := os.ReadDir(in.resolveEmbedPath(pos, path))
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

	case "embed.Load":
		if len(args) == 0 {
			fail("forgo: embed.Load requires at least one pattern")
		}
		patterns := make([]string, len(args))
		for i, a := range args {
			patterns[i] = scalarString(a, "embed.Load")
		}
		return in.loadEmbedFS(pos, patterns), true

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
// the type checker happens to be invoked from.
func (in *Interp) resolveEmbedPath(pos token.Pos, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	dir := "."
	if in.Fset != nil && pos.IsValid() {
		if name := in.Fset.PositionFor(pos, false).Filename; name != "" {
			dir = filepath.Dir(name)
		}
	}
	return filepath.Join(dir, path)
}

// loadEmbedFS implements embed.Load: it walks patterns (each either a
// plain file path or a directory, resolved against the call site's
// source directory the same way resolveEmbedPath does) and builds an
// objectVal shaped like comptime/embed's FS{files []file{name, data}}
// struct -- an unexported "files" field holding an arrayVal of
// name/data objectVals, sorted by (dir, elem) exactly as
// comptime/embed's split/lookup/readDir expect, so the resulting
// constant materializes into a real, queryable FS value (see
// the compiler's staticdata slice-composite handling in InitConst).
//
// A directory pattern embeds every file in its subtree, skipping names
// beginning with '.' or '_', matching the `//go:embed` directive's
// rules; a plain path embeds exactly that file.
func (in *Interp) loadEmbedFS(pos token.Pos, patterns []string) Value {
	type embedFile struct{ name, data string }
	var files []embedFile
	seen := map[string]bool{}

	var addFile func(fsPath, diskPath string)
	addFile = func(fsPath, diskPath string) {
		if seen[fsPath] {
			return
		}
		seen[fsPath] = true
		data, err := os.ReadFile(diskPath)
		if err != nil {
			fail("forgo: embed.Load: %s", err)
		}
		files = append(files, embedFile{name: fsPath, data: string(data)})
	}
	var walkDir func(diskDir, fsDir string)
	walkDir = func(diskDir, fsDir string) {
		entries, err := os.ReadDir(diskDir)
		if err != nil {
			fail("forgo: embed.Load: %s", err)
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			diskPath := filepath.Join(diskDir, name)
			fsPath := fsDir + "/" + name
			if e.IsDir() {
				walkDir(diskPath, fsPath)
				continue
			}
			addFile(fsPath, diskPath)
		}
	}

	for _, p := range patterns {
		diskPath := in.resolveEmbedPath(pos, p)
		info, err := os.Stat(diskPath)
		if err != nil {
			fail("forgo: embed.Load(%q): %s", p, err)
		}
		if info.IsDir() {
			walkDir(diskPath, p)
		} else {
			addFile(p, diskPath)
		}
	}

	// Add a directory marker entry (name ending in '/', no data) for
	// every ancestor directory of every file, mirroring
	// comptime/embed's addDirEntries: without it, FS.lookup/readDir
	// can't find/list "template" as a directory, only its leaf files.
	dirSeen := map[string]bool{}
	var dirs []embedFile
	for _, f := range files {
		for dir, _, _ := splitEmbedName(f.name); dir != "."; {
			if !dirSeen[dir] {
				dirSeen[dir] = true
				dirs = append(dirs, embedFile{name: dir + "/"})
			}
			dir, _, _ = splitEmbedName(dir)
		}
	}
	files = append(files, dirs...)

	sort.Slice(files, func(i, j int) bool {
		idir, ielem, _ := splitEmbedName(files[i].name)
		jdir, jelem, _ := splitEmbedName(files[j].name)
		if idir != jdir {
			return idir < jdir
		}
		return ielem < jelem
	})

	elems := make([]Value, len(files))
	for i, f := range files {
		elems[i] = objectVal{
			keys: []string{"name", "data"},
			fields: map[string]Value{
				"name": constant.MakeString(f.name),
				"data": constant.MakeString(f.data),
			},
		}
	}
	return objectVal{
		keys:   []string{"files"},
		fields: map[string]Value{"files": arrayVal{elems: elems}},
	}
}

// splitEmbedName mirrors comptime/embed's split: it separates name into
// (dir, elem), taking dir to be "." at the root, and reports whether
// name ended in a trailing slash (a directory entry). It's used to
// reproduce the FS files list's (dir, elem) sort order here at compile
// time.
func splitEmbedName(name string) (dir, elem string, isDir bool) {
	name, isDir = strings.CutSuffix(name, "/")
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return ".", name, isDir
	}
	return name[:i], name[i+1:], isDir
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
// go/types' forgoEvalConstCall to fold a const initializer that evaluates
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
