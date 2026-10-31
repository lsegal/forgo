// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"fmt"
	"reflect"
	"strings"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// forgoLowerWith rewrites every forgo `with` statement (parsed as
// *syntax.WithStmt) into ordinary Go, once the package has been type
// checked: what it lowers to depends on the type of the value. The
// other forgo lowerings (?, throw, postfix if) run before type
// checking and leave with statements in place, lowering inside them.
// forgoLowerWith reports whether it lowered any, so that the package
// must be type checked again; it lowers the ? it generates itself.
//
//	with NAME := VALUE {
//		BODY
//	}
//
// declares NAME, scoped to the statement, and runs BODY. If NAME has a
// Close method, it then calls NAME.Close(), however BODY is left:
// falling off its end, break, continue or goto out of it, return
// (including throw and ?), or a panic. It lowers to
//
//	{
//		NAME := VALUE
//		closed := false
//		defer func() {
//			if !closed {
//				NAME.Close()
//			}
//		}()
//		BODY
//		closed = true
//		NAME.Close()
//	}
//
// with the same `closed = true; NAME.Close()` before each break, continue
// and goto in BODY that leaves it: the deferred call only closes NAME on
// a return or a panic. When the enclosing function's last result is a
// named error, as `?` requires, an error from Close is not dropped: the
// last Close is `NAME.Close()?` (and so are those before a break,
// continue or goto), and the deferred one sets the error result if it
// is still nil.
//
// Otherwise NAME is a pointer to a nogc object, in nogc code (a
// //fgo:nogc function, a method of a //fgo:nogc type, or a function
// literal in one), and the object is freed by the end of BODY: nothing
// may keep a reference to it past BODY. It lowers to
//
//	{
//		NAME := _fgoWithBegin(VALUE)
//		BODY
//		_fgoWithEnd(NAME)
//	}
//
// with the same `_fgoWithEnd(NAME)` before each branch leaving BODY.
// _fgoWithBegin and _fgoWithEnd, declared once in the package, only mark
// where NAME's scope begins and ends: the nogc pass
// (cmd/compile/internal/ssa's forgo_rc.go) reports an error for every
// use in between that would keep a reference to NAME's object, removes
// the calls, and frees the object after its last use.
// Copies of NAME into other variables are checked here: only variables
// declared in BODY may hold one, and no function literal may refer to
// NAME or to them.
func forgoLowerWith(m posMap, info *types2.Info, noders []*noder) bool {
	wl := &forgoWithLowerer{m: m, info: info, ends: map[*syntax.BlockStmt]bool{}}
	nogcTypes := map[string]bool{}
	for _, p := range noders {
		if p.file == nil {
			continue
		}
		for _, d := range p.file.DeclList {
			if td, ok := d.(*syntax.TypeDecl); ok && forgoIsNogc(td.Pragma) {
				nogcTypes[td.Name.Value] = true
			}
		}
	}
	var first *syntax.File
	for _, p := range noders {
		if p.file == nil {
			continue
		}
		for _, d := range p.file.DeclList {
			if fd, ok := d.(*syntax.FuncDecl); ok && fd.Body != nil {
				nogc := forgoIsNogc(fd.Pragma) || fd.Recv != nil && nogcTypes[forgoRecvTypeName(fd.Recv.Type)]
				n := wl.n
				wl.fn(fd.Type, fd.Body, nogc)
				if wl.n > n && first == nil {
					first = p.file
				}
			}
		}
	}
	if base.Errors() > 0 {
		base.ErrorExit()
	}
	if wl.n == 0 {
		return false
	}
	if wl.frees {
		// The scope markers. _fgoWithEnd's parameter leaks, so escape
		// analysis leaves NAME's object on the nogc heap, where it is
		// counted.
		decls := wl.parseFile(first.Pos(), "var _fgoWithSink any\nfunc _fgoWithBegin[T any](p T) T { return p }\nfunc _fgoWithEnd[T any](p T) { _fgoWithSink = p }\n")
		decls[1].(*syntax.FuncDecl).Pragma = &pragmas{Flag: ir.Noinline}
		decls[2].(*syntax.FuncDecl).Pragma = &pragmas{Flag: ir.Noinline}
		first.DeclList = append(first.DeclList, decls...)
	}
	forgoLowerTry(m, noders) // the ? after each Close
	return true
}

