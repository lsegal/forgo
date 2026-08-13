// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
	"fmt"
)

// forgoLowerTry rewrites every use of the forgo `?` operator (parsed as
// *syntax.TryExpr) into plain Go control flow, so the rest of the compiler
// never has to know about it. It must run after parsing (and after macro
// expansion, in case a macro produced a `?`) and before type checking.
//
// expr? requires the innermost enclosing function to declare its last
// result as a named `error`, e.g. func f() (int, err error) or
// func g() (err error). Lowering assigns the propagated error to that named
// result and does a naked return, which relies on Go's automatic
// zero-initialization of the other named results — so no type information
// is needed to synthesize zero values.
//
// A `?` used as a whole statement (`foo()?`) is assumed to wrap a call that
// returns only an error; a `?` used anywhere a value is expected
// (`x := foo()?`, `foo()?.bar()`, `if foo()? { ... }`, ...) is assumed to
// wrap a call returning (value, error). Use `_ = foo()?` to discard a value
// explicitly in statement position.
func forgoLowerTry(m posMap, noders []*noder) {
	tl := &forgoTryLowerer{m: m}
	for _, p := range noders {
		if p.file == nil {
			continue
		}
		for _, d := range p.file.DeclList {
			if fd, ok := d.(*syntax.FuncDecl); ok && fd.Body != nil {
				tl.block(forgoFuncCtxOf(fd.Type), fd.Body)
			}
		}
	}
	if base.Errors() > 0 {
		base.ErrorExit()
	}
}

// forgoFuncCtx describes the innermost enclosing function for `?` lowering.
type forgoFuncCtx struct {
	// errName is the identifier of the named `error`-typed last result, or
	// "" if `?` cannot be used here (no named error result).
	errName string
}

func forgoFuncCtxOf(t *syntax.FuncType) forgoFuncCtx {
	n := len(t.ResultList)
	if n == 0 {
		return forgoFuncCtx{}
	}
	last := t.ResultList[n-1]
	if last.Name == nil {
		return forgoFuncCtx{}
	}
	if name, ok := last.Type.(*syntax.Name); !ok || name.Value != "error" {
		return forgoFuncCtx{}
	}
	return forgoFuncCtx{errName: last.Name.Value}
}

type forgoTryLowerer struct {
	m posMap
	n int
}

func (tl *forgoTryLowerer) errorf(pos syntax.Pos, format string, args ...any) {
	base.ErrorfAt(tl.m.makeXPos(pos), 0, format, args...)
}

func (tl *forgoTryLowerer) tempName() string {
	tl.n++
	return fmt.Sprintf("_fore_t%d", tl.n)
}

func (tl *forgoTryLowerer) block(fn forgoFuncCtx, b *syntax.BlockStmt) {
	var out []syntax.Stmt
	for _, s := range b.List {
		out = append(out, tl.stmt(fn, s)...)
	}
	b.List = out
}

func (tl *forgoTryLowerer) stmt(fn forgoFuncCtx, s syntax.Stmt) []syntax.Stmt {
	switch x := s.(type) {
	case *syntax.BlockStmt:
		tl.block(fn, x)
		return []syntax.Stmt{x}

	case *syntax.ExprStmt:
		if try, ok := x.X.(*syntax.TryExpr); ok {
			var pre []syntax.Stmt
			inner := tl.expr(fn, try.X, &pre)
			pre = append(pre, tl.hoistErrOnly(fn, inner, try.Pos())...)
			return pre
		}
		var pre []syntax.Stmt
		x.X = tl.expr(fn, x.X, &pre)
		return append(pre, x)

	case *syntax.ReturnStmt:
		var pre []syntax.Stmt
		x.Results = tl.exprOrNil(fn, x.Results, &pre)
		return append(pre, x)

	case *syntax.AssignStmt:
		var pre []syntax.Stmt
		x.Rhs = tl.exprOrNil(fn, x.Rhs, &pre)
		return append(pre, x)

	case *syntax.SendStmt:
		var pre []syntax.Stmt
		x.Chan = tl.expr(fn, x.Chan, &pre)
		x.Value = tl.expr(fn, x.Value, &pre)
		return append(pre, x)

	case *syntax.IfStmt:
		var pre []syntax.Stmt
		if x.Init != nil {
			x.Init = tl.simpleStmt(fn, x.Init, &pre)
		}
		x.Cond = tl.expr(fn, x.Cond, &pre)
		tl.block(fn, x.Then)
		switch e := x.Else.(type) {
		case *syntax.BlockStmt:
			tl.block(fn, e)
		case *syntax.IfStmt:
			tl.stmt(fn, e)
		}
		return append(pre, x)

	case *syntax.ForStmt:
		return tl.forStmt(fn, x)

	case *syntax.LabeledStmt:
		out := tl.stmt(fn, x.Stmt)
		if len(out) != 1 {
			tl.errorf(x.Pos(), "forgo: ? here would require hoisting statements before label %s; move the fallible call out of the labeled statement", x.Label.Value)
			return []syntax.Stmt{x}
		}
		x.Stmt = out[0]
		return []syntax.Stmt{x}

	case *syntax.DeclStmt:
		var pre []syntax.Stmt
		for _, d := range x.DeclList {
			switch dd := d.(type) {
			case *syntax.VarDecl:
				dd.Values = tl.exprOrNil(fn, dd.Values, &pre)
			case *syntax.ConstDecl:
				dd.Values = tl.exprOrNil(fn, dd.Values, &pre)
			}
		}
		return append(pre, x)

	default:
		return []syntax.Stmt{s}
	}
}

