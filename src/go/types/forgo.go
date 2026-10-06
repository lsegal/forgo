// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements forgo's //fgo:comptime const folding, mirroring
// cmd/compile/internal/types2/forgo.go so that tools built on go/types
// (cmd/vet in particular) see the same constants the compiler does. It is
// called from exactly one place in decl.go (constDecl) and otherwise stays
// out of the way of the rest of the type checker: no Checker struct field
// is used (a side table keyed by *Checker holds the interpreter instead).
//
// //fgo:comptime directives are read from function doc comments, so the
// files must have been parsed with parser.ParseComments. //fgo:macro calls
// are not handled here: like the compiler, callers expand them before type
// checking (see internal/forgo.ExpandMacros).
package types

import (
	"go/ast"
	"internal/forgo"
	. "internal/types/errors"
	"sync"
)

var forgoInterpByChecker sync.Map // *Checker -> *forgo.Interp

// forgoInterpreter returns the (lazily built) interpreter used to evaluate
// //fgo:comptime function calls appearing in constant declarations.
func forgoInterpreter(check *Checker) *forgo.Interp {
	if in, ok := forgoInterpByChecker.Load(check); ok {
		return in.(*forgo.Interp)
	}
	in := &forgo.Interp{Funcs: map[string]*ast.FuncDecl{}, Fset: check.fset}
	for _, file := range check.files {
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			if forgo.IsComptime(fd) {
				in.Funcs[fd.Name.Name] = fd
			}
		}
	}
	forgoInterpByChecker.Store(check, in)
	return in
}

// forgoForgetChecker drops check's interpreter, if any, once checking is
// done, so the side table does not keep every Checker alive.
func forgoForgetChecker(check *Checker) {
	forgoInterpByChecker.Delete(check)
}

// forgoEvalConstCall tries to evaluate init as a compile-time expression:
// a call to a //fgo:comptime function declared in this package (e.g.
// `f(...)`), a call to one of forgo's native compile-time helpers (a
// package-qualified call, e.g. `embed.ReadFile(...)`, optionally
// generic-instantiated like `json.Unmarshal[Config](...)`), or a chain of
// field selection/indexing on top of one of those (e.g.
// `json.Unmarshal[Config](s).Name`). It reports whether it handled init at
// all; x is only updated to a constant operand on success.
func forgoEvalConstCall(check *Checker, x *operand, init ast.Expr) bool {
	call := forgoRootCall(init)
	if call == nil {
		return false
	}
	in := forgoInterpreter(check)

	fun := ast.Unparen(call.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = ix.X // generic instantiation, e.g. json.Unmarshal[Config]
	case *ast.IndexListExpr:
		fun = ix.X
	}

	switch f := fun.(type) {
	case *ast.Ident:
		if _, ok := in.Funcs[f.Name]; !ok {
			return false
		}
	case *ast.SelectorExpr:
		pkg, ok := f.X.(*ast.Ident)
		if !ok || !in.HasNative(pkg.Name, f.Sel.Name) {
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
func forgoRootCall(e ast.Expr) *ast.CallExpr {
	for {
		switch x := e.(type) {
		case *ast.CallExpr:
			return x
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return nil
		}
	}
}

// forgoUseErrorsImport marks the "errors" import of the file containing s,
// a `throw "..."` statement, as used: the compiler lowers that statement to
// a call to errors.New through the file's existing import (see
// cmd/compile/internal/noder/forgo_throw.go), so the import must not be
// reported as unused. Like the compiler, it reports an error if the file
// does not import "errors".
func forgoUseErrorsImport(check *Checker, s *ast.ThrowStmt) {
	file := check.fset.File(s.Pos())
	for _, pkgName := range check.imports {
		if pkgName.imported.path == "errors" && check.fset.File(pkgName.pos) == file {
			check.usedPkgNames[pkgName] = true
			return
		}
	}
	check.error(s, InvalidSyntaxTree, `forgo: throw "..." requires this file to already import "errors"`)
}
