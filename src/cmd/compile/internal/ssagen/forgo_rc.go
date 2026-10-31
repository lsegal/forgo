// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/escape"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	rtabi "internal/abi"
)

// fgoNewObject allocates the structs nogc code allocates on the heap with
// the runtime's reference counted allocator instead. It returns nil
// elsewhere.
func (s *state) fgoNewObject(typ *types.Type) *ssa.Value {
	if !s.curfn.FgoNogc() || !typ.FgoIsRC() || typ.Size() == 0 {
		return nil
	}
	name := "fgoRCNew"
	if types.FgoMixed(typ) {
		name = "fgoRCNewMixed" // GC roots while alive; never reused
	}
	fn := typecheck.LookupRuntimeFunc(name)
	return s.rtcall(fn, true, []*types.Type{types.NewPtr(typ)}, s.reflectType(typ))[0]
}

// fgoStringRead copies v, just read from string or slice field n of a
// nogc object, onto the GC heap if escape analysis found that it outlives
// the frame. Otherwise v stays a view into the object's buffer, which the
// counting pass keeps alive over v's uses.
func (s *state) fgoStringRead(n ir.Node, v *ssa.Value) *ssa.Value {
	if !s.curfn.FgoNogc() {
		return v
	}
	if ir.FgoPtrSliceField(n) && n.Esc() != ir.EscNone {
		fn := typecheck.LookupRuntimeFunc("fgoPtrsClone")
		return s.rtcall(fn, true, []*types.Type{v.Type}, v)[0]
	}
	if !ir.FgoBufField(n) || n.Esc() == ir.EscNone {
		return v
	}
	if v.Type.IsSlice() {
		fn := typecheck.LookupRuntimeFunc("fgoSliceClone")
		return s.rtcall(fn, true, []*types.Type{v.Type}, v, s.fgoElemSize(v.Type))[0]
	}
	fn := typecheck.LookupRuntimeFunc("fgoStrClone")
	return s.rtcall(fn, true, []*types.Type{v.Type}, v)[0]
}

// fgoStringStore stores right into string or slice field left, selected
// through a struct pointer in nogc code, by copying it: into a buffer
// the object owns, freeing the one it replaces, if it is a counted
// object, and onto the GC heap otherwise.
func (s *state) fgoStringStore(left ir.Node, right *ssa.Value, deref bool) bool {
	if !s.curfn.FgoNogc() || !ir.FgoBufField(left) && !ir.FgoPtrSliceField(left) {
		return false
	}
	t := left.Type()
	switch {
	case right == nil:
		right = s.zeroVal(t)
	case deref:
		right = s.load(t, right)
	}
	addr, base := s.fgoFieldAddr(left)
	if types.FgoPtrSliceType(t) {
		// right was built by fgoPtrsMake (see walk's fgoBuildPtrSlices),
		// or is nil: the field takes it over.
		fn := typecheck.LookupRuntimeFunc("fgoPtrsPut")
		s.rtcall(fn, true, nil, addr, right, base)
		return true
	}
	if t.IsSlice() {
		fn := typecheck.LookupRuntimeFunc("fgoSliceSet")
		s.rtcall(fn, true, nil, addr, right, s.fgoElemSize(t), base)
		return true
	}
	fn := typecheck.LookupRuntimeFunc("fgoStrSet")
	s.rtcall(fn, true, nil, addr, right, base)
	return true
}

// fgoTaggedField reports whether n selects a struct field tagged
// `fgo:"nogc"`, which holds a counted reference (see
// runtime/forgo_rc_tag.go).
func fgoTaggedField(n ir.Node) bool {
	if n.Op() != ir.ODOT && n.Op() != ir.ODOTPTR {
		return false
	}
	sel := n.(*ir.SelectorExpr)
	return sel.Selection != nil && types.FgoTagged(sel.Selection)
}

// fgoTagStore stores right into tagged field left, in code that is not
// nogc, through runtime.fgoTagStore, which counts the field's reference.
// (nogc code's stores are the counting pass's.)
func (s *state) fgoTagStore(left ir.Node, right *ssa.Value, deref bool) bool {
	if s.curfn.FgoNogc() || !fgoTaggedField(left) {
		return false
	}
	t := left.Type()
	switch {
	case right == nil:
		right = s.zeroVal(t)
	case deref:
		right = s.load(t, right)
	}
	fn := typecheck.LookupRuntimeFunc("fgoTagStore")
	s.rtcall(fn, true, nil, s.addr(left), right)
	return true
}

