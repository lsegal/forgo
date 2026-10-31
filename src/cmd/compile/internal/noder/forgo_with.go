// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"fmt"
	"reflect"
	"strings"

	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
)

// forgoLowerWith rewrites every forgo `with` statement (parsed as
// *syntax.WithStmt) into ordinary Go. It runs first among the forgo
// lowerings, so that `?`, throw and postfix if in and around it are
// lowered by their own passes afterwards.
//
//	with NAME := VALUE {
//		BODY
//	}
//
// declares NAME, scoped to the statement, runs BODY, and then calls
// NAME.Close(), however BODY is left: falling off its end, break,
// continue or goto out of it, return (including throw and ?), or a panic.
// It lowers to
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
func forgoLowerWith(m posMap, noders []*noder) {
	wl := &forgoWithLowerer{m: m, ends: map[*syntax.BlockStmt]bool{}}
	for _, p := range noders {
		if p.file == nil {
			continue
		}
		for _, d := range p.file.DeclList {
			if fd, ok := d.(*syntax.FuncDecl); ok && fd.Body != nil {
				wl.fn(fd.Type, fd.Body)
			}
		}
	}
	if base.Errors() > 0 {
		base.ErrorExit()
	}
}

type forgoWithLowerer struct {
	m    posMap
	n    int
	ends map[*syntax.BlockStmt]bool // lowered with statements whose body ends in a terminating statement
}

// fn lowers every with statement in body, a function of type t, and in
// the function literals within it.
func (wl *forgoWithLowerer) fn(t *syntax.FuncType, body *syntax.BlockStmt) {
	errName := forgoFuncCtxOf(t).errName
	syntax.Inspect(body, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncLit:
			wl.fn(n.Type, n.Body)
			return false
		case *syntax.BlockStmt:
			n.List = wl.list(errName, n.List)
		case *syntax.CaseClause:
			n.Body = wl.list(errName, n.Body)
		case *syntax.CommClause:
			n.Body = wl.list(errName, n.Body)
		}
		return true
	})
}

func (wl *forgoWithLowerer) list(errName string, list []syntax.Stmt) []syntax.Stmt {
	for i, s := range list {
		switch s := s.(type) {
		case *syntax.WithStmt:
			list[i] = wl.lower(errName, s)
		case *syntax.LabeledStmt:
			if w, ok := s.Stmt.(*syntax.WithStmt); ok {
				s.Stmt = wl.lower(errName, w)
			}
		}
	}
	return list
}

// lower lowers w, whose innermost function has the named error result
// errName (or none, if it is "").
func (wl *forgoWithLowerer) lower(errName string, w *syntax.WithStmt) syntax.Stmt {
	// Nested with statements first, so that a branch leaving several
	// closes the innermost first.
	w.Body.List = wl.list(errName, w.Body.List)
	syntax.Inspect(w.Body, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncLit:
			return false
		case *syntax.BlockStmt:
			n.List = wl.list(errName, n.List)
		case *syntax.CaseClause:
			n.Body = wl.list(errName, n.Body)
		case *syntax.CommClause:
			n.Body = wl.list(errName, n.Body)
		}
		return true
	})

	wl.n++
	closed := fmt.Sprintf("_fgoWithClosed%d", wl.n)
	name := w.Name.Value
	closeStmt := name + ".Close()"
	deferred := closeStmt
	if errName != "" {
		closeStmt += "?"
		deferred = fmt.Sprintf("if _fgoCloseErr := %s.Close(); _fgoCloseErr != nil && %s == nil {\n%s = _fgoCloseErr\n}", name, errName, errName)
	}
	exit := closed + " = true\n" + closeStmt

	block := wl.parse(w.Pos(), fmt.Sprintf(`{
	_fgoWithName := _fgoWithValue
	%[1]s := false
	defer func() {
		if !%[1]s {
			%[2]s
		}
	}()
	_fgoWithBody
}`, closed, deferred))

	// Fill in the name, the value and the body.
	block.List[0].(*syntax.AssignStmt).Lhs = w.Name
	block.List[0].(*syntax.AssignStmt).Rhs = w.Value
	block.List[3] = w.Body
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

// parse parses block, a block statement of generated code, giving every
// node in it position pos.
func (wl *forgoWithLowerer) parse(pos syntax.Pos, block string) *syntax.BlockStmt {
	src := "package p\nfunc _() " + block + "\n"
	f, err := syntax.Parse(syntax.NewFileBase("<with>"), strings.NewReader(src), nil, nil, 0)
	if err != nil {
		base.FatalfAt(wl.m.makeXPos(pos), "forgo with: parsing generated code: %v\n%s", err, src)
	}
	b := f.DeclList[0].(*syntax.FuncDecl).Body
	syntax.Inspect(b, func(n syntax.Node) bool {
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
	return b
}
