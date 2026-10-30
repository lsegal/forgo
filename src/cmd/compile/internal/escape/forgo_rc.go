// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package escape

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

// fgoHeapAllocReason keeps the structs that nogc code allocates with nogc
// pointer, string or slice fields off the stack: they come from the
// reference counted allocator, which frees them when the last reference
// dies, and count the references and own the buffers they hold. (A stack
// object would have to hand every pointer stored into it to the GC, and
// keep copies of its strings on the GC heap.)
func fgoHeapAllocReason(fn *ir.Func, n ir.Node) string {
	if n != nil && fn.FgoNogc() && (n.Op() == ir.ONEW || n.Op() == ir.OPTRLIT) {
		if t := n.Type().Elem(); t.FgoIsRC() && types.FgoHasRCFields(t) {
			return "nogc object"
		}
	}
	return ""
}

// fgoParamTag decides whether nogc pointer parameter f of fn is owned:
// the caller hands over a reference, and fn releases it (or passes it
// on). It returns types.FgoOwnedTag for owned parameters and "" otherwise
// (borrowed: the caller keeps the reference for the duration of the call).
//
// The tag replaces the escape analysis tag, which carries nothing useful
// for a nogc pointer (its object is never on the stack or GC heap), and
// travels to importing packages the same way.
//
// A parameter is owned if fn consumes it anyway (returns or stores it, or
// passes it to an owned parameter of a function compiled before fn), or if
// fn allocates a nogc object of the same size, so the parameter's
// memory can be reused when fn holds the last reference. Only nogc
// copies of functions are considered: only nogc code calls them, always
// directly.
func fgoParamTag(fn *ir.Func, f *types.Field) string {
	if !fn.FgoNogcABI() || !f.Type.FgoIsRCPtr() || len(fn.Body) == 0 || fn.IsClosure() || fn.Wrapper() {
		return ""
	}
	p, ok := f.Nname.(*ir.Name)
	if !ok || p == nil {
		return ""
	}
	class := fgoSizeClass(f.Type.Elem())
	owned := false
	is := func(n ir.Node) bool {
		for n != nil && n.Op() == ir.OCONVNOP {
			n = n.(*ir.ConvExpr).X
		}
		return n == p
	}
	var visit func(n ir.Node) bool
	visit = func(n ir.Node) bool {
		switch n.Op() {
		case ir.ORETURN:
			for _, r := range n.(*ir.ReturnStmt).Results {
				owned = owned || is(r)
			}
		case ir.OAS:
			owned = owned || is(n.(*ir.AssignStmt).Y)
		case ir.OAS2:
			for _, r := range n.(*ir.AssignListStmt).Rhs {
				owned = owned || is(r)
			}
		case ir.OSTRUCTKEY:
			owned = owned || is(n.(*ir.StructKeyExpr).Value)
		case ir.ONEW, ir.OPTRLIT:
			if t := n.Type().Elem(); t.FgoIsRC() && fgoSizeClass(t) == class {
				owned = true
			}
		case ir.OCALLFUNC:
			call := n.(*ir.CallExpr)
			if callee := ir.StaticCalleeName(call.Fun); callee != nil {
				params := callee.Type().RecvParams()
				for i, a := range call.Args {
					owned = owned || i < len(params) && types.FgoParamOwned(params[i]) && is(a)
				}
			}
		case ir.OCLOSURE:
			return false
		}
		return owned || ir.DoChildren(n, visit)
	}
	for _, n := range fn.Body {
		if visit(n) {
			break
		}
	}
	if owned {
		return types.FgoOwnedTag
	}
	return ""
}

func fgoSizeClass(t *types.Type) int64 { return (t.Size() + 15) / 16 }

// fgoStringRead treats a read of a nogc object's string or slice field
// like an allocation. The value points into a buffer the object owns,
// which is fine for uses that end with the frame (the counting pass keeps
// the object alive over them); a value that escapes is copied onto the GC
// heap instead, which ssagen does when the read is marked EscHeap.
func (e *escape) fgoStringRead(k hole, n ir.Node) bool {
	if !e.curfn.FgoNogc() || !ir.FgoBufField(n) && !ir.FgoPtrSliceField(n) {
		return false
	}
	e.spill(k, n)
	e.discard(n.(*ir.SelectorExpr).X)
	return true
}

// fgoStringStore handles a store into a nogc object's string or slice
// field, which copies the value: the stored value goes nowhere.
func (e *escape) fgoStringStore(n ir.Node) (hole, bool) {
	if !e.curfn.FgoNogc() || !ir.FgoBufField(n) && !ir.FgoPtrSliceField(n) {
		return hole{}, false
	}
	e.discard(n.(*ir.SelectorExpr).X)
	return e.discardHole(), true
}

// fgoPtrLits holds the struct literals that nogc code takes the address
// of, which are built by storing into each field of the new object.
var fgoPtrLits = map[ir.Node]bool{}

func (e *escape) fgoPtrLit(n *ir.AddrExpr) {
	if e.curfn.FgoNogc() && n.X.Op() == ir.OSTRUCTLIT {
		fgoPtrLits[n.X] = true
	}
}

// fgoStringElt is fgoStringStore for an element of such a struct literal.
func (e *escape) fgoStringElt(lit *ir.CompLitExpr, elt ir.Node) bool {
	kv := elt.(*ir.StructKeyExpr)
	if !fgoPtrLits[lit] || !types.FgoBufType(kv.Field.Type) && !types.FgoPtrSliceType(kv.Field.Type) {
		return false
	}
	e.discard(kv.Value)
	return true
}