// fgoTagRead hands the object v points to, just read from tagged field
// n by code that is not nogc, to the GC: that code keeps v without
// counting it. Comparing the field with nil reads it as is.
func (s *state) fgoTagRead(n ir.Node, v *ssa.Value) *ssa.Value {
	if s.curfn.FgoNogc() || s.fgoTagRaw || !fgoTaggedField(n) {
		return v
	}
	fn := typecheck.LookupRuntimeFunc("fgoTagLoad")
	return s.rtcall(fn, true, []*types.Type{v.Type}, v)[0]
}

// fgoCompared evaluates x, an operand of a comparison, which keeps no
// pointer it reads from a tagged field.
func (s *state) fgoCompared(x ir.Node) *ssa.Value {
	if !fgoTaggedField(x) {
		return s.expr(x)
	}
	s.fgoTagRaw = true
	defer func() { s.fgoTagRaw = false }()
	return s.expr(x)
}

// fgoFieldAddr returns the address of field n (p.f or (*p).f) and p.
func (s *state) fgoFieldAddr(n ir.Node) (addr, base *ssa.Value) {
	sel := n.(*ir.SelectorExpr)
	x := sel.X
	if sel.Op() == ir.ODOT {
		x = x.(*ir.StarExpr).X
	}
	base = s.exprPtr(x, sel.Bounded(), sel.Pos())
	return s.newValue1I(ssa.OpOffPtr, types.NewPtr(n.Type()), sel.Offset(), base), base
}

func (s *state) fgoElemSize(t *types.Type) *ssa.Value {
	return s.constInt(types.Types[types.TUINTPTR], t.Elem().Size())
}

// fgoShadowInfos maps a function's LSym to its FUNCDATA_FgoShadow data.
var fgoShadowInfos sync.Map

// fgoShadowInfo describes, once fn's frame is laid out, the stack slots in
// which reference counting keeps fn's owned nogc pointers and reuse
// tokens, and string copies on their way to a store: for each of the
// three lists, a uvarint count, then each slot's offset below varp. The
// runtime releases owned pointers and frees tokens and copies still held
// when recover() or Goexit discards the frame.
func fgoShadowInfo(fn *ir.Func) {
	sh := ssa.FgoTakeShadowSlots(fn)
	offsets := func(names []*ir.Name) (offs []int64) {
		for _, n := range names {
			if n.Used() {
				offs = append(offs, -n.FrameOffset())
			}
		}
		return offs
	}
	owned, tokens, bufs := offsets(sh.Owned), offsets(sh.Tokens), offsets(sh.Bufs)
	if len(owned)+len(tokens)+len(bufs) == 0 {
		return
	}
	x := base.Ctxt.Lookup(fn.LSym.Name + ".fgoshadow")
	x.Set(obj.AttrContentAddressable, true)
	x.Align = 1
	off := 0
	for _, list := range [][]int64{owned, tokens, bufs} {
		off = objw.Uvarint(x, off, uint64(len(list)))
		for _, o := range list {
			off = objw.Uvarint(x, off, uint64(o))
		}
	}
	fgoShadowInfos.Store(fn.LSym, x)
}

func fgoEmitShadowInfo(e *ssafn, pp *objw.Progs) {
	x, ok := fgoShadowInfos.LoadAndDelete(e.curfn.LSym)
	if !ok {
		return
	}
	p := pp.Prog(obj.AFUNCDATA)
	p.From.SetConst(rtabi.FUNCDATA_FgoShadow)
	p.To.Type = obj.TYPE_MEM
	p.To.Name = obj.NAME_EXTERN
	p.To.Sym = x.(*obj.LSym)
}

// fgoNoteKeeps records, for nogc code's calls to sym, what the callee may
// do with each parameter, from their escape analysis tags: nogc code
// hands its objects to the GC before passing them to a callee that may
// keep them or change their pointers (see ssa's fgoRCPass.handsOff).
func (s *state) fgoNoteKeeps(sym *obj.LSym, t *types.Type) {
	if !s.curfn.FgoNogc() {
		return
	}
	var keeps []ssa.FgoKeep
	for _, f := range t.RecvParams() {
		l := escape.ParseLeaks(f.Note)
		k := ssa.FgoKeep{Heap: l.Heap() >= 0, Callee: l.Callee() >= 0, Mutator: l.Mutator() >= 0}
		for i := range 5 {
			k.Result = k.Result || l.Result(i) >= 0
		}
		keeps = append(keeps, k)
	}
	ssa.FgoNoteKeeps(sym, keeps)
}
