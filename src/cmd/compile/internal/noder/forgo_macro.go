// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/forgo"
	"cmd/compile/internal/syntax"
)

// forgoExpandMacros removes every //forgo:macro function declaration from the
// parsed package (macros are never type-checked or compiled as ordinary
// functions) and rewrites every call to one elsewhere in the package into
// the AST it expands to. It must run after parsing and before the package
// is handed to the type checker.
func forgoExpandMacros(noders []*noder) {
	macros := map[string]*syntax.FuncDecl{}
	for _, p := range noders {
		if p.file == nil {
			continue
		}
		kept := p.file.DeclList[:0]
		for _, d := range p.file.DeclList {
			if fd, ok := d.(*syntax.FuncDecl); ok && fd.Recv == nil {
				if fp, ok := fd.Pragma.(syntax.ForgoPragma); ok && fp.ForgoMacro() {
					macros[fd.Name.Value] = fd
					continue
				}
			}
			kept = append(kept, d)
		}
		p.file.DeclList = kept
	}

	if len(macros) == 0 {
		return
	}

	ex := &forgoExpander{
		macros: macros,
		interp: &forgo.Interp{Funcs: map[string]*syntax.FuncDecl{}},
	}
	for _, p := range noders {
		if p.file == nil {
			continue
		}
		for _, d := range p.file.DeclList {
			if fd, ok := d.(*syntax.FuncDecl); ok && fd.Body != nil {
				ex.block(fd.Body)
			}
		}
	}
}

type forgoExpander struct {
	macros map[string]*syntax.FuncDecl
	interp *forgo.Interp
}

// evalMacroCall runs the macro and returns the (not yet further-expanded)
// node it produced.
func (ex *forgoExpander) evalMacroCall(fdecl *syntax.FuncDecl, call *syntax.CallExpr) syntax.Node {
	argNodes := make([]syntax.Node, len(call.ArgList))
	for i, a := range call.ArgList {
		argNodes[i] = a
	}
	node, err := ex.interp.EvalMacro(fdecl, argNodes)
	if err != nil {
		base.Fatalf("%v: macro %s: %v", call.Pos(), fdecl.Name.Value, err)
	}
	return node
}

func (ex *forgoExpander) block(b *syntax.BlockStmt) {
	var out []syntax.Stmt
	for _, s := range b.List {
		out = append(out, ex.stmt(s)...)
	}
	b.List = out
}

// nodeToStmts flattens a macro expansion result for insertion into a
// statement list.
func nodeToStmts(n syntax.Node) []syntax.Stmt {
	switch x := n.(type) {
	case *syntax.BlockStmt:
		return x.List
	case syntax.Stmt:
		return []syntax.Stmt{x}
	case syntax.Expr:
		return []syntax.Stmt{&syntax.ExprStmt{X: x}}
	}
	base.Fatalf("forgo: macro expansion produced unsupported node %T", n)
	return nil
}

func (ex *forgoExpander) stmt(s syntax.Stmt) []syntax.Stmt {
	switch x := s.(type) {
	case *syntax.BlockStmt:
		ex.block(x)
		return []syntax.Stmt{x}

	case *syntax.ExprStmt:
		if call, ok := x.X.(*syntax.CallExpr); ok {
			if fdecl, ok := ex.macroFor(call); ok {
				node := ex.evalMacroCall(fdecl, call)
				return ex.expandProduced(node)
			}
		}
		x.X = ex.expr(x.X)
		return []syntax.Stmt{x}

	case *syntax.ReturnStmt:
		x.Results = ex.exprOrNil(x.Results)
		return []syntax.Stmt{x}

	case *syntax.AssignStmt:
		x.Lhs = ex.expr(x.Lhs)
		x.Rhs = ex.exprOrNil(x.Rhs)
		return []syntax.Stmt{x}

	case *syntax.IfStmt:
		x.Init = ex.simpleStmt(x.Init)
		x.Cond = ex.expr(x.Cond)
		ex.block(x.Then)
		switch e := x.Else.(type) {
		case *syntax.BlockStmt:
			ex.block(e)
		case *syntax.IfStmt:
			ex.stmt(e)
		}
		return []syntax.Stmt{x}

	case *syntax.ForStmt:
		x.Init = ex.simpleStmt(x.Init)
		x.Cond = ex.exprOrNil(x.Cond)
		x.Post = ex.simpleStmt(x.Post)
		ex.block(x.Body)
		return []syntax.Stmt{x}

	case *syntax.DeclStmt:
		for _, d := range x.DeclList {
			switch dd := d.(type) {
			case *syntax.VarDecl:
				dd.Values = ex.exprOrNil(dd.Values)
			case *syntax.ConstDecl:
				dd.Values = ex.exprOrNil(dd.Values)
			}
		}
		return []syntax.Stmt{x}

	default:
		return []syntax.Stmt{s}
	}
}

// expandProduced recursively expands a freshly produced macro-expansion
// node (which may itself contain further macro calls) and flattens it into
// a statement list.
func (ex *forgoExpander) expandProduced(n syntax.Node) []syntax.Stmt {
	switch x := n.(type) {
	case *syntax.BlockStmt:
		ex.block(x)
	case syntax.Stmt:
		return ex.stmt(x)
	case syntax.Expr:
		n = ex.expr(x)
	}
	return nodeToStmts(n)
}

func (ex *forgoExpander) simpleStmt(s syntax.SimpleStmt) syntax.SimpleStmt {
	if s == nil {
		return nil
	}
	out := ex.stmt(s)
	if len(out) == 1 {
		if simp, ok := out[0].(syntax.SimpleStmt); ok {
			return simp
		}
	}
	return s
}

func (ex *forgoExpander) exprOrNil(e syntax.Expr) syntax.Expr {
	if e == nil {
		return nil
	}
	return ex.expr(e)
}

func (ex *forgoExpander) macroFor(call *syntax.CallExpr) (*syntax.FuncDecl, bool) {
	name, ok := call.Fun.(*syntax.Name)
	if !ok {
		return nil, false
	}
	fdecl, ok := ex.macros[name.Value]
	return fdecl, ok
}

func (ex *forgoExpander) expr(e syntax.Expr) syntax.Expr {
	if e == nil {
		return nil
	}
	switch x := e.(type) {
	case *syntax.ParenExpr:
		x.X = ex.expr(x.X)
		return x

	case *syntax.Operation:
		x.X = ex.expr(x.X)
		x.Y = ex.exprOrNil(x.Y)
		return x

	case *syntax.SelectorExpr:
		x.X = ex.expr(x.X)
		return x

	case *syntax.IndexExpr:
		x.X = ex.expr(x.X)
		x.Index = ex.expr(x.Index)
		return x

	case *syntax.CallExpr:
		if fdecl, ok := ex.macroFor(x); ok {
			node := ex.evalMacroCall(fdecl, x)
			if expr, ok := node.(syntax.Expr); ok {
				return ex.expr(expr)
			}
			// Macro was used in expression position but expanded to a
			// statement; leave the call in place so the (now bogus)
			// syntax produces a clear type-checking error rather than
			// silently vanishing.
			return x
		}
		for i, a := range x.ArgList {
			x.ArgList[i] = ex.expr(a)
		}
		x.Fun = ex.expr(x.Fun)
		return x

	default:
		return e
	}
}
