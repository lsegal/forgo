// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements forgo's enum types: closed sum types whose
// variants carry their own fields.
//
//	type Shape enum {
//		Circle(radius float64)
//		Rect(w, h float64)
//		Empty
//	}
//
// An enum type is represented as a struct: a tag field followed by one
// field per variant, holding that variant's payload as a struct.
//
//	struct {
//		_tag   uint8 `fgo:"enum"`
//		Circle struct{ radius float64 }
//		Rect   struct{ w, h float64 }
//		Empty  struct{}
//	}
//
// Only the payload of the variant named by the tag is ever non-zero, so
// struct equality is enum equality, and the zero value is the first
// variant with zero fields. Positional fields are named _0, _1, and so on.
// The struct tag on the first field marks the struct as an enum, so the
// enum survives export data and importers unchanged; everything that
// does not know about enums (the backend, reflection) sees a plain struct.
//
// The type checker keeps the representation closed: a struct literal of an
// enum type, a selector naming one of its fields, and a conversion from a
// different type are errors. Values are built with variant constructors
// (Shape.Circle(2), Shape.Empty) and taken apart with a type switch, which
// must name every variant or have a default clause.
package types2

import (
	"cmd/compile/internal/syntax"
	"fmt"
	. "internal/types/errors"
	"strings"
)

const (
	forgoEnumTagField = "_tag"       // name of an enum's tag field
	forgoEnumTag      = `fgo:"enum"` // struct tag marking an enum's tag field
)

// forgoEnumOf returns the representation struct of t if t is an enum
// type, and nil otherwise. Type parameters are never enums.
func forgoEnumOf(t Type) *Struct {
	if t == nil || isTypeParam(t) {
		return nil
	}
	s, _ := t.Underlying().(*Struct)
	if s == nil || len(s.fields) == 0 || s.fields[0].name != forgoEnumTagField || s.Tag(0) != forgoEnumTag {
		return nil
	}
	return s
}

// forgoDeclaring reports whether t names a type whose declaration is
// being checked, so its underlying type is not yet known. The selector
// then reports the cycle itself.
func (check *Checker) forgoDeclaring(t Type) bool {
	var obj Object
	switch t := t.(type) {
	case *Alias:
		obj = t.obj
	case *Named:
		obj = t.obj
	default:
		return false
	}
	_, found := check.objPathIdx[obj]
	return found
}

// forgoEnumVariant returns the index of the field of enum struct s that
// holds variant name, or -1.
func forgoEnumVariant(s *Struct, name string) int {
	for i := 1; i < len(s.fields); i++ {
		if s.fields[i].name == name {
			return i
		}
	}
	return -1
}

// forgoEnumPayload returns the payload struct of the variant held by
// field i of enum struct s.
func forgoEnumPayload(s *Struct, i int) *Struct {
	p, _ := s.fields[i].typ.Underlying().(*Struct)
	if p == nil {
		p = new(Struct)
	}
	return p
}

// forgoEnumType type-checks an enum type expression and returns its
// representation struct.
func (check *Checker) forgoEnumType(e *syntax.EnumType) Type {
	if len(e.VariantList) == 0 {
		check.error(e, InvalidSyntaxTree, "enum type must have at least one variant")
		return Typ[Invalid]
	}
	if len(e.VariantList) > 1<<16 {
		check.error(e, InvalidSyntaxTree, "too many variants in enum type")
		return Typ[Invalid]
	}
	tagType := Typ[Uint8]
	if len(e.VariantList) > 1<<8 {
		tagType = Typ[Uint16]
	}

	fields := []*Var{NewField(e.Pos(), check.pkg, forgoEnumTagField, tagType, false)}
	tags := []string{forgoEnumTag}
	var fset objset
	for _, v := range e.VariantList {
		name := v.Name.Value
		pos := v.Name.Pos()
		payload := check.forgoEnumPayloadType(v)
		fld := NewField(pos, check.pkg, name, payload, false)
		switch {
		case name == "_":
			check.error(v.Name, InvalidSyntaxTree, "enum variant name cannot be _")
			continue
		case name == forgoEnumTagField:
			check.errorf(v.Name, DuplicateDecl, "enum variant name %s is reserved", name)
			continue
		case !check.declareInSet(&fset, pos, fld):
			continue
		}
		check.recordDef(v.Name, fld)
		fields = append(fields, fld)
		tags = append(tags, "")
	}

	s := new(Struct)
	s.fields = fields
	s.tags = tags
	s.markComplete()
	return s
}

