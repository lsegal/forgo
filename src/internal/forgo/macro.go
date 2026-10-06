// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package forgo

import (
	"go/ast"
	"go/token"
)

// evalQuote implements the Quote(func(){ ... }) builtin available inside
// //fgo:macro bodies. It captures the function literal's body as an AST
// template (without evaluating it), substitutes any Splice(x) call it finds
// for the NodeVal bound to x in the current macro scope, and returns the
// result as a NodeVal. A single-statement expression template unwraps to
// that expression, so the macro can be used in expression position.
func (in *Interp) evalQuote(sc *scope, call *ast.CallExpr) Value {
	if len(call.Args) != 1 {
		fail("forgo: Quote takes exactly one func literal argument")
	}
	lit, ok := call.Args[0].(*ast.FuncLit)
	if !ok {
		fail("forgo: Quote's argument must be a func literal, e.g. Quote(func(){ ... })")
	}

	body := in.subst(sc, lit.Body).(*ast.BlockStmt)

	if len(body.List) == 1 {
		if es, ok := body.List[0].(*ast.ExprStmt); ok {
			return NodeVal{Node: es.X}
		}
		return NodeVal{Node: body.List[0]}
	}
	return NodeVal{Node: body}
}

// subst deep-clones n, replacing every Splice(x) call expression it finds
// with the syntax tree bound to local variable x (which must hold a
// NodeVal, i.e. be a macro parameter or a value derived from one).
func (in *Interp) subst(sc *scope, n ast.Node) ast.Node {
	if n == nil {
		return nil
	}
	switch x := n.(type) {
	case *ast.Ident:
		nn := *x
		nn.Obj = nil
		return &nn

	case *ast.BasicLit:
		nn := *x
		return &nn

	case *ast.ParenExpr:
		nn := *x
		nn.X = in.subst(sc, x.X).(ast.Expr)
		return &nn

	case *ast.UnaryExpr:
		nn := *x
		nn.X = in.subst(sc, x.X).(ast.Expr)
		return &nn

	case *ast.BinaryExpr:
		nn := *x
		nn.X = in.subst(sc, x.X).(ast.Expr)
		nn.Y = in.subst(sc, x.Y).(ast.Expr)
		return &nn

	case *ast.SelectorExpr:
		nn := *x
		nn.X = in.subst(sc, x.X).(ast.Expr)
		nn.Sel = in.subst(sc, x.Sel).(*ast.Ident)
		return &nn

	case *ast.IndexExpr:
		nn := *x
		nn.X = in.subst(sc, x.X).(ast.Expr)
		nn.Index = in.subst(sc, x.Index).(ast.Expr)
		return &nn

	case *ast.CallExpr:
		if name, ok := x.Fun.(*ast.Ident); ok && name.Name == "Splice" {
			if len(x.Args) != 1 {
				fail("forgo: Splice takes exactly one argument")
			}
			argName, ok := x.Args[0].(*ast.Ident)
			if !ok {
				fail("forgo: Splice's argument must be a local variable bound to a macro parameter")
			}
			v, ok := sc.get(argName.Name)
			if !ok {
				fail("forgo: undefined: %s", argName.Name)
			}
			nv, ok := v.(NodeVal)
			if !ok {
				fail("forgo: Splice(%s): %s is not a quoted node", argName.Name, argName.Name)
			}
			return nv.Node
		}
		nn := *x
		nn.Fun = in.subst(sc, x.Fun).(ast.Expr)
		if x.Args != nil {
			args := make([]ast.Expr, len(x.Args))
			for i, a := range x.Args {
				args[i] = in.subst(sc, a).(ast.Expr)
			}
			nn.Args = args
		}
		return &nn

	case *ast.ExprStmt:
		nn := *x
		nn.X = in.subst(sc, x.X).(ast.Expr)
		return &nn

	case *ast.ReturnStmt:
		nn := *x
		nn.Results = in.substList(sc, x.Results)
		return &nn

	case *ast.AssignStmt:
		nn := *x
		nn.Lhs = in.substList(sc, x.Lhs)
		nn.Rhs = in.substList(sc, x.Rhs)
		return &nn

	case *ast.IncDecStmt:
		nn := *x
		nn.X = in.subst(sc, x.X).(ast.Expr)
		return &nn

	case *ast.IfStmt:
		nn := *x
		nn.Cond = in.subst(sc, x.Cond).(ast.Expr)
		nn.Body = in.subst(sc, x.Body).(*ast.BlockStmt)
		if x.Else != nil {
			nn.Else = in.subst(sc, x.Else).(ast.Stmt)
		}
		return &nn

	case *ast.BlockStmt:
		nn := *x
		list := make([]ast.Stmt, len(x.List))
		for i, s := range x.List {
			list[i] = in.subst(sc, s).(ast.Stmt)
		}
		nn.List = list
		return &nn

	default:
		// Node kinds not explicitly handled are returned unchanged; they
		// cannot contain a Splice(...) call created from macro parameters
		// in the templates forgo v1 supports.
		return n
	}
}

