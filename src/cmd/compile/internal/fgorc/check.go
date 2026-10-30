// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fgorc checks nogc code (see cmd/compile/internal/noder's
// forgo_nogc.go) for the few things the reference counting pass cannot
// handle, and records which fields nogc code reassigns after
// construction.
package fgorc

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

type checker struct {
	seenFn   map[*ir.Func]bool
	reported map[src.XPos]bool
	parents  map[ir.Node]ir.Node
}

// Check reports misuses of nogc objects in target's nogc code and fills
// in types.FgoMutableFields. It must run before inlining and escape
// analysis.
func Check(target *ir.Package) {
	c := &checker{
		seenFn:   map[*ir.Func]bool{},
		reported: map[src.XPos]bool{},
		parents:  map[ir.Node]ir.Node{},
	}
	for _, fn := range target.Funcs {
		if fn.FgoNogc() {
			c.fn(fn)
		}
	}
	base.ExitIfErrors()
}

func (c *checker) errorf(pos src.XPos, format string, args ...any) {
	if c.reported[pos] {
		return
	}
	c.reported[pos] = true
	base.ErrorfAt(pos, 0, "nogc: "+format, args...)
}

func (c *checker) fn(fn *ir.Func) {
	if c.seenFn[fn] {
		return
	}
	c.seenFn[fn] = true
	if fn.Wrapper() {
		return // compiler-generated; its callers are checked instead
	}
	for _, n := range fn.Body {
		c.visit(n, nil)
	}
}

func (c *checker) visit(n, parent ir.Node) {
	if n == nil {
		return
	}
	c.parents[n] = parent
	c.node(n, parent)
	ir.DoChildren(n, func(ch ir.Node) bool {
		c.visit(ch, n)
		return false
	})
}

// rcFieldOf returns the field selected by n, and the struct type it is
// selected from, if n selects a field through a nogc pointer (p.f or
// (*p).f with p a struct pointer).
func rcFieldOf(n ir.Node) (*types.Type, *types.Field) { return ir.FgoRCField(n) }

func (c *checker) assigned(lhs ir.Node) {
	if lhs == nil {
		return
	}
	if lhs.Op() == ir.OINDEX && ir.FgoPtrSliceField(lhs.(*ir.IndexExpr).X) {
		c.errorf(lhs.Pos(), "cannot assign to an element of a slice of struct pointers in a struct field (rebuild the slice instead)")
		return
	}
	if _, f := rcFieldOf(lhs); f != nil {
		types.FgoMutableFields[f] = true
	}
}

func (c *checker) node(n, parent ir.Node) {
	if n.Op() == ir.OTYPE || n.Op() == ir.ONONAME {
		return
	}
	if t := n.Type(); t != nil && !t.IsFuncArgStruct() && types.FgoPtrSliceType(t) {
		c.ptrSlice(n, parent)
	}

	switch n.Op() {
	case ir.OCLOSURE:
		c.fn(n.(*ir.ClosureExpr).Func)

	case ir.OAS:
		c.assigned(n.(*ir.AssignStmt).X)
	case ir.OASOP:
		c.assigned(n.(*ir.AssignOpStmt).X)
	case ir.OAS2, ir.OAS2DOTTYPE, ir.OAS2FUNC, ir.OAS2MAPR, ir.OAS2RECV:
		for _, l := range n.(*ir.AssignListStmt).Lhs {
			c.assigned(l)
		}
	case ir.ORANGE:
		n := n.(*ir.RangeStmt)
		c.assigned(n.Key)
		c.assigned(n.Value)
	}
}

// ptrSlice checks expression n of type []*T, T a nogc type. Such a slice
// is an ordinary GC slice, except when read from a nogc object's field:
// that field owns a reference to each element, so its elements cannot be
// written in place. Literals stored into such a field are built straight
// into its buffer, which cannot take keyed elements.
func (c *checker) ptrSlice(n, parent ir.Node) {
	if n.Op() == ir.OSLICELIT && c.storedToField(n, parent) {
		for _, e := range n.(*ir.CompLitExpr).List {
			if e.Op() == ir.OKEY {
				c.errorf(e.Pos(), "slice literal of struct pointers stored into a struct field cannot have keys")
			}
		}
	}
	if parent == nil || !ir.FgoPtrSliceField(n) {
		return
	}
	switch parent.Op() {
	case ir.OCOPY:
		if parent.(*ir.BinaryExpr).X == n {
			c.errorf(n.Pos(), "cannot write the elements of a slice of struct pointers in a struct field (rebuild the slice instead)")
		}
	case ir.OCLEAR:
		c.errorf(n.Pos(), "cannot write the elements of a slice of struct pointers in a struct field (rebuild the slice instead)")
	}
}

// storedToField reports whether n, a slice literal or append of nogc
// pointers, is (part of) the value stored into a nogc object field.
func (c *checker) storedToField(n, parent ir.Node) bool {
	for parent != nil {
		switch parent.Op() {
		case ir.OAS:
			as := parent.(*ir.AssignStmt)
			return as.Y == n && ir.FgoPtrSliceField(as.X)
		case ir.OSTRUCTKEY:
			// A literal under &: walk stores its fields one by one.
			lit := c.parents[parent]
			return lit != nil && c.parents[lit] != nil && c.parents[lit].Op() == ir.OPTRLIT
		case ir.OAPPEND:
			// part of an outer append: that one must be stored
			n, parent = parent, c.parents[parent]
			continue
		}
		return false
	}
	return false
}