type forgoWithLowerer struct {
	m     posMap
	info  *types2.Info
	n     int
	frees bool                       // whether any with statement frees its value
	ends  map[*syntax.BlockStmt]bool // lowered with statements whose body ends in a terminating statement
}

// fn lowers every with statement in body, a function of type t (nogc
// code, if nogc), and in the function literals within it.
func (wl *forgoWithLowerer) fn(t *syntax.FuncType, body *syntax.BlockStmt, nogc bool) {
	errName := forgoFuncCtxOf(t).errName
	syntax.Inspect(body, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncLit:
			wl.fn(n.Type, n.Body, nogc)
			return false
		case *syntax.BlockStmt:
			n.List = wl.list(errName, nogc, n.List)
		case *syntax.CaseClause:
			n.Body = wl.list(errName, nogc, n.Body)
		case *syntax.CommClause:
			n.Body = wl.list(errName, nogc, n.Body)
		}
		return true
	})
}

func (wl *forgoWithLowerer) list(errName string, nogc bool, list []syntax.Stmt) []syntax.Stmt {
	for i, s := range list {
		switch s := s.(type) {
		case *syntax.WithStmt:
			list[i] = wl.lower(errName, nogc, s)
		case *syntax.LabeledStmt:
			if w, ok := s.Stmt.(*syntax.WithStmt); ok {
				s.Stmt = wl.lower(errName, nogc, w)
			}
		}
	}
	return list
}

// lower lowers w, whose innermost function has the named error result
// errName (or none, if it is "") and is nogc code if nogc.
func (wl *forgoWithLowerer) lower(errName string, nogc bool, w *syntax.WithStmt) syntax.Stmt {
	// Nested with statements first, so that a branch leaving several
	// closes the innermost first.
	w.Body.List = wl.list(errName, nogc, w.Body.List)
	syntax.Inspect(w.Body, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncLit:
			return false
		case *syntax.BlockStmt:
			n.List = wl.list(errName, nogc, n.List)
		case *syntax.CaseClause:
			n.Body = wl.list(errName, nogc, n.Body)
		case *syntax.CommClause:
			n.Body = wl.list(errName, nogc, n.Body)
		}
		return true
	})

	wl.n++
	name := w.Name.Value
	v, _ := wl.info.Defs[w.Name].(*types2.Var)
	hasClose := true
	if v != nil {
		obj, _, _ := types2.LookupFieldOrMethod(v.Type(), true, v.Pkg(), "Close")
		hasClose = obj != nil
	}

	var exit string
	var block *syntax.BlockStmt
	if hasClose {
		closed := fmt.Sprintf("_fgoWithClosed%d", wl.n)
		closeStmt := name + ".Close()"
		deferred := closeStmt
		if errName != "" {
			closeStmt += "?"
			deferred = fmt.Sprintf("if _fgoCloseErr := %s.Close(); _fgoCloseErr != nil && %s == nil {\n%s = _fgoCloseErr\n}", name, errName, errName)
		}
		exit = closed + " = true\n" + closeStmt
		block = wl.parse(w.Pos(), fmt.Sprintf(`{
	_fgoWithName := _fgoWithValue
	%[1]s := false
	defer func() {
		if !%[1]s {
			%[2]s
		}
	}()
	_fgoWithBody
}`, closed, deferred))
		block.List[3] = w.Body
	} else {
		if !nogc {
			base.ErrorfAt(wl.m.makeXPos(w.Name.Pos()), 0, "forgo: with: %s (type %s) has no Close method; freeing it at the end of the block needs nogc code (//fgo:nogc)", name, v.Type())
			return w.Body
		}
		if !wl.fresh(w.Value) {
			base.ErrorfAt(wl.m.makeXPos(w.Value.Pos()), 0, "forgo: with: freeing %s needs a new object: an allocation (&T{...}, new(T)) or a function's result, not a value something else may refer to", name)
		}
		wl.frees = true
		wl.checkCopies(w, v)
		exit = "_fgoWithEnd(" + name + ")"
		block = wl.parse(w.Pos(), "{\n_fgoWithName := _fgoWithBegin(_fgoWithValue)\n_fgoWithBody\n}")
		block.List[1] = w.Body
		block.List[0].(*syntax.AssignStmt).Rhs.(*syntax.CallExpr).ArgList[0] = w.Value
		w.Value = block.List[0].(*syntax.AssignStmt).Rhs
	}

	// Fill in the name and the value.
	block.List[0].(*syntax.AssignStmt).Lhs = w.Name
	block.List[0].(*syntax.AssignStmt).Rhs = w.Value
	block.Rbrace = w.Body.Rbrace
	if wl.terminating(w.Body) {
		// The end of the body is never reached; nor would be code after
		// it, which would make the block not a terminating statement.
		wl.ends[block] = true
	} else {
		// At the end of the body, so positions stay in order.
		block.List = append(block.List, wl.parse(w.Body.Rbrace, "{\n"+exit+"\n}").List...)
	}

	inner := map[string]bool{} // labels declared in the body
	syntax.Inspect(w.Body, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncLit:
			return false
		case *syntax.LabeledStmt:
			inner[n.Label.Value] = true
		}
		return true
	})
	leave := func(b *syntax.BranchStmt) syntax.Stmt {
		blk := wl.parse(b.Pos(), "{\n"+exit+"\n_fgoWithBranch\n}")
		blk.List[len(blk.List)-1] = b
		return blk
	}
	w.Body.List = forgoWithExits(w.Body.List, false, false, inner, leave)
	return block
}

