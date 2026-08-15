// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements forgo's //fgo:comptime const folding. It is called
// from exactly one place in decl.go (constDecl) and otherwise stays out of
// the way of the rest of the type checker: no Checker struct field is used
// (a side table keyed by *Checker holds the interpreter instead), so this
// feature can be added, changed, or removed without touching check.go.
package types2

import (
	"cmd/compile/internal/forgo"
	"cmd/compile/internal/syntax"
	. "internal/types/errors"
)

var forgoInterpByChecker = map[*Checker]*forgo.Interp{}

// forgoInterpreter returns the (lazily built) interpreter used to evaluate
// //fgo:comptime function calls appearing in constant declarations.
func forgoInterpreter(check *Checker) *forgo.Interp {
	if in, ok := forgoInterpByChecker[check]; ok {
		return in
	}
	in := &forgo.Interp{Funcs: map[string]*syntax.FuncDecl{}}
	for _, file := range check.files {
		for _, d := range file.DeclList {
			fd, ok := d.(*syntax.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			if fp, ok := fd.Pragma.(syntax.ForgoPragma); ok && fp.ForgoComptime() {
				in.Funcs[fd.Name.Value] = fd
			}
		}
	}
	forgoInterpByChecker[check] = in
	return in
}

// forgoEvalConstCall tries to evaluate init as a compile-time expression:
// a call to a //fgo:comptime function declared in this package (e.g.
// `f(...)`), a call to one of forgo's native compile-time helpers (a
// package-qualified call, e.g. `embed.ReadFile(...)`, optionally
// generic-instantiated like `json.Unmarshal[Config](...)`), or a chain of
// field selection/indexing on top of one of those (e.g.
// `json.Unmarshal[Config](s).Name`). It reports whether it handled init at
// all; x is only updated to a constant operand on success.
func forgoEvalConstCall(check *Checker, x *operand, init syntax.Expr) bool {
	call := forgoRootCall(init)
	if call == nil {
		return false
	}
	in := forgoInterpreter(check)

	fun := call.Fun
	if ix, ok := fun.(*syntax.IndexExpr); ok {
		fun = ix.X // generic instantiation, e.g. json.Unmarshal[Config]
	}

	switch f := fun.(type) {
	case *syntax.Name:
		if _, ok := in.Funcs[f.Value]; !ok {
			return false
		}
	case *syntax.SelectorExpr:
		pkg, ok := f.X.(*syntax.Name)
		if !ok || !in.HasNative(pkg.Value, f.Sel.Value) {
			return false
		}
	default:
		return false
	}

	v, err := in.EvalExprValue(init)
	if err != nil {
		check.errorf(init, InvalidConstInit, "compile-time evaluation failed: %s", err)
		x.mode_ = invalid
		return true
	}
	// v is either a scalar or a struct/map/slice composite (see
	// forgo.ToConstant); either way it folds into a real go/constant.Value
	// -- x.typ is already whatever check.expr inferred for init from the
	// real (possibly generic) function signatures involved, e.g. Schema
	// for json.Unmarshal[Schema](...), so it's left untouched here.
	cv, ok := forgo.ToConstant(v)
	if !ok {
		check.error(init, InvalidConstInit, "compile-time evaluation did not produce a usable constant value")
		x.mode_ = invalid
		return true
	}

	x.mode_ = constant_
	x.val = cv
	return true
}

// forgoRootCall walks down through chained field selection (.Field) and
// indexing ([i] / ["key"]) to find the call expression underneath, e.g.
// returning the CallExpr for `f(...).Field[0]`. It returns nil if init
// isn't (a chain rooted at) a call at all, so forgoEvalConstCall can
// quickly decline anything that couldn't possibly be a forgo comptime
// expression and let normal type-checking report its own error.
func forgoRootCall(e syntax.Expr) *syntax.CallExpr {
	for {
		switch x := e.(type) {
		case *syntax.CallExpr:
			return x
		case *syntax.SelectorExpr:
			e = x.X
		case *syntax.IndexExpr:
			e = x.X
		default:
			return nil
		}
	}
}
