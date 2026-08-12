// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
)

// forgoLowerThrow rewrites every use of the forgo `throw` statement (parsed
// as *syntax.ThrowStmt) into an ordinary `return`, so the rest of the
// compiler never has to know about it. It must run after parsing (and
// after macro expansion) and before `?` lowering, since a thrown
// expression may itself contain `?` (forgoLowerTry already knows how to
// lower `?` found in a ReturnStmt's Results) and before type checking.
//
// `throw EXPR` requires the innermost enclosing function to declare its
// last result as `error` (named or not). It lowers to a `return` that
// passes EXPR through as the error result and a syntactically-determined
// zero value for every other result — e.g. `throw errors.New("empty")` in
// a function returning (*Thing, error) becomes `return nil,
// errors.New("empty")`. If a leading result's zero value can't be
// determined from its syntax alone (e.g. a named struct/defined type,
// which might not be nil-able), lowering reports "no default value for
// <return arg>" instead of guessing.
//
// `throw "some text"` (a bare string literal operand) is sugar for `throw
// errors.New("some text")`; lowering rewrites it to a call to errors.New.
// This requires the file to already import "errors" (under any name, or
// via "."): cmd/go computes a package's import graph from the literal
// source text before the compiler ever runs, so an import synthesized
// here, inside the compiler, would not be visible to it.
func forgoLowerThrow(m posMap, noders []*noder) {
	tl := &forgoThrowLowerer{m: m}
	for _, p := range noders {
		if p.file == nil {
			continue
		}
		tl.file = p.file
		for _, d := range p.file.DeclList {
			if fd, ok := d.(*syntax.FuncDecl); ok && fd.Body != nil {
				tl.block(forgoThrowCtxOf(fd.Type), fd.Body)
			}
		}
	}
	if base.Errors() > 0 {
		base.ErrorExit()
	}
}

// forgoThrowCtx describes the innermost enclosing function for `throw`
// lowering.
type forgoThrowCtx struct {
	// results holds every declared result, including the last (error) one.
	results []*syntax.Field
	// ok reports whether the last result's type is spelled "error", i.e.
	// whether `throw` can be used here at all.
	ok bool
}

func forgoThrowCtxOf(t *syntax.FuncType) forgoThrowCtx {
	n := len(t.ResultList)
	if n == 0 {
		return forgoThrowCtx{}
	}
	last := t.ResultList[n-1]
	name, isName := last.Type.(*syntax.Name)
	return forgoThrowCtx{results: t.ResultList, ok: isName && name.Value == "error"}
}

type forgoThrowLowerer struct {
	m    posMap
	file *syntax.File
}

func (tl *forgoThrowLowerer) errorf(pos syntax.Pos, format string, args ...any) {
	base.ErrorfAt(tl.m.makeXPos(pos), 0, format, args...)
}

func (tl *forgoThrowLowerer) block(fn forgoThrowCtx, b *syntax.BlockStmt) {
	for i, s := range b.List {
		b.List[i] = tl.stmt(fn, s)
	}
}

// stmt lowers a single statement in place, recursing into every nested
// statement list and function literal reachable from it, and returns the
// (possibly replaced) statement.
func (tl *forgoThrowLowerer) stmt(fn forgoThrowCtx, s syntax.Stmt) syntax.Stmt {
	switch x := s.(type) {
	case *syntax.ThrowStmt:
		return tl.lowerThrow(fn, x)

	case *syntax.BlockStmt:
		tl.block(fn, x)
		return x

	case *syntax.IfStmt:
		tl.scanExprsForFuncLits(fn, simpleStmtExpr(x.Init), x.Cond)
		tl.block(fn, x.Then)
		switch e := x.Else.(type) {
		case *syntax.BlockStmt:
			tl.block(fn, e)
		case *syntax.IfStmt:
			x.Else = tl.stmt(fn, e)
		}
		return x

	case *syntax.ForStmt:
		tl.scanExprsForFuncLits(fn, simpleStmtExpr(x.Init), x.Cond, simpleStmtExpr(x.Post))
		tl.block(fn, x.Body)
		return x

	case *syntax.SwitchStmt:
		tl.scanExprsForFuncLits(fn, simpleStmtExpr(x.Init), x.Tag)
		for _, c := range x.Body {
			tl.scanExprsForFuncLits(fn, c.Cases)
			for i, cs := range c.Body {
				c.Body[i] = tl.stmt(fn, cs)
			}
		}
		return x

	case *syntax.SelectStmt:
		for _, c := range x.Body {
			if c.Comm != nil {
				tl.scanExprsForFuncLits(fn, simpleStmtExpr(c.Comm))
			}
			for i, cs := range c.Body {
				c.Body[i] = tl.stmt(fn, cs)
			}
		}
		return x

	case *syntax.LabeledStmt:
		x.Stmt = tl.stmt(fn, x.Stmt)
		return x

	case *syntax.ExprStmt:
		tl.scanExprsForFuncLits(fn, x.X)
		return x

	case *syntax.AssignStmt:
		tl.scanExprsForFuncLits(fn, x.Lhs, x.Rhs)
		return x

	case *syntax.SendStmt:
		tl.scanExprsForFuncLits(fn, x.Chan, x.Value)
		return x

	case *syntax.ReturnStmt:
		tl.scanExprsForFuncLits(fn, x.Results)
		return x

	case *syntax.DeclStmt:
		for _, d := range x.DeclList {
			switch dd := d.(type) {
			case *syntax.VarDecl:
				tl.scanExprsForFuncLits(fn, dd.Values)
			case *syntax.ConstDecl:
				tl.scanExprsForFuncLits(fn, dd.Values)
			}
		}
		return x

	default:
		return s
	}
}