// forgoEnumPayloadType type-checks the fields of variant v.
func (check *Checker) forgoEnumPayloadType(v *syntax.EnumVariant) *Struct {
	var fields []*Var
	var fset objset
	var typ Type
	var prev syntax.Expr
	for i, f := range v.ParamList {
		if i == 0 || f.Type != prev {
			typ = check.varType(f.Type)
			prev = f.Type
		}
		if f.Name == nil {
			fields = append(fields, NewField(f.Pos(), check.pkg, fmt.Sprintf("_%d", i), typ, false))
			continue
		}
		fld := NewField(f.Name.Pos(), check.pkg, f.Name.Value, typ, false)
		if f.Name.Value == "_" || check.declareInSet(&fset, f.Name.Pos(), fld) {
			check.recordDef(f.Name, fld)
			fields = append(fields, fld)
		}
	}
	s := new(Struct)
	s.fields = fields
	s.markComplete()
	return s
}

// forgoEnumSelector evaluates the operand e.X of a selector expression. If
// e names a variant of an enum type (Shape.Circle, Option.Some,
// Option[int].None), it handles all of e and reports true. Otherwise it
// leaves x as the evaluated e.X, exactly as check.exprOrType(x, e.X, false)
// would, and reports false.
func (check *Checker) forgoEnumSelector(x *operand, e *syntax.SelectorExpr) bool {
	check.exprOrType(x, e.X, true)
	if x.mode() == typexpr && !check.forgoDeclaring(x.typ()) {
		if s := forgoEnumOf(x.typ()); s != nil {
			if i := forgoEnumVariant(s, e.Sel.Value); i > 0 {
				check.forgoVariantExpr(x, e, s, i)
				return true
			}
		}
	}
	check.nonGeneric(nil, x)
	return false
}

// forgoVariantExpr sets x to the value of the variant selector e, which
// names field i of enum struct s of enum type x.typ: a value of the enum
// type for a unit variant, and the variant's constructor function, which
// must be called, otherwise.
func (check *Checker) forgoVariantExpr(x *operand, e *syntax.SelectorExpr, s *Struct, i int) {
	enum := x.typ()
	fld := s.fields[i]
	if !fld.Exported() && fld.pkg != check.pkg {
		check.errorf(e.Sel, UnexportedName, "variant %s of enum %s is not exported", fld.name, enum)
		// ok to continue
	}
	check.recordUse(e.Sel, fld.Origin())
	x.expr = e

	payload := forgoEnumPayload(s, i)
	if len(payload.fields) == 0 {
		if isGeneric(enum) {
			check.errorf(e, WrongTypeArgCount, "cannot use generic enum %s without instantiation (write %s[...].%s)", e.X, e.X, fld.name)
			x.invalidate()
			return
		}
		if check.forgoEnumUnitCase == nil {
			check.forgoEnumUnitCase = make(map[*syntax.SelectorExpr]bool)
		}
		check.forgoEnumUnitCase[e] = true
		x.mode_ = value
		x.typ_ = enum
		return
	}

	if check.forgoEnumCall != e {
		check.errorf(e, UncalledBuiltin, "variant constructor %s must be called", syntax.Expr(e))
		x.invalidate()
		return
	}

	params := make([]*Var, len(payload.fields))
	for j, f := range payload.fields {
		name := f.name
		if strings.HasPrefix(name, "_") {
			name = "" // positional field
		}
		params[j] = NewParam(f.pos, f.pkg, name, f.typ)
	}
	sig := &Signature{
		params:  NewTuple(params...),
		results: NewTuple(NewParam(e.Pos(), check.pkg, "", enum)),
	}
	if named := asNamed(enum); named != nil && isGeneric(named) {
		// Option.Some: a generic function func[T any](T) Option[T], whose
		// type parameters are fresh copies of the enum's.
		tparams := named.TypeParams().list()
		targs := make([]Type, len(tparams))
		for j, tp := range tparams {
			targs[j] = tp
		}
		sig.results = NewTuple(NewParam(e.Pos(), check.pkg, "", check.instance(e.Pos(), named, targs, nil, check.context())))
		tparams2, typ := check.renameTParams(e.Pos(), tparams, sig)
		sig = typ.(*Signature)
		sig.tparams = &TypeParamList{tparams: tparams2}
	}
	x.mode_ = value
	x.typ_ = sig
}