// simpleStmtExpr returns a representative expression from s to probe for a
// `?` (via containsTry); it doesn't need to be exhaustive of s's
// sub-expressions since callers only use it as a yes/no signal.
func simpleStmtExpr(s syntax.SimpleStmt) syntax.Expr {
	switch x := s.(type) {
	case *syntax.ExprStmt:
		return x.X
	case *syntax.AssignStmt:
		return x.Rhs
	case *syntax.SendStmt:
		return x.Value
	}
	return nil
}

func containsTry(e syntax.Expr) bool {
	if e == nil {
		return false
	}
	found := false
	syntax.Inspect(e, func(n syntax.Node) bool {
		if _, ok := n.(*syntax.TryExpr); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// simpleStmt lowers a `?` appearing in an if/for init clause. The init
// clause slot only accepts a single simple statement, so if lowering
// produced more than one (typically a hoisted temp-assign followed by an
// error check), everything except a trailing simple statement is hoisted
// whole into *pre, before the enclosing if/for. A bare `foo()?` init (no
// value bound) hoists entirely, leaving the init clause empty.
func (tl *forgoTryLowerer) simpleStmt(fn forgoFuncCtx, s syntax.SimpleStmt, pre *[]syntax.Stmt) syntax.SimpleStmt {
	out := tl.stmt(fn, s)
	if last, ok := out[len(out)-1].(syntax.SimpleStmt); ok {
		*pre = append(*pre, out[:len(out)-1]...)
		return last
	}
	*pre = append(*pre, out...)
	return nil
}

// forStmt lowers a ForStmt, including `?` appearing in its Cond or Post
// clauses. If neither contains `?`, the 3-clause for is left as-is (only
// Init and Body are lowered). Otherwise the loop is rewritten to an
// equivalent form with empty Cond/Post: the condition becomes an
// `if !cond { break }` at the top of the body, and the post statement moves
// to the bottom, behind a label that bare/labeled `continue` targeting this
// loop is redirected to via goto (found using the parser's own branch
// resolution, syntax.BranchStmt.Target, so nested loops are unaffected).
func (tl *forgoTryLowerer) forStmt(fn forgoFuncCtx, x *syntax.ForStmt) []syntax.Stmt {
	var pre []syntax.Stmt
	if _, isRange := x.Init.(*syntax.RangeClause); !isRange && x.Init != nil {
		x.Init = tl.simpleStmt(fn, x.Init, &pre)
	}

	if !containsTry(x.Cond) && !containsTry(simpleStmtExpr(x.Post)) {
		tl.block(fn, x.Body)
		return append(pre, x)
	}

	postLabel := tl.tempName() + "_post"
	origCond, origPost := x.Cond, x.Post
	rewriteContinues(x, postLabel, x.Body)
	tl.block(fn, x.Body)

	var newBody []syntax.Stmt
	if origCond != nil {
		var condPre []syntax.Stmt
		condVal := tl.expr(fn, origCond, &condPre)
		newBody = append(newBody, condPre...)
		notCond := &syntax.Operation{Op: syntax.Not, X: condVal}
		notCond.SetPos(origCond.Pos())
		brk := &syntax.BranchStmt{Tok: syntax.Break}
		brk.SetPos(origCond.Pos())
		then := &syntax.BlockStmt{List: []syntax.Stmt{brk}, Rbrace: origCond.Pos()}
		then.SetPos(origCond.Pos())
		ifBreak := &syntax.IfStmt{Cond: notCond, Then: then}
		ifBreak.SetPos(origCond.Pos())
		newBody = append(newBody, ifBreak)
	}
	newBody = append(newBody, x.Body.List...)

	var postStmts []syntax.Stmt
	if origPost != nil {
		postStmts = tl.stmt(fn, origPost)
		// The post clause moves from the loop header to the bottom of the
		// body, so it now runs after everything above it. Move its statements'
		// positions to the closing brace to match: the compiler records
		// variable scopes in source order and rejects a scope that opens
		// before the one preceding it.
		for _, s := range postStmts {
			forgoRepositionStmt(s, x.Body.Rbrace)
		}
	} else {
		empty := new(syntax.EmptyStmt)
		empty.SetPos(x.Pos())
		postStmts = []syntax.Stmt{empty}
	}
	labeled := &syntax.LabeledStmt{Label: syntax.NewName(x.Pos(), postLabel), Stmt: postStmts[0]}
	labeled.SetPos(postStmts[0].Pos())
	newBody = append(newBody, labeled)
	newBody = append(newBody, postStmts[1:]...)

	x.Cond = nil
	x.Post = nil
	// The rewritten body's first statement is the old Cond, which sat to the
	// left of the old body's opening brace on the same source line. The
	// block's own open-scope position has to be no later than that, or the
	// scope check sees the block "open" after its own first statement.
	body := &syntax.BlockStmt{List: newBody, Rbrace: x.Body.Rbrace}
	body.SetPos(x.Pos())
	x.Body = body
	return append(pre, x)
}

// forgoRepositionStmt moves s, and every statement nested in it, to pos.
// Expressions keep the positions they were written at, so errors and
// debugging still point at the source the user wrote; only the statement
// scaffolding moves, which is what scope bookkeeping looks at.
func forgoRepositionStmt(s syntax.Stmt, pos syntax.Pos) {
	if s == nil {
		return
	}
	s.SetPos(pos)
	switch x := s.(type) {
	case *syntax.BlockStmt:
		x.Rbrace = pos
		for _, st := range x.List {
			forgoRepositionStmt(st, pos)
		}
	case *syntax.IfStmt:
		forgoRepositionStmt(x.Init, pos)
		forgoRepositionStmt(x.Then, pos)
		forgoRepositionStmt(x.Else, pos)
	case *syntax.LabeledStmt:
		forgoRepositionStmt(x.Stmt, pos)
	}
}

// rewriteContinues redirects every `continue` (bare, or labeled targeting
// this loop) found in s into `goto postLabel`, without descending into
// nested function literals (which can't continue an outer loop) or
// re-resolving continues that already target a different, nested loop.
func rewriteContinues(target *syntax.ForStmt, postLabel string, s syntax.Stmt) {
	if s == nil {
		return
	}
	switch x := s.(type) {
	case *syntax.BlockStmt:
		for _, st := range x.List {
			rewriteContinues(target, postLabel, st)
		}
	case *syntax.BranchStmt:
		if x.Tok == syntax.Continue && x.Target == syntax.Stmt(target) {
			x.Tok = syntax.Goto
			x.Label = syntax.NewName(x.Pos(), postLabel)
			x.Target = nil
		}
	case *syntax.IfStmt:
		rewriteContinues(target, postLabel, x.Init)
		rewriteContinues(target, postLabel, x.Then)
		rewriteContinues(target, postLabel, x.Else)
	case *syntax.ForStmt:
		rewriteContinues(target, postLabel, x.Body)
	case *syntax.SwitchStmt:
		for _, c := range x.Body {
			for _, st := range c.Body {
				rewriteContinues(target, postLabel, st)
			}
		}
	case *syntax.SelectStmt:
		for _, c := range x.Body {
			for _, st := range c.Body {
				rewriteContinues(target, postLabel, st)
			}
		}
	case *syntax.LabeledStmt:
		rewriteContinues(target, postLabel, x.Stmt)
	}
}

func (tl *forgoTryLowerer) exprOrNil(fn forgoFuncCtx, e syntax.Expr, pre *[]syntax.Stmt) syntax.Expr {
	if e == nil {
		return nil
	}
	return tl.expr(fn, e, pre)
}

// expr rewrites e in place, hoisting any TryExpr it finds into *pre and
// replacing it with a reference to the resulting temporary value.
func (tl *forgoTryLowerer) expr(fn forgoFuncCtx, e syntax.Expr, pre *[]syntax.Stmt) syntax.Expr {
	if e == nil {
		return nil
	}
	switch x := e.(type) {
	case *syntax.TryExpr:
		inner := tl.expr(fn, x.X, pre)
		return tl.hoistValue(fn, inner, x.Pos(), pre)

	case *syntax.ParenExpr:
		x.X = tl.expr(fn, x.X, pre)
		return x

	case *syntax.ListExpr:
		for i, el := range x.ElemList {
			x.ElemList[i] = tl.expr(fn, el, pre)
		}
		return x

	case *syntax.Operation:
		x.X = tl.expr(fn, x.X, pre)
		x.Y = tl.exprOrNil(fn, x.Y, pre)
		return x

	case *syntax.SelectorExpr:
		x.X = tl.expr(fn, x.X, pre)
		return x

	case *syntax.IndexExpr:
		x.X = tl.expr(fn, x.X, pre)
		x.Index = tl.expr(fn, x.Index, pre)
		return x

	case *syntax.CallExpr:
		x.Fun = tl.expr(fn, x.Fun, pre)
		for i, a := range x.ArgList {
			x.ArgList[i] = tl.expr(fn, a, pre)
		}
		return x

	case *syntax.FuncLit:
		tl.block(forgoFuncCtxOf(x.Type), x.Body)
		return x

	default:
		return e
	}
}

// hoistValue lowers a `?` used where a value is expected: it assumes inner
// evaluates to (value, error), and returns a reference to the value.
func (tl *forgoTryLowerer) hoistValue(fn forgoFuncCtx, inner syntax.Expr, pos syntax.Pos, pre *[]syntax.Stmt) syntax.Expr {
	if fn.errName == "" {
		tl.errorf(pos, "forgo: ? requires the enclosing function's last result to be a named error")
		fn.errName = "_"
	}
	valName := tl.tempName()
	errName := tl.tempName()

	assign := &syntax.AssignStmt{
		Op:  syntax.Def,
		Lhs: &syntax.ListExpr{ElemList: []syntax.Expr{syntax.NewName(pos, valName), syntax.NewName(pos, errName)}},
		Rhs: inner,
	}
	assign.SetPos(pos)
	*pre = append(*pre, assign, forgoErrCheck(fn, errName, pos))
	return syntax.NewName(pos, valName)
}

// hoistErrOnly lowers a `?` used as a whole statement: it assumes inner
// evaluates to a single error result.
func (tl *forgoTryLowerer) hoistErrOnly(fn forgoFuncCtx, inner syntax.Expr, pos syntax.Pos) []syntax.Stmt {
	if fn.errName == "" {
		tl.errorf(pos, "forgo: ? requires the enclosing function's last result to be a named error")
		fn.errName = "_"
	}
	errName := tl.tempName()
	assign := &syntax.AssignStmt{
		Op:  syntax.Def,
		Lhs: syntax.NewName(pos, errName),
		Rhs: inner,
	}
	assign.SetPos(pos)
	return []syntax.Stmt{assign, forgoErrCheck(fn, errName, pos)}
}

// forgoErrCheck builds:
//
//	if tmpErr != nil {
//	    namedErr = tmpErr
//	    return
//	}
func forgoErrCheck(fn forgoFuncCtx, tmpErr string, pos syntax.Pos) *syntax.IfStmt {
	cond := &syntax.Operation{
		Op: syntax.Neq,
		X:  syntax.NewName(pos, tmpErr),
		Y:  syntax.NewName(pos, "nil"),
	}
	cond.SetPos(pos)

	setErr := &syntax.AssignStmt{
		Lhs: syntax.NewName(pos, fn.errName),
		Rhs: syntax.NewName(pos, tmpErr),
	}
	setErr.SetPos(pos)

	ret := new(syntax.ReturnStmt)
	ret.SetPos(pos)

	// The synthesized block needs a closing-brace position like any other:
	// the noder records one for every block it walks, and DWARF scope
	// generation rejects a block whose end is nowhere.
	then := &syntax.BlockStmt{List: []syntax.Stmt{setErr, ret}, Rbrace: pos}
	then.SetPos(pos)

	ifStmt := &syntax.IfStmt{Cond: cond, Then: then}
	ifStmt.SetPos(pos)
	return ifStmt
}