// scanExprsForFuncLits walks each of exprs looking for function literals
// and lowers `throw` within each one found, using that literal's own
// result list as the enclosing function context. It doesn't otherwise
// rewrite exprs, since `throw` never appears in expression position.
func (tl *forgoThrowLowerer) scanExprsForFuncLits(fn forgoThrowCtx, exprs ...syntax.Expr) {
	for _, e := range exprs {
		if e == nil {
			continue
		}
		syntax.Inspect(e, func(n syntax.Node) bool {
			if fl, ok := n.(*syntax.FuncLit); ok {
				tl.block(forgoThrowCtxOf(fl.Type), fl.Body)
				return false
			}
			return true
		})
	}
}

// lowerThrow rewrites `throw X` into a `return` that fills every result
// but the last with a syntactic zero value and the last with X (or, if X
// is a bare string literal, errors.New(X)).
func (tl *forgoThrowLowerer) lowerThrow(fn forgoThrowCtx, x *syntax.ThrowStmt) syntax.Stmt {
	pos := x.Pos()
	ret := new(syntax.ReturnStmt)
	ret.SetPos(pos)

	if !fn.ok {
		tl.errorf(pos, "forgo: throw requires the enclosing function's last result to be error")
		return ret
	}

	n := len(fn.results)
	elems := make([]syntax.Expr, 0, n)
	for _, f := range fn.results[:n-1] {
		z, ok := zeroExpr(pos, f.Type)
		if !ok {
			tl.errorf(pos, "forgo: no default value for %s", resultDesc(f))
			z = syntax.NewName(pos, "nil")
		}
		elems = append(elems, z)
	}
	elems = append(elems, tl.throwErrExpr(x.X))

	if len(elems) == 1 {
		ret.Results = elems[0]
	} else {
		lst := new(syntax.ListExpr)
		lst.SetPos(pos)
		lst.ElemList = elems
		ret.Results = lst
	}
	return ret
}

// resultDesc names a result for use in the "no default value for ..."
// diagnostic: its declared name if it has one, otherwise its type.
func resultDesc(f *syntax.Field) string {
	if f.Name != nil {
		return f.Name.Value
	}
	return syntax.String(f.Type)
}

// zeroExpr returns a syntactic zero-value expression for typ, or false if
// one can't be determined without a type checker — e.g. typ is a plain
// name that might refer to a struct or array type, whose zero value isn't
// a simple literal.
func zeroExpr(pos syntax.Pos, typ syntax.Expr) (syntax.Expr, bool) {
	switch t := typ.(type) {
	case *syntax.ParenExpr:
		return zeroExpr(pos, t.X)

	case *syntax.Operation:
		if t.Op == syntax.Mul && t.Y == nil {
			// pointer type: *T
			return syntax.NewName(pos, "nil"), true
		}

	case *syntax.SliceType, *syntax.MapType, *syntax.ChanType, *syntax.FuncType, *syntax.InterfaceType, *syntax.DotsType:
		return syntax.NewName(pos, "nil"), true

	case *syntax.Name:
		switch t.Value {
		case "error", "any":
			return syntax.NewName(pos, "nil"), true
		case "string":
			return basicLit(pos, syntax.StringLit, `""`), true
		case "bool":
			return syntax.NewName(pos, "false"), true
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
			"byte", "rune", "float32", "float64", "complex64", "complex128":
			return basicLit(pos, syntax.IntLit, "0"), true
		}
	}
	return nil, false
}

func basicLit(pos syntax.Pos, kind syntax.LitKind, val string) *syntax.BasicLit {
	b := new(syntax.BasicLit)
	b.SetPos(pos)
	b.Kind = kind
	b.Value = val
	return b
}

// throwErrExpr returns the expression to use as the error result of a
// lowered throw. A bare string literal is sugar for errors.New(that
// string); anything else (a call, an identifier, ...) is assumed to
// already evaluate to an error and is passed through unchanged.
func (tl *forgoThrowLowerer) throwErrExpr(x syntax.Expr) syntax.Expr {
	lit, ok := x.(*syntax.BasicLit)
	if !ok || lit.Kind != syntax.StringLit {
		return x
	}

	pos := x.Pos()
	pkg, ok := tl.errorsIdent(pos)
	if !ok {
		tl.errorf(pos, `forgo: throw "..." requires this file to already import "errors"`)
		return x
	}

	call := new(syntax.CallExpr)
	call.SetPos(pos)
	if pkg == "" {
		call.Fun = syntax.NewName(pos, "New")
	} else {
		sel := new(syntax.SelectorExpr)
		sel.SetPos(pos)
		sel.X = syntax.NewName(pos, pkg)
		sel.Sel = syntax.NewName(pos, "New")
		call.Fun = sel
	}
	call.ArgList = []syntax.Expr{lit}
	return call
}

// errorsIdent reports the identifier that refers to the "errors" package
// in tl.file, and whether "errors" is imported there at all. The returned
// name is "" (with ok true) when "errors" is dot-imported, since then New
// is already unqualified.
func (tl *forgoThrowLowerer) errorsIdent(pos syntax.Pos) (name string, ok bool) {
	const errorsPath = `"errors"`
	for _, d := range tl.file.DeclList {
		imp, isImport := d.(*syntax.ImportDecl)
		if !isImport || imp.Path == nil || imp.Path.Value != errorsPath {
			continue
		}
		switch {
		case imp.LocalPkgName == nil:
			return "errors", true
		case imp.LocalPkgName.Value == "_":
			// Promote the blank import to a real one now that we use it.
			imp.LocalPkgName = nil
			return "errors", true
		case imp.LocalPkgName.Value == ".":
			return "", true
		default:
			return imp.LocalPkgName.Value, true
		}
	}
	return "", false
}