// forgoEnumFieldSelect reports (and returns true for) a selector that
// would reach into the representation of an enum: a field of x.typ found
// at the given index path whose path passes through an enum struct.
func (check *Checker) forgoEnumFieldSelect(x *operand, e *syntax.SelectorExpr, obj Object, index []int) bool {
	if _, ok := obj.(*Var); !ok {
		return false
	}
	typ := x.typ()
	for _, i := range index {
		t, _ := deref(typ)
		if forgoEnumOf(t) != nil {
			check.errorf(e.Sel, MissingFieldOrMethod, "%s.%s undefined (the variants of enum %s are reached through a type switch)", x.expr, e.Sel.Value, t)
			return true
		}
		s, _ := t.Underlying().(*Struct)
		if s == nil || i >= len(s.fields) {
			return false
		}
		typ = s.fields[i].typ
	}
	return false
}

// forgoEnumCompositeLit reports (and returns true for) a composite literal
// of enum type T.
func (check *Checker) forgoEnumCompositeLit(e *syntax.CompositeLit, T Type) bool {
	s := forgoEnumOf(T)
	if s == nil {
		return false
	}
	check.errorf(e, InvalidLit, "invalid composite literal of enum type %s (use a variant constructor such as %s.%s)", T, T, s.fields[1].name)
	return true
}

// forgoEnumConversion reports (and returns true for) a conversion between
// an enum type and a different type.
func (check *Checker) forgoEnumConversion(x *operand, T Type) bool {
	if !x.isValid() || forgoEnumOf(T) == nil && forgoEnumOf(x.typ()) == nil || Identical(x.typ(), T) {
		return false
	}
	check.errorf(x, InvalidConversion, "cannot convert %s to type %s (enum types only convert to themselves)", x, T)
	x.invalidate()
	return true
}

// forgoEnumTypeSwitch type-checks a type switch over a value sx of enum
// type: each case names variants of the enum, the switch variable has the
// variant's payload type in a case naming one variant with fields (and the
// enum type otherwise), and a switch without a default clause must name every
// variant.
func (check *Checker) forgoEnumTypeSwitch(inner stmtContext, s *syntax.SwitchStmt, lhs *syntax.Name, sx *operand) {
	enum := sx.typ()
	es := forgoEnumOf(enum)

	check.multipleSwitchDefaults(s.Body)

	var lhsVars []*Var
	seen := make(map[int]syntax.Expr)
	hasDefault := false
	for _, clause := range s.Body {
		if clause == nil {
			check.error(s, InvalidSyntaxTree, "incorrect type switch case")
			continue
		}
		cases := syntax.UnpackListExpr(clause.Cases)
		if clause.Cases == nil {
			hasDefault = true
		}
		var T Type = enum
		for _, e := range cases {
			i := check.forgoEnumCase(e, enum, es)
			if i < 0 {
				continue
			}
			if other := seen[i]; other != nil {
				err := check.newError(DuplicateCase)
				err.addf(e, "duplicate case %s in enum switch", syntax.Expr(e))
				err.addf(other, "previous case")
				err.report()
				continue
			}
			seen[i] = e
			if len(cases) == 1 && len(forgoEnumPayload(es, i).fields) > 0 {
				T = es.fields[i].typ
			}
		}
		check.openScope(clause, "case")
		if lhs != nil {
			obj := newVar(LocalVar, lhs.Pos(), check.pkg, lhs.Value, T)
			check.declare(check.scope, nil, obj, clause.Colon)
			check.recordImplicit(clause, obj)
			lhsVars = append(lhsVars, obj)
		}
		check.stmtList(inner, clause.Body)
		check.closeScope()
	}

	if !hasDefault {
		check.forgoEnumExhaustive(s, enum, es, func(i int) bool { return seen[i] != nil }, false)
	}

	if lhs != nil {
		var used bool
		for _, v := range lhsVars {
			if check.usedVars[v] {
				used = true
			}
			check.usedVars[v] = true // avoid usage error when checking entire function
		}
		if !used {
			check.softErrorf(lhs, UnusedVar, "%s declared and not used", lhs.Value)
		}
	}
}

