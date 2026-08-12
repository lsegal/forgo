// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package forgo

import "cmd/compile/internal/syntax"

// evalQuote implements the Quote(func(){ ... }) builtin available inside
// //fgo:macro bodies. It captures the function literal's body as an AST
// template (without evaluating it), substitutes any Splice(x) call it finds
// for the NodeVal bound to x in the current macro scope, and returns the
// result as a NodeVal. A single-statement expression template unwraps to
// that expression, so the macro can be used in expression position.
func (in *Interp) evalQuote(sc *scope, call *syntax.CallExpr) Value {
	if len(call.ArgList) != 1 {
		fail("forgo: Quote takes exactly one func literal argument")
	}
	lit, ok := call.ArgList[0].(*syntax.FuncLit)
	if !ok {
		fail("forgo: Quote's argument must be a func literal, e.g. Quote(func(){ ... })")
	}

	body := in.subst(sc, lit.Body).(*syntax.BlockStmt)

	if len(body.List) == 1 {
		if es, ok := body.List[0].(*syntax.ExprStmt); ok {
			return NodeVal{Node: es.X}
		}
		return NodeVal{Node: body.List[0]}
	}
	return NodeVal{Node: body}
}

// subst deep-clones n, replacing every Splice(x) call expression it finds
// with the syntax tree bound to local variable x (which must hold a
// NodeVal, i.e. be a macro parameter or a value derived from one).
func (in *Interp) subst(sc *scope, n syntax.Node) syntax.Node {
	if n == nil {
		return nil
	}
	switch x := n.(type) {
	case *syntax.Name:
		nn := *x
		return &nn

	case *syntax.BasicLit:
		nn := *x
		return &nn

	case *syntax.ParenExpr:
		nn := *x
		nn.X = in.subst(sc, x.X).(syntax.Expr)
		return &nn

	case *syntax.Operation:
		nn := *x
		nn.X = in.subst(sc, x.X).(syntax.Expr)
		if x.Y != nil {
			nn.Y = in.subst(sc, x.Y).(syntax.Expr)
		}
		return &nn

	case *syntax.SelectorExpr:
		nn := *x
		nn.X = in.subst(sc, x.X).(syntax.Expr)
		return &nn

	case *syntax.IndexExpr:
		nn := *x
		nn.X = in.subst(sc, x.X).(syntax.Expr)
		nn.Index = in.subst(sc, x.Index).(syntax.Expr)
		return &nn

	case *syntax.CallExpr:
		if name, ok := x.Fun.(*syntax.Name); ok && name.Value == "Splice" {
			if len(x.ArgList) != 1 {
				fail("forgo: Splice takes exactly one argument")
			}
			argName, ok := x.ArgList[0].(*syntax.Name)
			if !ok {
				fail("forgo: Splice's argument must be a local variable bound to a macro parameter")
			}
			v, ok := sc.get(argName.Value)
			if !ok {
				fail("forgo: undefined: %s", argName.Value)
			}
			nv, ok := v.(NodeVal)
			if !ok {
				fail("forgo: Splice(%s): %s is not a quoted node", argName.Value, argName.Value)
			}
			return nv.Node
		}
		nn := *x
		nn.Fun = in.subst(sc, x.Fun).(syntax.Expr)
		if x.ArgList != nil {
			args := make([]syntax.Expr, len(x.ArgList))
			for i, a := range x.ArgList {
				args[i] = in.subst(sc, a).(syntax.Expr)
			}
			nn.ArgList = args
		}
		return &nn

	case *syntax.ExprStmt:
		nn := *x
		nn.X = in.subst(sc, x.X).(syntax.Expr)
		return &nn

	case *syntax.ReturnStmt:
		nn := *x
		if x.Results != nil {
			nn.Results = in.subst(sc, x.Results).(syntax.Expr)
		}
		return &nn

	case *syntax.AssignStmt:
		nn := *x
		nn.Lhs = in.subst(sc, x.Lhs).(syntax.Expr)
		if x.Rhs != nil {
			nn.Rhs = in.subst(sc, x.Rhs).(syntax.Expr)
		}
		return &nn

	case *syntax.IfStmt:
		nn := *x
		nn.Cond = in.subst(sc, x.Cond).(syntax.Expr)
		nn.Then = in.subst(sc, x.Then).(*syntax.BlockStmt)
		if x.Else != nil {
			nn.Else = in.subst(sc, x.Else).(syntax.Stmt)
		}
		return &nn

	case *syntax.BlockStmt:
		nn := *x
		list := make([]syntax.Stmt, len(x.List))
		for i, s := range x.List {
			list[i] = in.subst(sc, s).(syntax.Stmt)
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
