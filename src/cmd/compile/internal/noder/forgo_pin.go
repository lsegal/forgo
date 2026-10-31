// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"reflect"
	"strconv"

	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// forgoPinCheck checks the uses of struct fields tagged `fgo:"nogc"`,
// which hold counted references wherever their struct lives (see
// runtime/forgo_rc_tag.go).
//
// The tag goes on fields of pointer-to-struct type only. A value holding
// a tagged field, directly or in a field or array element, is pinned: a
// copy of it would hold the field's reference without counting it, so
// it is never copied. It can be declared (as a zero value), allocated
// (&T{...}, new(T)), addressed, and have its fields selected, its
// elements indexed and its methods called through a pointer; it cannot
// be assigned, passed, returned, converted, stored in an interface, sent,
// ranged over by value, used as a map key or element, a channel element
// or a type argument, or have methods with value receivers, and a slice
// of it cannot be appended to, copied or cleared. Its tagged fields
// cannot have their address taken.
func forgoPinCheck(m posMap, info *types2.Info, files []*syntax.File) {
	pc := &forgoPinChecker{m: m, info: info, memo: map[types2.Type]bool{}}
	for _, file := range files {
		pc.node(file, nil)
	}
	base.ExitIfErrors()
}

type forgoPinChecker struct {
	m    posMap
	info *types2.Info
	memo map[types2.Type]bool
}

func (pc *forgoPinChecker) errorf(n syntax.Node, format string, args ...any) {
	base.ErrorfAt(pc.m.makeXPos(n.Pos()), 0, "fgo:\"nogc\": "+format, args...)
}

func forgoTaggedNogc(tag string) bool { return reflect.StructTag(tag).Get("fgo") == "nogc" }

// pinned reports whether a value of type t holds a tagged field.
func (pc *forgoPinChecker) pinned(t types2.Type) bool {
	if t == nil {
		return false
	}
	if v, ok := pc.memo[t]; ok {
		return v
	}
	pc.memo[t] = false // cycles through pointers never pin
	v := false
	switch u := t.Underlying().(type) {
	case *types2.Struct:
		for i := 0; i < u.NumFields(); i++ {
			if forgoTaggedNogc(u.Tag(i)) || pc.pinned(u.Field(i).Type()) {
				v = true
				break
			}
		}
	case *types2.Array:
		v = pc.pinned(u.Elem())
	}
	pc.memo[t] = v
	return v
}

func (pc *forgoPinChecker) typeOf(e syntax.Expr) types2.Type {
	if e == nil {
		return nil
	}
	t, _ := e.GetTypeInfo().Type.(types2.Type)
	return t
}

// isValue reports whether e is an expression denoting a value (not a
// type, a package or a builtin).
func (pc *forgoPinChecker) isValue(e syntax.Expr) bool {
	tv := e.GetTypeInfo()
	return tv.Type != nil && tv.IsValue()
}

// node checks n, whose parent is parent, and its children.
func (pc *forgoPinChecker) node(n, parent syntax.Node) {
	if n == nil {
		return
	}
	switch n := n.(type) {
	case *syntax.StructType:
		for i, f := range n.FieldList {
			if i < len(n.TagList) && n.TagList[i] != nil && forgoTaggedNogc(forgoTagValue(n.TagList[i])) {
				pc.checkTaggedField(f)
			}
		}
	case *syntax.FuncType:
		for _, f := range append(append([]*syntax.Field(nil), n.ParamList...), n.ResultList...) {
			if t := pc.typeOf(f.Type); pc.pinned(t) {
				pc.errorf(f.Type, "%s holds a field tagged fgo:\"nogc\" and cannot be passed or returned by value; use *%s", t, t)
			}
		}
	case *syntax.FuncDecl:
		if n.Recv != nil {
			if t := pc.typeOf(n.Recv.Type); pc.pinned(t) {
				pc.errorf(n.Recv.Type, "%s holds a field tagged fgo:\"nogc\" and cannot be a value receiver; use *%s", t, t)
			}
		}
	case *syntax.MapType:
		for _, e := range []syntax.Expr{n.Key, n.Value} {
			if t := pc.typeOf(e); pc.pinned(t) {
				pc.errorf(e, "%s holds a field tagged fgo:\"nogc\" and cannot be a map key or element; use *%s", t, t)
			}
		}
	case *syntax.ChanType:
		if t := pc.typeOf(n.Elem); pc.pinned(t) {
			pc.errorf(n.Elem, "%s holds a field tagged fgo:\"nogc\" and cannot be a channel element; use *%s", t, t)
		}
	case *syntax.Name:
		// A generic instantiation copies values of its type arguments.
		if inst, ok := pc.info.Instances[n]; ok {
			for i := 0; i < inst.TypeArgs.Len(); i++ {
				if t := inst.TypeArgs.At(i); pc.pinned(t) {
					pc.errorf(n, "%s holds a field tagged fgo:\"nogc\" and cannot be a type argument; use *%s", t, t)
				}
			}
		}
	case *syntax.RangeClause:
		if n.Lhs != nil {
			lhs := syntax.UnpackListExpr(n.Lhs)
			if len(lhs) == 2 {
				if t := pc.rangeElem(n.X); pc.pinned(t) {
					pc.errorf(lhs[1], "ranging over %s by value copies elements holding a field tagged fgo:\"nogc\"; range over the indices", pc.typeOf(n.X))
				}
			}
		}
	case *syntax.CallExpr:
		// append, copy and clear move or zero a slice's elements
		// without counting them.
		if tv := syntax.Unparen(n.Fun).GetTypeInfo(); tv.IsBuiltin() && len(n.ArgList) > 0 {
			switch name := forgoBuiltinName(n.Fun); name {
			case "append", "copy", "clear":
				if s, ok := pc.typeOf(n.ArgList[0]).Underlying().(*types2.Slice); ok && pc.pinned(s.Elem()) {
					pc.errorf(n, "cannot %s a slice of %s, which holds a field tagged fgo:\"nogc\"; use a slice of *%s", name, s.Elem(), s.Elem())
				}
			}
		}
	case *syntax.Operation:
		if n.Op == syntax.And && n.Y == nil {
			if sel, ok := syntax.Unparen(n.X).(*syntax.SelectorExpr); ok && pc.selectsTagged(sel) {
				pc.errorf(n, "cannot take the address of %s, a field tagged fgo:\"nogc\"", sel.Sel.Value)
			}
		}
	}
	if e, ok := n.(syntax.Expr); ok && pc.isValue(e) && pc.pinned(pc.typeOf(e)) && !pc.allowed(e, parent) {
		t := pc.typeOf(e)
		pc.errorf(e, "copies a value of type %s, which holds a field tagged fgo:\"nogc\"; use a *%s", t, t)
	}
	forgoChildren(n, func(c syntax.Node) { pc.node(c, n) })
}

