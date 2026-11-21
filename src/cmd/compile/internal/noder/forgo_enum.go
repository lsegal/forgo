// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file lowers forgo's enum types (see types2/forgo_enum.go). An enum
// value is a struct holding a tag and one payload struct per variant, so
// the type itself needs no lowering; only variant constructors and type
// switches over enums do:
//
//	Shape.Circle(2)             =>  Shape{_tag: 0, Circle: {2}}
//	Shape.Empty                 =>  Shape{_tag: 2}
//
//	switch v := s.(type) {      =>  tmp := s
//	case Shape.Circle:              switch tmp._tag {
//		...                         case 0:
//	case Shape.Rect, Shape.Empty:       v := tmp.Circle; ...
//		...                         case 1, 2:
//	}                                   v := tmp; ...
//	                                }

package noder

import (
	"go/constant"

	"cmd/compile/internal/ir"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/compile/internal/types2"
	"cmd/internal/src"
	"internal/pkgbits"
)

// forgoEnumVariant reports whether expr is a selector naming a variant of
// an enum type (Shape.Circle), and if so returns the enum's representation
// struct and the index of the variant's field.
func (w *writer) forgoEnumVariant(expr syntax.Expr) (*types2.Struct, int, bool) {
	sel, ok := syntax.Unparen(expr).(*syntax.SelectorExpr)
	if !ok {
		return nil, 0, false
	}
	tv, ok := w.p.maybeTypeAndValue(sel.X)
	if !ok || !tv.IsType() {
		return nil, 0, false
	}
	s := types2.ForgoEnum(tv.Type)
	if s == nil {
		return nil, 0, false
	}
	i := types2.ForgoEnumVariant(s, sel.Sel.Value)
	return s, i, i > 0
}

// forgoEnumExpr writes expr and reports true if it is a variant
// constructor call (Shape.Circle(2)) or a unit variant (Shape.Empty).
func (w *writer) forgoEnumExpr(expr syntax.Expr) bool {
	var args []syntax.Expr
	fun := expr
	if call, ok := expr.(*syntax.CallExpr); ok {
		fun = call.Fun
		if ix, ok := syntax.Unparen(fun).(*syntax.IndexExpr); ok {
			fun = ix.X // Result.Ok[int, error](1)
		}
		args = call.ArgList
	}
	if _, _, ok := w.forgoEnumVariant(fun); !ok {
		return false
	}
	if fun == expr {
		if tv, ok := w.p.maybeTypeAndValue(expr); !ok || types2.ForgoEnum(tv.Type) == nil {
			return false // a constructor that isn't called; types2 rejects it
		}
	}

	// The enum type is the expression's type, which carries the type
	// arguments of a generic enum.
	enum := w.p.typeOf(expr)
	s := types2.ForgoEnum(enum)
	i := types2.ForgoEnumVariant(s, syntax.Unparen(fun).(*syntax.SelectorExpr).Sel.Value)
	payload := s.Field(i).Type()

	w.Code(exprCompLit)
	w.Sync(pkgbits.SyncCompLit)
	w.pos(expr)
	w.typ(enum)
	assert(w.Version().Has(pkgbits.CompactCompLiterals))

	n := 1
	if len(args) > 0 {
		n = 2
	}
	w.Int(-n) // keyed fields
	w.pos(expr)
	w.Int(0)
	w.Code(exprConst)
	w.pos(expr)
	w.typ(s.Field(0).Type())
	w.Value(constant.MakeInt64(int64(i - 1)))
	if len(args) > 0 {
		ps := payload.Underlying().(*types2.Struct)
		w.pos(expr)
		w.Int(i)
		w.Code(exprCompLit)
		w.Sync(pkgbits.SyncCompLit)
		w.pos(expr)
		w.typ(payload)
		w.Int(len(args)) // positional fields
		for j, arg := range args {
			w.pos(arg)
			w.implicitConvExpr(ps.Field(j).Type(), arg)
		}
	}
	return true
}