// forgoEnumCase type-checks a case e of a type switch over a value of
// enum type enum (with representation es) and returns the index of the
// variant field it names, or -1 after reporting an error. A case is
// written as a selector naming the variant on the enum type: Shape.Circle,
// pkg.Shape.Circle, or, for a generic enum, Option.Some or Option[int].Some.
func (check *Checker) forgoEnumCase(e syntax.Expr, enum Type, es *Struct) int {
	sel, _ := syntax.Unparen(e).(*syntax.SelectorExpr)
	if sel == nil {
		var x operand
		check.exprOrType(&x, e, false)
		if x.isValid() {
			check.errorf(e, InvalidTypeSwitch, "case %s in switch on enum %s must name a variant, as in %s.%s", syntax.Expr(e), enum, enum, es.fields[1].name)
		}
		return -1
	}
	var x operand
	check.exprOrType(&x, sel.X, true)
	if !x.isValid() {
		return -1
	}
	if x.mode() != typexpr || !forgoSameEnum(x.typ(), enum) {
		check.errorf(e, InvalidTypeSwitch, "case %s in switch on enum %s must name a variant of %s", syntax.Expr(e), enum, enum)
		return -1
	}
	i := forgoEnumVariant(es, sel.Sel.Value)
	if i < 0 {
		check.errorf(sel.Sel, MissingFieldOrMethod, "%s is not a variant of enum %s", sel.Sel.Value, enum)
		return -1
	}
	fld := es.fields[i]
	if !fld.Exported() && fld.pkg != check.pkg {
		check.errorf(sel.Sel, UnexportedName, "variant %s of enum %s is not exported", fld.name, enum)
	}
	check.recordUse(sel.Sel, fld.Origin())
	return i
}

// forgoSameEnum reports whether case type t names the enum type enum. A
// generic enum may be named without its type arguments.
func forgoSameEnum(t, enum Type) bool {
	if Identical(t, enum) {
		return true
	}
	tn, en := asNamed(t), asNamed(enum)
	return tn != nil && en != nil && isGeneric(tn) && tn.Origin() == en.Origin()
}

// forgoEnumExhaustive reports an error at switch s, naming every missing
// variant, when the variants of enum for which covered is false are not
// all handled. exprSwitch reports whether s is an expression switch,
// which can only cover unit variants.
func (check *Checker) forgoEnumExhaustive(s *syntax.SwitchStmt, enum Type, es *Struct, covered func(int) bool, exprSwitch bool) {
	var missing []string
	payloads := false
	for i := 1; i < len(es.fields); i++ {
		if !covered(i) {
			missing = append(missing, check.sprintf("%s.%s", enum, es.fields[i].name))
			if len(forgoEnumPayload(es, i).fields) > 0 {
				payloads = true
			}
		}
	}
	if len(missing) == 0 {
		return
	}
	cases := "case"
	if len(missing) > 1 {
		cases = "cases"
	}
	msg := check.sprintf("switch on %s is not exhaustive: missing %s %s", enum, cases, strings.Join(missing, ", "))
	if exprSwitch && payloads {
		msg += " (match variants with fields in a type switch: switch v := x.(type))"
	}
	check.error(s, InvalidExprSwitch, msg)
}

// forgoEnumExprSwitch checks an expression switch over a value of enum
// type for exhaustiveness: without a default clause, every variant must
// appear as a case, which is only possible when all of them are unit
// variants.
func (check *Checker) forgoEnumExprSwitch(s *syntax.SwitchStmt, x *operand) {
	if !x.isValid() || s.Tag == nil {
		return
	}
	es := forgoEnumOf(x.typ())
	if es == nil {
		return
	}
	seen := make(map[int]bool)
	for _, clause := range s.Body {
		if clause == nil {
			continue
		}
		if clause.Cases == nil {
			return // default
		}
		for _, e := range syntax.UnpackListExpr(clause.Cases) {
			if sel, _ := syntax.Unparen(e).(*syntax.SelectorExpr); sel != nil && check.forgoEnumUnitCase[sel] {
				seen[forgoEnumVariant(es, sel.Sel.Value)] = true
			}
		}
	}
	check.forgoEnumExhaustive(s, x.typ(), es, func(i int) bool { return seen[i] }, true)
}