// fresh reports whether e makes a new object: an allocation, or a
// call (not a conversion), whose result the nogc pass then checks.
func (wl *forgoWithLowerer) fresh(e syntax.Expr) bool {
	switch e := syntax.Unparen(e).(type) {
	case *syntax.Operation:
		_, ok := syntax.Unparen(e.X).(*syntax.CompositeLit)
		return e.Op == syntax.And && e.Y == nil && ok
	case *syntax.CallExpr:
		return !e.Fun.GetTypeInfo().IsType()
	}
	return false
}

// checkCopies reports the copies of v, the variable a with statement
// frees, that could outlive its body w.Body: copies into variables
// declared outside it, and uses in function literals, which may run
// after it. Its address cannot be taken, nor can it be assigned to.
// Copies stored anywhere else are the nogc pass's to report.
func (wl *forgoWithLowerer) checkCopies(w *syntax.WithStmt, v *types2.Var) {
	body := w.Body
	inBody := func(obj types2.Object) bool {
		p := obj.Pos()
		return p.Base() == body.Pos().Base() && p.Cmp(body.Pos()) >= 0 && p.Cmp(body.Rbrace) < 0
	}
	errorf := func(n syntax.Node, format string, args ...any) {
		base.ErrorfAt(wl.m.makeXPos(n.Pos()), 0, "forgo: with: %s outlives its with statement: "+format, append([]any{v.Name()}, args...)...)
	}
	copies := map[types2.Object]bool{v: true} // v and the variables holding it
	isCopy := func(e syntax.Expr) bool {
		n, ok := syntax.Unparen(e).(*syntax.Name)
		return ok && copies[wl.info.Uses[n]]
	}
	for changed := true; changed; {
		changed = false
		assign := func(lhs, rhs []syntax.Expr) {
			if len(lhs) != len(rhs) {
				return
			}
			for i, r := range rhs {
				if !isCopy(r) {
					continue
				}
				n, ok := syntax.Unparen(lhs[i]).(*syntax.Name)
				if !ok {
					continue // a store: the nogc pass checks it
				}
				obj := wl.info.Defs[n]
				if obj == nil {
					obj = wl.info.Uses[n]
				}
				if obj == nil || copies[obj] {
					continue
				}
				if !inBody(obj) {
					errorf(n, "copied to %s, declared outside it", n.Value)
					copies[obj] = true // reported once
					continue
				}
				copies[obj] = true
				changed = true
			}
		}
		syntax.Inspect(body, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.AssignStmt:
				if n.Rhs != nil {
					assign(syntax.UnpackListExpr(n.Lhs), syntax.UnpackListExpr(n.Rhs))
				}
			case *syntax.VarDecl:
				names := make([]syntax.Expr, len(n.NameList))
				for i, x := range n.NameList {
					names[i] = x
				}
				assign(names, syntax.UnpackListExpr(n.Values))
			}
			return true
		})
	}
	reported := map[syntax.Pos]bool{}
	syntax.Inspect(body, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.AssignStmt:
			for _, l := range syntax.UnpackListExpr(n.Lhs) {
				if x, ok := syntax.Unparen(l).(*syntax.Name); ok && wl.info.Uses[x] == v {
					base.ErrorfAt(wl.m.makeXPos(x.Pos()), 0, "forgo: with: cannot assign to %s, which the with statement frees", v.Name())
				}
			}
		case *syntax.FuncLit:
			syntax.Inspect(n.Body, func(m syntax.Node) bool {
				if x, ok := m.(*syntax.Name); ok && copies[wl.info.Uses[x]] && !reported[x.Pos()] {
					reported[x.Pos()] = true
					errorf(x, "used by a function literal")
				}
				return true
			})
			return false
		case *syntax.Operation:
			if n.Op == syntax.And && n.Y == nil && isCopy(n.X) {
				errorf(n, "its address is taken")
			}
		}
		return true
	})
}