// forgoTagValue returns the text of a struct tag literal.
func forgoTagValue(lit *syntax.BasicLit) string {
	v := lit.Value
	if len(v) >= 2 {
		if v[0] == '`' {
			return v[1 : len(v)-1]
		}
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
	}
	return ""
}

// checkTaggedField reports a tagged field whose type is not a pointer to
// a struct.
func (pc *forgoPinChecker) checkTaggedField(f *syntax.Field) {
	t := pc.typeOf(f.Type)
	if t == nil {
		return
	}
	if p, ok := t.Underlying().(*types2.Pointer); ok {
		if _, ok := p.Elem().Underlying().(*types2.Struct); ok {
			return
		}
	}
	pc.errorf(f.Type, "a field tagged fgo:\"nogc\" must be a pointer to a struct, not %s", t)
}

// selectsTagged reports whether sel selects a field tagged fgo:"nogc".
func (pc *forgoPinChecker) selectsTagged(sel *syntax.SelectorExpr) bool {
	s := pc.info.Selections[sel]
	if s == nil || s.Kind() != types2.FieldVal {
		return false
	}
	t := s.Recv()
	for _, i := range s.Index()[:len(s.Index())-1] {
		t = forgoStructOf(t).Field(i).Type()
	}
	st := forgoStructOf(t)
	last := s.Index()[len(s.Index())-1]
	return st != nil && forgoTaggedNogc(st.Tag(last))
}

func forgoStructOf(t types2.Type) *types2.Struct {
	if p, ok := t.Underlying().(*types2.Pointer); ok {
		t = p.Elem()
	}
	st, _ := t.Underlying().(*types2.Struct)
	return st
}

// rangeElem returns the type of the elements a range over x yields.
func (pc *forgoPinChecker) rangeElem(x syntax.Expr) types2.Type {
	t := pc.typeOf(x)
	if t == nil {
		return nil
	}
	if p, ok := t.Underlying().(*types2.Pointer); ok {
		t = p.Elem()
	}
	switch u := t.Underlying().(type) {
	case *types2.Array:
		return u.Elem()
	case *types2.Slice:
		return u.Elem()
	}
	return nil
}

// allowed reports whether e, a pinned value, is used without being
// copied, as a child of parent.
func (pc *forgoPinChecker) allowed(e syntax.Expr, parent syntax.Node) bool {
	switch p := parent.(type) {
	case *syntax.ParenExpr:
		return true // its own use is checked
	case *syntax.Operation:
		if p.Op == syntax.And && p.Y == nil {
			return true // &e
		}
		switch p.Op {
		case syntax.Eql, syntax.Neq:
			return true // comparing keeps nothing
		}
	case *syntax.SelectorExpr:
		if p.X != e {
			return false
		}
		if s := pc.info.Selections[p]; s != nil && s.Kind() != types2.FieldVal {
			sig := s.Obj().Type().(*types2.Signature)
			_, ptr := sig.Recv().Type().Underlying().(*types2.Pointer)
			return ptr // a value receiver gets a copy
		}
		return true
	case *syntax.IndexExpr:
		return p.X == e
	case *syntax.SliceExpr:
		return p.X == e
	case *syntax.RangeClause:
		return p.X == e // the value variable is checked on its own
	case *syntax.CallExpr:
		// len, cap and unsafe's functions look at their argument only.
		if tv := syntax.Unparen(p.Fun).GetTypeInfo(); tv.IsBuiltin() {
			switch name := forgoBuiltinName(p.Fun); name {
			case "len", "cap", "Sizeof", "Alignof", "Offsetof":
				return true
			}
		}
	case *syntax.CompositeLit, *syntax.KeyValueExpr:
		// An element of a literal is filled in place, if it is a
		// literal itself.
		_, lit := syntax.Unparen(e).(*syntax.CompositeLit)
		if kv, ok := p.(*syntax.KeyValueExpr); ok && kv.Key == e {
			return false
		}
		return lit
	}
	return false
}

func forgoBuiltinName(fun syntax.Expr) string {
	switch f := syntax.Unparen(fun).(type) {
	case *syntax.Name:
		return f.Value
	case *syntax.SelectorExpr:
		return f.Sel.Value
	}
	return ""
}

// forgoChildren calls f for each child node of n.
func forgoChildren(n syntax.Node, f func(syntax.Node)) {
	syntax.Inspect(n, func(c syntax.Node) bool {
		if c == n {
			return true
		}
		if c != nil {
			f(c)
		}
		return false
	})
}
