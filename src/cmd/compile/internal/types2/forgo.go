// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements forgo's //forgo:comptime const folding. It is called
// from exactly one place in decl.go (constDecl) and otherwise stays out of
// the way of the rest of the type checker: no Checker struct field is used
// (a side table keyed by *Checker holds the interpreter instead), so this
// feature can be added, changed, or removed without touching check.go.
package types2

import (
	"cmd/compile/internal/forgo"
	"cmd/compile/internal/syntax"
	"go/constant"
	. "internal/types/errors"
)

var forgoInterpByChecker = map[*Checker]*forgo.Interp{}

// forgoInterpreter returns the (lazily built) interpreter used to evaluate
// //forgo:comptime function calls appearing in constant declarations.
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

// forgoEvalConstCall tries to evaluate init as a call to a //forgo:comptime
// function with constant-foldable arguments. It reports whether it handled
// init at all; x is only updated to a constant operand on success.
func forgoEvalConstCall(check *Checker, x *operand, init syntax.Expr) bool {
	call, ok := init.(*syntax.CallExpr)
	if !ok {
		return false
	}
	fname, ok := call.Fun.(*syntax.Name)
	if !ok {
		return false
	}
	in := forgoInterpreter(check)
	fdecl, ok := in.Funcs[fname.Value]
	if !ok {
		return false
	}

	args := make([]constant.Value, len(call.ArgList))
	for i, a := range call.ArgList {
		v, err := in.EvalConstExpr(a)
		if err != nil {
			// Argument isn't constant-foldable by forgo; let normal
			// type-checking report whatever error is appropriate.
			return false
		}
		args[i] = v
	}

	result, err := in.EvalComptime(fdecl, args)
	if err != nil {
		check.errorf(init, InvalidConstInit, "compile-time evaluation of %s failed: %s", fname.Value, err)
		x.mode = invalid
		return true
	}

	x.mode = constant_
	x.val = result
	return true
}