// forgoEnumSwitch writes stmt and reports true if it is a type switch over
// a value of enum type. It is the enum counterpart of switchStmt: it
// writes the same scopes, and the case values are tags.
func (w *writer) forgoEnumSwitch(stmt *syntax.SwitchStmt) bool {
	guard, ok := stmt.Tag.(*syntax.TypeSwitchGuard)
	if !ok {
		w.Bool(false)
		return false
	}
	enum := w.p.typeOf(guard.X)
	s := types2.ForgoEnum(enum)
	if w.Bool(s != nil); s == nil {
		return false
	}

	w.openScope(stmt.Pos())
	w.pos(stmt)
	w.stmt(stmt.Init)

	w.pos(guard)
	if tag := guard.Lhs; w.Bool(tag != nil) {
		w.pos(tag)
		w.Sync(pkgbits.SyncLocalIdent)
		w.pkg(w.p.curpkg)
		w.String(tag.Value)
	}
	w.expr(guard.X)

	w.Len(len(stmt.Body))
	for i, clause := range stmt.Body {
		if i > 0 {
			w.closeScope(clause.Pos())
		}
		w.openScope(clause.Pos())

		w.pos(clause)

		cases := syntax.UnpackListExpr(clause.Cases)
		field := -1
		w.Len(len(cases))
		for _, cas := range cases {
			_, i, ok := w.forgoEnumVariant(cas)
			assert(ok)
			w.Int(i - 1)
			field = i
		}

		if obj, ok := w.p.info.Implicits[clause]; ok {
			pos := clause.Pos()
			if len(cases) != 0 {
				pos = typeExprEndPos(cases[len(cases)-1])
			}
			w.pos(pos)

			obj := obj.(*types2.Var)
			if types2.Identical(obj.Type(), enum) {
				field = -1 // the whole value
			}
			w.typ(obj.Type())
			w.Int(field)
			w.addLocal(obj)
		}

		w.stmts(clause.Body)
	}
	if len(stmt.Body) > 0 {
		w.closeScope(stmt.Rbrace)
	}

	w.closeScope(stmt.Rbrace)
	return true
}

// forgoEnumSwitch reads a switch statement written by
// writer.forgoEnumSwitch, after its leading flag.
func (r *reader) forgoEnumSwitch(label *types.Sym) ir.Node {
	r.openScope()
	pos := r.pos()
	init := r.stmt()

	gpos := r.pos()
	var ident *ir.Ident
	if r.Bool() {
		ident = ir.NewIdent(r.pos(), r.localIdent())
	}
	x := r.expr()

	var inits ir.Nodes
	if init != nil {
		inits.Append(init)
	}
	tmp := r.tempCopy(gpos, x, &inits)
	enum := tmp.Type()
	tagField := enum.Field(0)
	field := func(pos src.XPos, f *types.Field) ir.Node {
		return typecheck.Expr(ir.NewSelectorExpr(pos, ir.OXDOT, tmp, f.Sym))
	}
	tag := field(gpos, tagField)

	clauses := make([]*ir.CaseClause, r.Len())
	for i := range clauses {
		if i > 0 {
			r.closeScope()
		}
		r.openScope()

		pos := r.pos()
		var cases []ir.Node
		if n := r.Len(); n > 0 {
			cases = make([]ir.Node, n)
			for j := range cases {
				cases[j] = ir.NewBasicLit(pos, tagField.Type, constant.MakeInt64(int64(r.Int())))
			}
		}
		clause := ir.NewCaseStmt(pos, cases, nil)

		var body []ir.Node
		if ident != nil {
			vpos := r.pos()
			name := r.curfn.NewLocal(vpos, ident.Sym(), r.typ())
			var val ir.Node = tmp
			if f := r.Int(); f >= 0 {
				val = field(vpos, enum.Field(f))
			}
			r.addLocal(name)
			assign := ir.NewAssignStmt(vpos, name, val)
			assign.Def = true
			name.Defn = assign
			body = append(body,
				typecheck.Stmt(ir.NewDecl(vpos, ir.ODCL, name)),
				typecheck.Stmt(assign))
		}

		clause.Body = append(body, r.stmts()...)
		clauses[i] = clause
	}
	if len(clauses) > 0 {
		r.closeScope()
	}
	r.closeScope()

	n := ir.NewSwitchStmt(pos, tag, clauses)
	n.Label = label
	n.SetInit(inits)
	return n
}