// forgoWithExits replaces each branch statement in list that leaves the
// block it is in (a with statement's body) by leave(branch). inLoop and
// inBreakable report whether list is inside a loop, or a loop, switch or
// select, of that block; inner holds the labels declared in it.
func forgoWithExits(list []syntax.Stmt, inLoop, inBreakable bool, inner map[string]bool, leave func(*syntax.BranchStmt) syntax.Stmt) []syntax.Stmt {
	var stmt func(s syntax.Stmt, inLoop, inBreakable bool) syntax.Stmt
	stmts := func(list []syntax.Stmt, inLoop, inBreakable bool) {
		for i, s := range list {
			list[i] = stmt(s, inLoop, inBreakable)
		}
	}
	stmt = func(s syntax.Stmt, inLoop, inBreakable bool) syntax.Stmt {
		switch s := s.(type) {
		case *syntax.BranchStmt:
			leaves := false
			switch {
			case s.Tok == syntax.Fallthrough:
			case s.Label != nil:
				leaves = !inner[s.Label.Value]
			case s.Tok == syntax.Break:
				leaves = !inBreakable
			case s.Tok == syntax.Continue:
				leaves = !inLoop
			}
			if leaves {
				return leave(s)
			}
		case *syntax.BlockStmt:
			stmts(s.List, inLoop, inBreakable)
		case *syntax.IfStmt:
			stmts(s.Then.List, inLoop, inBreakable)
			if s.Else != nil {
				s.Else = stmt(s.Else, inLoop, inBreakable)
			}
		case *syntax.ForStmt:
			stmts(s.Body.List, true, true)
		case *syntax.SwitchStmt:
			for _, c := range s.Body {
				stmts(c.Body, inLoop, true)
			}
		case *syntax.SelectStmt:
			for _, c := range s.Body {
				stmts(c.Body, inLoop, true)
			}
		case *syntax.LabeledStmt:
			s.Stmt = stmt(s.Stmt, inLoop, inBreakable)
		case *syntax.PostfixIfStmt:
			s.Stmt = stmt(s.Stmt, inLoop, inBreakable)
		}
		return s
	}
	stmts(list, inLoop, inBreakable)
	return list
}

// terminating reports whether s is a terminating statement, by the
// rules of the spec (as go/types applies them to with statements).
func (wl *forgoWithLowerer) terminating(s syntax.Stmt) bool { return wl.term(s, "") }

// term is terminating for s, labeled label (or not, if it is "").
func (wl *forgoWithLowerer) term(s syntax.Stmt, label string) bool {
	switch s := s.(type) {
	case *syntax.ReturnStmt, *syntax.ThrowStmt:
		return true
	case *syntax.BranchStmt:
		return s.Tok == syntax.Goto || s.Tok == syntax.Fallthrough
	case *syntax.ExprStmt:
		call, ok := syntax.Unparen(s.X).(*syntax.CallExpr)
		if !ok {
			return false
		}
		name, ok := syntax.Unparen(call.Fun).(*syntax.Name)
		return ok && name.Value == "panic"
	case *syntax.BlockStmt:
		return wl.ends[s] || wl.terminatingList(s.List)
	case *syntax.IfStmt:
		return s.Else != nil && wl.terminating(s.Then) && wl.terminating(s.Else)
	case *syntax.LabeledStmt:
		return wl.term(s.Stmt, s.Label.Value)
	case *syntax.ForStmt:
		if _, ok := s.Init.(*syntax.RangeClause); ok || s.Cond != nil {
			return false
		}
		return !forgoHasBreak(s.Body, label, true)
	case *syntax.SwitchStmt:
		hasDefault := false
		for _, c := range s.Body {
			if c.Cases == nil {
				hasDefault = true
			}
			if !wl.terminatingList(c.Body) || forgoHasBreakList(c.Body, label, true) {
				return false
			}
		}
		return hasDefault
	case *syntax.SelectStmt:
		for _, c := range s.Body {
			if !wl.terminatingList(c.Body) || forgoHasBreakList(c.Body, label, true) {
				return false
			}
		}
		return true
	}
	return false
}