func (in *Interp) substList(sc *scope, list []ast.Expr) []ast.Expr {
	if list == nil {
		return nil
	}
	out := make([]ast.Expr, len(list))
	for i, e := range list {
		out[i] = in.subst(sc, e).(ast.Expr)
	}
	return out
}

// ExpandMacros removes every //fgo:macro function declaration from files
// (macros are never type-checked as ordinary functions) and rewrites every
// call to one elsewhere in the package into the AST it expands to, the way
// the compiler does before type checking. files must have been parsed with
// comments, so the directives are visible. It mutates files in place.
func ExpandMacros(fset *token.FileSet, files []*ast.File) (err error) {
	macros := map[string]*ast.FuncDecl{}
	for _, f := range files {
		kept := f.Decls[:0]
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && IsMacro(fd) {
				macros[fd.Name.Name] = fd
				continue
			}
			kept = append(kept, d)
		}
		f.Decls = kept
	}

	if len(macros) == 0 {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(runtimeErr); ok {
				err = re.err
				return
			}
			panic(r)
		}
	}()
	ex := &expander{
		fset:   fset,
		macros: macros,
		interp: &Interp{Funcs: map[string]*ast.FuncDecl{}, Fset: fset},
	}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				ex.block(fd.Body)
			}
		}
	}
	return nil
}

type expander struct {
	fset   *token.FileSet
	macros map[string]*ast.FuncDecl
	interp *Interp
}

// evalMacroCall runs the macro and returns the (not yet further-expanded)
// node it produced.
func (ex *expander) evalMacroCall(fdecl *ast.FuncDecl, call *ast.CallExpr) ast.Node {
	argNodes := make([]ast.Node, len(call.Args))
	for i, a := range call.Args {
		argNodes[i] = a
	}
	node, err := ex.interp.EvalMacro(fdecl, argNodes)
	if err != nil {
		fail("%v: macro %s: %v", ex.fset.Position(call.Pos()), fdecl.Name.Name, err)
	}
	return node
}

func (ex *expander) block(b *ast.BlockStmt) {
	var out []ast.Stmt
	for _, s := range b.List {
		out = append(out, ex.stmt(s)...)
	}
	b.List = out
}

// nodeToStmts flattens a macro expansion result for insertion into a
// statement list.
func nodeToStmts(n ast.Node) []ast.Stmt {
	switch x := n.(type) {
	case *ast.BlockStmt:
		return x.List
	case ast.Stmt:
		return []ast.Stmt{x}
	case ast.Expr:
		return []ast.Stmt{&ast.ExprStmt{X: x}}
	}
	fail("forgo: macro expansion produced unsupported node %T", n)
	return nil
}

