// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/syntax"
)

// forgoLowerPostfixIf rewrites every use of the forgo postfix-if statement
// modifier (parsed as *syntax.PostfixIfStmt) into an ordinary *syntax.IfStmt
// wrapping a one-statement block, so the rest of the compiler never has to
// know about it:
//
//	throw "empty" if s == ""
//
// becomes exactly what `if s == "" { throw "empty" }` would have parsed
// as. It must run before forgoLowerThrow and forgoLowerTry, since either
// of those may need to see the wrapped statement (e.g. the throw above)
// inside its new enclosing block, and neither has a case for
// PostfixIfStmt itself.
//
// Unlike throw/? lowering, this rewrite needs no type information and no
// enclosing-function context, so it's a plain, unconditional tree rewrite.
func forgoLowerPostfixIf(noders []*noder) {
	for _, p := range noders {
		if p.file == nil {
			continue
		}
		for _, d := range p.file.DeclList {
			if fd, ok := d.(*syntax.FuncDecl); ok && fd.Body != nil {
				postfixIfBlock(fd.Body)
			}
		}
	}
}

func postfixIfBlock(b *syntax.BlockStmt) {
	for i, s := range b.List {
		b.List[i] = postfixIfStmt(s)
	}
}

// postfixIfStmt rewrites a single statement in place, recursing into every
// nested statement list and function literal reachable from it, and
// returns the (possibly replaced) statement.
func postfixIfStmt(s syntax.Stmt) syntax.Stmt {
	switch x := s.(type) {
	case *syntax.PostfixIfStmt:
		inner := postfixIfStmt(x.Stmt)
		postfixIfExprFuncLits(x.Cond)
		then := &syntax.BlockStmt{List: []syntax.Stmt{inner}}
		then.SetPos(inner.Pos())
		ifs := &syntax.IfStmt{Cond: x.Cond, Then: then}
		ifs.SetPos(x.Pos())
		return ifs

	case *syntax.BlockStmt:
		postfixIfBlock(x)
		return x

	case *syntax.IfStmt:
		if x.Init != nil {
			x.Init = postfixIfSimpleStmt(x.Init)
		}
		postfixIfExprFuncLits(x.Cond)
		postfixIfBlock(x.Then)
		switch e := x.Else.(type) {
		case *syntax.BlockStmt:
			postfixIfBlock(e)
		case *syntax.IfStmt:
			x.Else = postfixIfStmt(e)
		}
		return x

	case *syntax.ForStmt:
		if x.Init != nil {
			x.Init = postfixIfSimpleStmt(x.Init)
		}
		postfixIfExprFuncLits(x.Cond)
		if x.Post != nil {
			x.Post = postfixIfSimpleStmt(x.Post)
		}
		postfixIfBlock(x.Body)
		return x

	case *syntax.SwitchStmt:
		if x.Init != nil {
			x.Init = postfixIfSimpleStmt(x.Init)
		}
		postfixIfExprFuncLits(x.Tag)
		for _, c := range x.Body {
			postfixIfExprFuncLits(c.Cases)
			for i, cs := range c.Body {
				c.Body[i] = postfixIfStmt(cs)
			}
		}
		return x

	case *syntax.SelectStmt:
		for _, c := range x.Body {
			if c.Comm != nil {
				c.Comm = postfixIfSimpleStmt(c.Comm)
			}
			for i, cs := range c.Body {
				c.Body[i] = postfixIfStmt(cs)
			}
		}
		return x

	case *syntax.LabeledStmt:
		x.Stmt = postfixIfStmt(x.Stmt)
		return x

	case *syntax.RangeClause:
		postfixIfExprFuncLits(x.X)
		return x

	case *syntax.ExprStmt:
		postfixIfExprFuncLits(x.X)
		return x

	case *syntax.AssignStmt:
		postfixIfExprFuncLits(x.Lhs)
		postfixIfExprFuncLits(x.Rhs)
		return x

	case *syntax.SendStmt:
		postfixIfExprFuncLits(x.Chan)
		postfixIfExprFuncLits(x.Value)
		return x

	case *syntax.ReturnStmt:
		postfixIfExprFuncLits(x.Results)
		return x

	case *syntax.DeclStmt:
		for _, d := range x.DeclList {
			switch dd := d.(type) {
			case *syntax.VarDecl:
				postfixIfExprFuncLits(dd.Values)
			case *syntax.ConstDecl:
				postfixIfExprFuncLits(dd.Values)
			}
		}
		return x

	default:
		return s
	}
}

// postfixIfSimpleStmt is postfixIfStmt for the SimpleStmt slots (if/for
// init and post clauses, select comm clauses). None of those positions can
// themselves hold a *syntax.PostfixIfStmt (the parser only produces one in
// ordinary statement-list position), so the result is always still a
// SimpleStmt.
func postfixIfSimpleStmt(s syntax.SimpleStmt) syntax.SimpleStmt {
	return postfixIfStmt(s).(syntax.SimpleStmt)
}

// postfixIfExprFuncLits walks e looking for function literals and lowers
// postfix-if within each one found. It doesn't otherwise rewrite e, since
// postfix-if never appears in expression position.
func postfixIfExprFuncLits(e syntax.Expr) {
	if e == nil {
		return
	}
	syntax.Inspect(e, func(n syntax.Node) bool {
		if fl, ok := n.(*syntax.FuncLit); ok {
			postfixIfBlock(fl.Body)
			return false
		}
		return true
	})
}
