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
// Positional fields are named _0, _1, and so on, and tagged `fgo:"pos"`,
// which makes them accessible from any package (v._0).
//
// Only the payload of the variant named by the tag is ever non-zero, so
// struct equality is enum equality, and the zero value is the first
// variant with zero fields. The struct tag on the first field marks the
// struct as an enum, so the enum survives export data and importers
// unchanged; everything that does not know about enums (the backend,
// reflection) sees a plain struct.
//
// The type checker keeps the representation closed: a struct literal of an
// enum type, a selector naming one of its fields, and a conversion from a
// different type are errors. Values are built with variant constructors
// (Shape.Circle(2), Shape.Empty) and taken apart with a type switch, which
// must name every variant or have a default clause.
package types

import (
	"fmt"
	"go/ast"
	"go/token"
	. "internal/types/errors"
	"strings"
)

const (
	forgoEnumTagField = "_tag"       // name of an enum's tag field
	forgoEnumTag      = `fgo:"enum"` // struct tag marking an enum's tag field
	forgoPosTag       = `fgo:"pos"`  // struct tag marking a variant's positional field
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

// forgoResolved reports whether the underlying type of t is known: t is
// a named type (possibly through aliases) whose declaration is not still
// being checked. Otherwise the selector reports any problem itself.
func (check *Checker) forgoResolved(t Type) bool {
	for {
		a, ok := t.(*Alias)
		if !ok {
			break
		}
		if _, found := check.objPathIdx[a.obj]; found || a.fromRHS == nil {
			return false
		}
		t = a.fromRHS
	}
	n, _ := t.(*Named)
	if n == nil {
		return false
	}
	orig := n.Origin()
	if _, found := check.objPathIdx[orig.obj]; found {
		return false
	}
	if orig.unpack(); orig.rhs() == nil {
		return false
	}
	return true
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
func (check *Checker) forgoEnumType(e *ast.EnumType) Type {
	if len(e.Variants) == 0 {
		check.error(e, InvalidSyntaxTree, "enum type must have at least one variant")
		return Typ[Invalid]
	}
	if len(e.Variants) > 1<<16 {
		check.error(e, InvalidSyntaxTree, "too many variants in enum type")
		return Typ[Invalid]
	}
	tagType := Typ[Uint8]
	if len(e.Variants) > 1<<8 {
		tagType = Typ[Uint16]
	}

	fields := []*Var{NewField(e.Pos(), check.pkg, forgoEnumTagField, tagType, false)}
	tags := []string{forgoEnumTag}
	var fset objset
	for _, v := range e.Variants {
		name := v.Name.Name
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
func (check *Checker) forgoEnumPayloadType(v *ast.EnumVariant) *Struct {
	var fields []*Var
	var tags []string
	var fset objset
	if v.Params == nil {
		s := new(Struct)
		s.markComplete()
		return s
	}
	i := 0
	for _, f := range v.Params.List {
		typ := check.varType(f.Type)
		if len(f.Names) == 0 {
			fields = append(fields, NewField(f.Type.Pos(), check.pkg, fmt.Sprintf("_%d", i), typ, false))
			tags = append(tags, forgoPosTag)
			i++
			continue
		}
		for _, name := range f.Names {
			i++
			fld := NewField(name.Pos(), check.pkg, name.Name, typ, false)
			if name.Name == "_" || check.declareInSet(&fset, name.Pos(), fld) {
				check.recordDef(name, fld)
				fields = append(fields, fld)
			}
		}
	}
	s := new(Struct)
	s.fields = fields
	if len(tags) > 0 {
		s.tags = tags // all fields are positional, or none are
	}
	s.markComplete()
	return s
}

// forgoLookupPkg returns the package in which to look up field sel of a
// value of type T: the package declaring the enum when sel names a
// positional field of a variant (v._0), which is accessible wherever the
// variant is, and the current package otherwise.
func (check *Checker) forgoLookupPkg(T Type, sel string) *Package {
	if strings.HasPrefix(sel, "_") {
		t, _ := deref(T)
		if s, _ := t.Underlying().(*Struct); s != nil {
			for i, f := range s.fields {
				if f.name == sel && s.Tag(i) == forgoPosTag {
					return f.pkg
				}
			}
		}
	}
	return check.pkg
}

// forgoEnumSelector evaluates the operand e.X of a selector expression. If
// e names a variant of an enum type (Shape.Circle, Option.Some,
// Option[int].None), it handles all of e and reports true. Otherwise it
// leaves x as the evaluated e.X, exactly as check.exprOrType(x, e.X, false)
// would, and reports false.
func (check *Checker) forgoEnumSelector(x *operand, e *ast.SelectorExpr) bool {
	check.exprOrType(x, e.X, true)
	if x.mode() == typexpr && check.forgoResolved(x.typ()) {
		if s := forgoEnumOf(x.typ()); s != nil {
			if i := forgoEnumVariant(s, e.Sel.Name); i > 0 {
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
func (check *Checker) forgoVariantExpr(x *operand, e *ast.SelectorExpr, s *Struct, i int) {
	enum := x.typ()
	fld := s.fields[i]
	if !fld.Exported() && fld.pkg != check.pkg {
		check.errorf(e.Sel, UnexportedName, "variant %s of enum %s is not exported", fld.name, enum)
		// ok to continue
	}
	check.recordUse(e.Sel, check.forgoVariantObj(enum, i))
	x.expr = e

	payload := forgoEnumPayload(s, i)
	if len(payload.fields) == 0 {
		if isGeneric(enum) {
			check.errorf(e.Sel, WrongTypeArgCount, "cannot use generic enum %s without instantiation (write %s[...].%s)", e.X, e.X, fld.name)
			x.invalidate()
			return
		}
		if check.forgoEnumUnitCase == nil {
			check.forgoEnumUnitCase = make(map[*ast.SelectorExpr]bool)
		}
		check.forgoEnumUnitCase[e] = true
		x.mode_ = value
		x.typ_ = enum
		return
	}

	if check.forgoEnumCall != e {
		check.errorf(e.Sel, UncalledBuiltin, "variant constructor %s must be called", ast.Expr(e))
		x.invalidate()
		return
	}

	sig := check.forgoCtorSig(e.Pos(), enum, s, i)
	x.mode_ = value
	x.typ_ = sig
}

// forgoCtorSig returns the signature of the constructor of the variant
// held by field i of enum struct s of enum type enum: func(fields) enum,
// or, for a generic enum not yet instantiated (Option.Some), a generic
// function func[T any](T) Option[T] whose type parameters are fresh
// copies of the enum's.
func (check *Checker) forgoCtorSig(pos token.Pos, enum Type, s *Struct, i int) *Signature {
	payload := forgoEnumPayload(s, i)
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
		results: NewTuple(NewParam(pos, check.pkg, "", enum)),
	}
	if named := asNamed(enum); named != nil && isGeneric(named) {
		tparams := named.TypeParams().list()
		targs := make([]Type, len(tparams))
		for j, tp := range tparams {
			targs[j] = tp
		}
		sig.results = NewTuple(NewParam(pos, check.pkg, "", check.instance(pos, named, targs, nil, check.context())))
		tparams2, typ := check.renameTParams(pos, tparams, sig)
		sig = typ.(*Signature)
		sig.tparams = &TypeParamList{tparams: tparams2}
	}
	return sig
}

// forgoVariantObj returns the object recorded as the use of a variant
// name in a selector naming field i of enum type enum (Shape.Circle): a
// function, the variant's constructor, for a variant with fields, and a
// variable of the enum type for a unit variant. Like a generic function,
// the object belongs to the enum's origin type, so that every use of a
// variant records the same object. It is not declared in any scope.
func (check *Checker) forgoVariantObj(enum Type, i int) Object {
	if named := asNamed(enum); named != nil {
		enum = named.Origin()
	}
	s := forgoEnumOf(enum)
	fld := s.fields[i]
	if obj := check.forgoVariantObjs[fld]; obj != nil {
		return obj
	}
	var obj Object
	if len(forgoEnumPayload(s, i).fields) == 0 {
		obj = NewVar(fld.pos, fld.pkg, fld.name, enum)
	} else {
		obj = NewFunc(fld.pos, fld.pkg, fld.name, check.forgoCtorSig(fld.pos, enum, s, i))
	}
	if check.forgoVariantObjs == nil {
		check.forgoVariantObjs = make(map[*Var]Object)
	}
	check.forgoVariantObjs[fld] = obj
	return obj
}

// forgoEnumFieldSelect reports (and returns true for) a selector that
// would reach into the representation of an enum: a field of x.typ found
// at the given index path whose path passes through an enum struct.
func (check *Checker) forgoEnumFieldSelect(x *operand, e *ast.SelectorExpr, obj Object, index []int) bool {
	if _, ok := obj.(*Var); !ok {
		return false
	}
	typ := x.typ()
	for _, i := range index {
		t, _ := deref(typ)
		if forgoEnumOf(t) != nil {
			check.errorf(e.Sel, MissingFieldOrMethod, "%s.%s undefined (the variants of enum %s are reached through a type switch)", x.expr, e.Sel.Name, t)
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
func (check *Checker) forgoEnumCompositeLit(e *ast.CompositeLit, T Type) bool {
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
func (check *Checker) forgoEnumTypeSwitch(inner stmtContext, s *ast.TypeSwitchStmt, lhs *ast.Ident, sx *operand) {
	enum := sx.typ()
	es := forgoEnumOf(enum)

	check.multipleDefaults(s.Body.List)

	var lhsVars []*Var
	seen := make(map[int]ast.Expr)
	hasDefault := false
	for _, c := range s.Body.List {
		clause, _ := c.(*ast.CaseClause)
		if clause == nil {
			check.error(c, InvalidSyntaxTree, "incorrect type switch case")
			continue
		}
		cases := clause.List
		if clause.List == nil {
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
				err.addf(forgoCasePos(e), "duplicate case %s in enum switch", ast.Expr(e))
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
			obj := newVar(LocalVar, lhs.Pos(), check.pkg, lhs.Name, T)
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
			check.softErrorf(lhs, UnusedVar, "%s declared and not used", lhs.Name)
		}
	}
}

// forgoEnumCase type-checks a case e of a type switch over a value of
// enum type enum (with representation es) and returns the index of the
// variant field it names, or -1 after reporting an error. A case is
// written as a selector naming the variant on the enum type: Shape.Circle,
// pkg.Shape.Circle, or, for a generic enum, Option.Some or Option[int].Some.
func (check *Checker) forgoEnumCase(e ast.Expr, enum Type, es *Struct) int {
	sel, _ := ast.Unparen(e).(*ast.SelectorExpr)
	if sel == nil {
		var x operand
		check.exprOrType(&x, e, false)
		if x.isValid() {
			check.errorf(e, InvalidTypeSwitch, "case %s in switch on enum %s must name a variant, as in %s.%s", e, enum, enum, es.fields[1].name)
		}
		return -1
	}
	var x operand
	check.exprOrType(&x, sel.X, true)
	if !x.isValid() {
		return -1
	}
	if x.mode() != typexpr || !forgoSameEnum(x.typ(), enum) {
		check.errorf(e, InvalidTypeSwitch, "case %s in switch on enum %s must name a variant of %s", ast.Expr(e), enum, enum)
		return -1
	}
	i := forgoEnumVariant(es, sel.Sel.Name)
	if i < 0 {
		check.errorf(sel.Sel, MissingFieldOrMethod, "%s is not a variant of enum %s", sel.Sel.Name, enum)
		return -1
	}
	fld := es.fields[i]
	if !fld.Exported() && fld.pkg != check.pkg {
		check.errorf(sel.Sel, UnexportedName, "variant %s of enum %s is not exported", fld.name, enum)
	}
	check.recordUse(sel.Sel, check.forgoVariantObj(enum, i))
	// Record the case as a type, like any type switch case: the type of
	// the switch variable in a case naming just this variant.
	T := enum
	if len(forgoEnumPayload(es, i).fields) > 0 {
		T = fld.typ
	}
	check.recordTypeAndValue(e, typexpr, T, nil)
	return i
}

// forgoCasePos returns the node at which to report an error about case e:
// the variant name of a selector, and e otherwise.
func forgoCasePos(e ast.Expr) positioner {
	if sel, _ := ast.Unparen(e).(*ast.SelectorExpr); sel != nil {
		return sel.Sel
	}
	return e
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
// all handled, and otherwise records s as complete for
// forgoEnumTerminating. exprSwitch reports whether s is an expression
// switch, which can only cover unit variants.
func (check *Checker) forgoEnumExhaustive(s ast.Stmt, enum Type, es *Struct, covered func(int) bool, exprSwitch bool) {
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
		if check.forgoEnumComplete == nil {
			check.forgoEnumComplete = make(map[ast.Stmt]bool)
		}
		check.forgoEnumComplete[s] = true
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
	check.error(atPos(s.Pos()), InvalidExprSwitch, msg)
}

// forgoEnumExprSwitch checks an expression switch over a value of enum
// type for exhaustiveness: without a default clause, every variant must
// appear as a case, which is only possible when all of them are unit
// variants.
func (check *Checker) forgoEnumExprSwitch(s *ast.SwitchStmt, x *operand) {
	if !x.isValid() || s.Tag == nil {
		return
	}
	es := forgoEnumOf(x.typ())
	if es == nil {
		return
	}
	seen := make(map[int]bool)
	for _, c := range s.Body.List {
		clause, _ := c.(*ast.CaseClause)
		if clause == nil {
			continue
		}
		if clause.List == nil {
			return // default
		}
		for _, e := range clause.List {
			if sel, _ := ast.Unparen(e).(*ast.SelectorExpr); sel != nil && check.forgoEnumUnitCase[sel] {
				seen[forgoEnumVariant(es, sel.Sel.Name)] = true
			}
		}
	}
	check.forgoEnumExhaustive(s, x.typ(), es, func(i int) bool { return seen[i] }, true)
}

// forgoEnumTerminating reports whether switch s (with body body), labeled
// label, is a terminating statement because it is an exhaustive switch
// over an enum without a default clause: it names every variant, every
// clause is terminating, and no break refers to s. The compiler gives such
// a switch an implicit default that panics (see
// cmd/compile/internal/noder/forgo_enum.go), so control cannot fall off
// its end.
func (check *Checker) forgoEnumTerminating(s ast.Stmt, body *ast.BlockStmt, label string) bool {
	if !check.forgoEnumComplete[s] {
		return false
	}
	for _, c := range body.List {
		cc := c.(*ast.CaseClause)
		if !check.isTerminatingList(cc.Body, "") || hasBreakList(cc.Body, label, true) {
			return false
		}
	}
	return true
}

// ForgoEnum returns the representation struct of t if t is a forgo enum
// type, and nil otherwise. Field 0 of the struct is the tag; field i > 0
// holds the payload struct of the variant with tag value i-1.
func ForgoEnum(t Type) *Struct { return forgoEnumOf(t) }

// ForgoEnumVariant returns the index of the field of enum struct s that
// holds the variant named name, or -1.
func ForgoEnumVariant(s *Struct, name string) int { return forgoEnumVariant(s, name) }