func (ex *expander) stmt(s ast.Stmt) []ast.Stmt {
	switch x := s.(type) {
	case *ast.BlockStmt:
		ex.block(x)
		return []ast.Stmt{x}

	case *ast.ExprStmt:
		if call, ok := x.X.(*ast.CallExpr); ok {
			if fdecl, ok := ex.macroFor(call); ok {
				node := ex.evalMacroCall(fdecl, call)
				return ex.expandProduced(node)
			}
		}
		x.X = ex.expr(x.X)
		return []ast.Stmt{x}

	case *ast.ReturnStmt:
		ex.exprs(x.Results)
		return []ast.Stmt{x}

	case *ast.AssignStmt:
		ex.exprs(x.Lhs)
		ex.exprs(x.Rhs)
		return []ast.Stmt{x}

	case *ast.IncDecStmt:
		x.X = ex.expr(x.X)
		return []ast.Stmt{x}

	case *ast.IfStmt:
		x.Init = ex.simpleStmt(x.Init)
		x.Cond = ex.expr(x.Cond)
		ex.block(x.Body)
		switch e := x.Else.(type) {
		case *ast.BlockStmt:
			ex.block(e)
		case *ast.IfStmt:
			ex.stmt(e)
		}
		return []ast.Stmt{x}

	case *ast.ForStmt:
		x.Init = ex.simpleStmt(x.Init)
		x.Cond = ex.exprOrNil(x.Cond)
		x.Post = ex.simpleStmt(x.Post)
		ex.block(x.Body)
		return []ast.Stmt{x}

	case *ast.RangeStmt:
		x.X = ex.expr(x.X)
		ex.block(x.Body)
		return []ast.Stmt{x}

	case *ast.DeclStmt:
		if gd, ok := x.Decl.(*ast.GenDecl); ok {
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					ex.exprs(vs.Values)
				}
			}
		}
		return []ast.Stmt{x}

	default:
		return []ast.Stmt{s}
	}
}

// expandProduced recursively expands a freshly produced macro-expansion
// node (which may itself contain further macro calls) and flattens it into
// a statement list.
func (ex *expander) expandProduced(n ast.Node) []ast.Stmt {
	switch x := n.(type) {
	case *ast.BlockStmt:
		ex.block(x)
	case ast.Stmt:
		return ex.stmt(x)
	case ast.Expr:
		n = ex.expr(x)
	}
	return nodeToStmts(n)
}

func (ex *expander) simpleStmt(s ast.Stmt) ast.Stmt {
	if s == nil {
		return nil
	}
	if out := ex.stmt(s); len(out) == 1 {
		return out[0]
	}
	return s
}

func (ex *expander) exprOrNil(e ast.Expr) ast.Expr {
	if e == nil {
		return nil
	}
	return ex.expr(e)
}

func (ex *expander) exprs(list []ast.Expr) {
	for i, e := range list {
		list[i] = ex.expr(e)
	}
}

func (ex *expander) macroFor(call *ast.CallExpr) (*ast.FuncDecl, bool) {
	name, ok := call.Fun.(*ast.Ident)
	if !ok {
		return nil, false
	}
	fdecl, ok := ex.macros[name.Name]
	return fdecl, ok
}

func (ex *expander) expr(e ast.Expr) ast.Expr {
	if e == nil {
		return nil
	}
	switch x := e.(type) {
	case *ast.ParenExpr:
		x.X = ex.expr(x.X)
		return x

	case *ast.UnaryExpr:
		x.X = ex.expr(x.X)
		return x

	case *ast.BinaryExpr:
		x.X = ex.expr(x.X)
		x.Y = ex.expr(x.Y)
		return x

	case *ast.SelectorExpr:
		x.X = ex.expr(x.X)
		return x

	case *ast.IndexExpr:
		x.X = ex.expr(x.X)
		x.Index = ex.expr(x.Index)
		return x

	case *ast.TryExpr:
		x.X = ex.expr(x.X)
		return x

	case *ast.CallExpr:
		if fdecl, ok := ex.macroFor(x); ok {
			node := ex.evalMacroCall(fdecl, x)
			if expr, ok := node.(ast.Expr); ok {
				return ex.expr(expr)
			}
			// Macro was used in expression position but expanded to a
			// statement; leave the call in place so the (now bogus)
			// syntax produces a clear type-checking error rather than
			// silently vanishing.
			return x
		}
		ex.exprs(x.Args)
		x.Fun = ex.expr(x.Fun)
		return x

	default:
		return e
	}
}