func (wl *forgoWithLowerer) terminatingList(list []syntax.Stmt) bool {
	for i := len(list) - 1; i >= 0; i-- {
		if _, ok := list[i].(*syntax.EmptyStmt); !ok {
			return wl.terminating(list[i])
		}
	}
	return false
}

// forgoHasBreak reports whether s contains a break statement leaving
// the statement s is (part of) the body of: one with label, or, if
// implicit, an unlabeled one not inside a nested loop, switch or select.
func forgoHasBreak(s syntax.Stmt, label string, implicit bool) bool {
	switch s := s.(type) {
	case *syntax.BranchStmt:
		if s.Tok == syntax.Break {
			if s.Label == nil {
				return implicit
			}
			return s.Label.Value == label
		}
	case *syntax.BlockStmt:
		return forgoHasBreakList(s.List, label, implicit)
	case *syntax.IfStmt:
		return forgoHasBreak(s.Then, label, implicit) || s.Else != nil && forgoHasBreak(s.Else, label, implicit)
	case *syntax.LabeledStmt:
		return forgoHasBreak(s.Stmt, label, implicit)
	case *syntax.PostfixIfStmt:
		return forgoHasBreak(s.Stmt, label, implicit)
	case *syntax.ForStmt:
		return label != "" && forgoHasBreak(s.Body, label, false)
	case *syntax.SwitchStmt:
		if label == "" {
			return false
		}
		for _, c := range s.Body {
			if forgoHasBreakList(c.Body, label, false) {
				return true
			}
		}
	case *syntax.SelectStmt:
		if label == "" {
			return false
		}
		for _, c := range s.Body {
			if forgoHasBreakList(c.Body, label, false) {
				return true
			}
		}
	}
	return false
}

func forgoHasBreakList(list []syntax.Stmt, label string, implicit bool) bool {
	for _, s := range list {
		if forgoHasBreak(s, label, implicit) {
			return true
		}
	}
	return false
}

var forgoPosType = reflect.TypeFor[syntax.Pos]()

// parseFile parses decls, declarations of generated code, giving every
// node in them position pos.
func (wl *forgoWithLowerer) parseFile(pos syntax.Pos, decls string) []syntax.Decl {
	src := "package p\n" + decls
	f, err := syntax.Parse(syntax.NewFileBase("<with>"), strings.NewReader(src), nil, nil, 0)
	if err != nil {
		base.FatalfAt(wl.m.makeXPos(pos), "forgo with: parsing generated code: %v\n%s", err, src)
	}
	for _, d := range f.DeclList {
		forgoSetPos(d, pos)
	}
	return f.DeclList
}

// parse parses block, a block statement of generated code, giving every
// node in it position pos.
func (wl *forgoWithLowerer) parse(pos syntax.Pos, block string) *syntax.BlockStmt {
	src := "package p\nfunc _() " + block + "\n"
	f, err := syntax.Parse(syntax.NewFileBase("<with>"), strings.NewReader(src), nil, nil, 0)
	if err != nil {
		base.FatalfAt(wl.m.makeXPos(pos), "forgo with: parsing generated code: %v\n%s", err, src)
	}
	b := f.DeclList[0].(*syntax.FuncDecl).Body
	forgoSetPos(b, pos)
	return b
}

// forgoSetPos gives every node in n position pos.
func forgoSetPos(n syntax.Node, pos syntax.Pos) {
	syntax.Inspect(n, func(n syntax.Node) bool {
		if n == nil {
			return false
		}
		v := reflect.ValueOf(n).Elem()
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.Type() == forgoPosType && f.CanSet() {
				f.Set(reflect.ValueOf(pos))
			}
		}
		n.SetPos(pos)
		return true
	})
}
