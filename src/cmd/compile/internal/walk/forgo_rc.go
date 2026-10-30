// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// fgoBuildPtrSlices rewrites every value that nogc code stores into a
// struct field that is a slice of struct pointers (a slice literal, an append, or a view
// of another such field) to build a new slice in a buffer outside the GC
// heap, holding a reference to each element. The store then takes that
// buffer over (see ssagen's fgoStringStore). It runs before order, so the
// literals and appends never become GC allocations.
func fgoBuildPtrSlices(fn *ir.Func) {
	if !fn.FgoNogc() {
		return
	}
	var edit func(n ir.Node) ir.Node
	edit = func(n ir.Node) ir.Node {
		ir.EditChildren(n, edit)
		switch n.Op() {
		case ir.OAS:
			as := n.(*ir.AssignStmt)
			if as.Y != nil && ir.FgoPtrSliceField(as.X) {
				as.Y = fgoBuildPtrSlice(as.Y)
			}
		case ir.OPTRLIT:
			if lit, ok := n.(*ir.AddrExpr).X.(*ir.CompLitExpr); ok && lit.Op() == ir.OSTRUCTLIT {
				for _, e := range lit.List {
					kv := e.(*ir.StructKeyExpr)
					if types.FgoPtrSliceType(kv.Field.Type) {
						kv.Value = fgoBuildPtrSlice(kv.Value)
					}
				}
			}
		}
		return n
	}
	ir.EditChildren(fn, edit)
}

// fgoBuildPtrSlice returns an expression building the slice e describes:
// its pieces (single elements and views) are evaluated in order into
// temporaries, then copied into a new buffer of the total length.
func fgoBuildPtrSlice(e ir.Node) ir.Node {
	if ir.IsNil(e) {
		return e
	}
	pos := e.Pos()
	t := e.Type()
	elem := t.Elem().Elem()
	var body []ir.Node
	type piece struct {
		v    *ir.Name
		many bool
	}
	var pieces []piece
	add := func(x ir.Node, many bool) {
		if many && ir.IsNil(x) {
			return
		}
		tmp := typecheck.TempAt(pos, ir.CurFunc, x.Type())
		body = append(body, typecheck.Stmt(ir.NewAssignStmt(pos, tmp, x)))
		pieces = append(pieces, piece{tmp, many})
	}
	var split func(x ir.Node)
	split = func(x ir.Node) {
		switch x.Op() {
		case ir.OSLICELIT:
			for _, el := range x.(*ir.CompLitExpr).List {
				add(el, false)
			}
		case ir.OAPPEND:
			call := x.(*ir.CallExpr)
			split(call.Args[0])
			if call.IsDDD {
				split(call.Args[1])
			} else {
				for _, a := range call.Args[1:] {
					add(a, false)
				}
			}
		default:
			add(x, true)
		}
	}
	split(e)

	intType := types.Types[types.TINT]
	length := func() ir.Node {
		ones := int64(0)
		for _, p := range pieces {
			if !p.many {
				ones++
			}
		}
		var sum ir.Node = ir.NewInt(pos, ones)
		for _, p := range pieces {
			if p.many {
				sum = ir.NewBinaryExpr(pos, ir.OADD, sum, ir.NewUnaryExpr(pos, ir.OLEN, p.v))
			}
		}
		return typecheck.Expr(sum)
	}
	s := typecheck.TempAt(pos, ir.CurFunc, t)
	call := func(name string, args ...ir.Node) ir.Node {
		// One type per "any" the declaration's type substitutes.
		fn := typecheck.LookupRuntime(name)
		list := []*types.Type{elem, elem, elem, elem}
		types.SubstAny(fn.Type(), &list)
		subst := make([]*types.Type, 4-len(list))
		for i := range subst {
			subst[i] = elem
		}
		return typecheck.Call(pos, typecheck.LookupRuntime(name, subst...), args, false)
	}
	body = append(body, typecheck.Stmt(ir.NewAssignStmt(pos, s, call("fgoPtrsMake", length()))))
	i := typecheck.TempAt(pos, ir.CurFunc, intType)
	body = append(body, typecheck.Stmt(ir.NewAssignStmt(pos, i, ir.NewInt(pos, 0))))
	for _, p := range pieces {
		if p.many {
			body = append(body, call("fgoPtrsAppendN", s, i, p.v))
			body = append(body, typecheck.Stmt(ir.NewAssignStmt(pos, i,
				typecheck.Expr(ir.NewBinaryExpr(pos, ir.OADD, i, ir.NewUnaryExpr(pos, ir.OLEN, p.v))))))
		} else {
			body = append(body, call("fgoPtrsAppend1", s, i, p.v))
			body = append(body, typecheck.Stmt(ir.NewAssignStmt(pos, i,
				typecheck.Expr(ir.NewBinaryExpr(pos, ir.OADD, i, ir.NewInt(pos, 1))))))
		}
	}
	inl := ir.NewInlinedCallExpr(pos, body, []ir.Node{s})
	inl.SetType(t)
	inl.SetTypecheck(1)
	return inl
}
