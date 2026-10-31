// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"maps"
	"math/bits"
	"slices"
	"strings"
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/src"
)

// fgoRC inserts reference counting for nogc pointers (pointers to
// structs, in nogc code: see cmd/compile/internal/noder's forgo_nogc.go),
// Perceus style: each owned value is
// released right after its last use, and a release that is followed by
// an allocation of the same size in the same block hands its memory to
// that allocation when it was the last reference.
//
// Conventions: parameters are borrowed (the caller keeps them alive for
// the whole call) and results are owned (+1). A value loaded from a
// field is borrowed from the object holding it:
//   - if every use of the load reads fields under the same memory state,
//     nothing can free it before those uses, so it needs no count;
//   - if the field is never reassigned after construction, the holder
//     keeps it alive for as long as the holder is alive, so it needs no
//     count either, and the holder's lifetime is extended over its uses;
//   - otherwise it is counted (+1) right after the load.
//
// The pass runs before call expansion, so calls still carry their
// arguments directly, and it finishes by rebuilding the memory chain
// around the calls it inserts.
func fgoRC(f *Func) {
	if !fgoRCNeeded(f) {
		return
	}
	p := &fgoRCPass{f: f}
	p.run()
}

func fgoRCNeeded(f *Func) bool {
	if fn := f.fe.Func(); fn == nil || !fn.FgoNogc() {
		return false
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Type.FgoIsRCPtr() {
				return true
			}
			if v.Op == OpStore && v.Aux.(*types.Type).FgoIsRCPtr() {
				return true
			}
			if fgoIsCall(v) {
				// A nogc result the caller ignores still needs releasing.
				aux := v.Aux.(*AuxCall)
				for i := int64(0); i < aux.NResults(); i++ {
					if aux.TypeOfResult(i).FgoIsRCPtr() {
						return true
					}
				}
			}
		}
	}
	return false
}

const fgoEnd = 1 << 30 // position of the block's end (control, results)

type fgoKind uint8

const (
	fgoOwned   fgoKind = iota + 1 // call result, phi, or counted load
	fgoParam                      // borrowed parameter
	fgoElided                     // load used only under its own memory state
	fgoDerived                    // load from an immutable field, kept alive by owner
)

type fgoDef struct {
	v     *Value
	kind  fgoKind
	owner *fgoDef // fgoDerived: the def keeping this one alive (owned or param)
	uses  []fgoUse
	extra []fgoUse // uses of values derived from this one (owned only)
	block *Block
	slot  int // slot right after the definition (owned only)
	pos   int // position of the definition (owned only)
}

// fgoElem is where an element of a slice of nogc pointers came from: the
// object holding the slice, and whether its field is never reassigned
// (the elements of such a slice never change).
type fgoElem struct {
	root      *Value
	immutable bool
}

type fgoUse struct {
	b         *Block
	pos       int
	consuming bool // ownership moves to the user (store, result, phi)
	noreturn  bool // argument of a call that never returns
	call      bool // argument of a call
	moved     bool
}

type fgoReq struct {
	b      *Block
	slot   int
	order  int // within a slot: see the fgoOrder constants
	inc    bool
	clone  bool   // copy string or slice v onto the GC heap, and use the copy instead
	dup    *Value // clone: copy v for this fgoStrSet or fgoSliceSet call to take instead
	v      *Value
	reuse  *Value // allocation call this release hands its memory to
	reuseB *Block // and where that call is: block, slot just before it
	reuseS int
	tok    *Value
	tokOf  *fgoReq  // shadow set: store tokOf's reuse token
	clear  *fgoReq  // release: the request forgetting its shadow slot
	shadow *ir.Name // set (v != nil), clear (v == nil), or keep alive this slot
	keep   bool
	escape bool            // hand v's object to the GC (fgoRCHandoff)
	keepOf int64           // release: words of v's object whose loads take its references (planKeep)
	fused  bool            // inc: done by a release's keepOf instead
	sunkTo map[*Block]bool // release: blocks planSink moved it through
}

// Order of inserted operations within one slot.
const (
	fgoOrderZero    = -3 // clear shadow slots at function entry
	fgoOrderOwn     = -2 // record a value just owned (call result, phi, param) in its shadow slot
	fgoOrderClone   = -1 // copy a string or slice, before anything at the slot uses the copy
	fgoOrderInc     = 0
	fgoOrderSet     = 1 // record a counted load, reuse token or string copy in its shadow slot
	fgoOrderClear   = 2 // forget it, just before releasing it or handing it on
	fgoOrderRelease = 3
	fgoOrderKeep    = 4 // keep shadow slots alive to the return
)

type fgoRCPass struct {
	f    *Func
	syms struct {
		new, newAt, inc, dec, dropReuse  *obj.LSym
		decKeep, dropReuseKeep           *obj.LSym
		decAll, dropReuseAll             *obj.LSym
		newInit, newAtInit               *obj.LSym
		newMixed, escape, ptrsClone      *obj.LSym
		strClone, strSet, strDup, strPut *obj.LSym
		sliceClone, sliceSet, sliceDup   *obj.LSym
		take, appendN                    *obj.LSym
		slicePut                         *obj.LSym
		store, storeGC, vet              *obj.LSym
		storeTag, storeTagGC             *obj.LSym
	}
	nogcABI bool              // the function follows the nogc calling convention
	strict  bool              // //fgo:nogc strict: hand-offs are errors
	cur     *Value            // the use being looked at
	names   map[*Value]string // value -> a variable it is held in
	refused map[src.XPos]bool // strict errors already reported
	vets    []fgoVet

	chain   map[*Block][]*Value // memory values in order
	idx     map[*Value]int      // memory value -> 1-based index in its block's chain
	callMem map[*Value]*Value   // call -> its memory result
	callRes map[*Value]*Value   // call -> its single nogc pointer result
	tupMem  map[*Value]*Value   // tuple op (an atomic) -> its memory
	defs    map[*Value]*fgoDef
	users   map[*Value][]*Value
	elems   map[*Value]fgoElem // nogc pointer loaded from a view of a slice field
	order   []*fgoDef
	reqs    []*fgoReq
	initMem *Value
	failed  bool

	shadow map[*fgoDef]*ir.Name // stack slot holding each owned value while owned
	tokens []*ir.Name           // stack slots holding reuse tokens across code
	bufs   []*ir.Name           // stack slots holding string copies on their way to a store
	sp     *Value
}

func (p *fgoRCPass) errorf(pos src.XPos, format string, args ...any) {
	base.ErrorfAt(pos, 0, "nogc: "+format, args...)
	p.failed = true
}

func (p *fgoRCPass) lookup(name string) *obj.LSym {
	return p.f.Config.ctxt.LookupABI("runtime."+name, obj.ABIInternal)
}

func (p *fgoRCPass) run() {
	f := p.f
	p.syms.new = p.lookup("fgoRCNew")
	p.syms.newAt = p.lookup("fgoRCNewAt")
	p.syms.newMixed = p.lookup("fgoRCNewMixed")
	p.syms.escape = p.lookup("fgoRCHandoff")
	p.syms.store = p.lookup("fgoRCStore")
	p.syms.storeGC = p.lookup("fgoRCStoreGC")
	p.syms.storeTag = p.lookup("fgoRCStoreTag")
	p.syms.storeTagGC = p.lookup("fgoRCStoreTagGC")
	p.syms.vet = p.lookup("fgoRCVet")
	p.nogcABI = f.fe.Func().FgoNogcABI()
	p.strict = f.fe.Func().FgoNogcStrict()
	p.syms.ptrsClone = p.lookup("fgoPtrsClone")
	p.syms.inc = p.lookup("fgoRCInc")
	p.syms.dec = p.lookup("fgoRCDec")
	p.syms.dropReuse = p.lookup("fgoRCDropReuse")
	p.syms.decKeep = p.lookup("fgoRCDecKeep")
	p.syms.dropReuseKeep = p.lookup("fgoRCDropReuseKeep")
	p.syms.decAll = p.lookup("fgoRCDecAll")
	p.syms.dropReuseAll = p.lookup("fgoRCDropReuseAll")
	p.syms.newInit = p.lookup("fgoRCNewInit")
	p.syms.newAtInit = p.lookup("fgoRCNewAtInit")
	p.syms.strClone = p.lookup("fgoStrClone")
	p.syms.strSet = p.lookup("fgoStrSet")
	p.syms.strDup = p.lookup("fgoStrDup")
	p.syms.strPut = p.lookup("fgoStrPut")
	p.syms.sliceClone = p.lookup("fgoSliceClone")
	p.syms.sliceSet = p.lookup("fgoSliceSet")
	p.syms.sliceDup = p.lookup("fgoSliceDup")
	p.syms.slicePut = p.lookup("fgoSlicePut")
	p.syms.take = p.lookup("fgoPtrsTake")
	p.syms.appendN = p.lookup("fgoPtrsAppendN")

	p.splitCritical()
	p.stripFreshZero()
	if p.failed {
		return
	}
	p.lowerStores()
	p.discardedResults()
	p.takeElems()
	p.hoistImmutable()
	p.hoistLoads()
	p.buildChains()
	if p.failed {
		return
	}
	p.collectDefs()
	p.collectUses()
	if p.failed {
		return
	}
	p.classifyLoads()
	if p.failed {
		return
	}
	p.planStores()
	p.planDefs()
	if p.failed {
		return
	}
	p.planReuse()
	p.planReuseAcross()
	p.planSink()
	p.planKeep()
	p.planTokens()
	p.planShadows()
	p.materialize()
	for _, v := range p.vets {
		p.vetIf(v.objs, v.m, v.masks, v.keep)
	}
	p.inlineCounts()
	if len(p.shadow)+len(p.tokens)+len(p.bufs) > 0 {
		var owned []*ir.Name
		for _, n := range p.shadow {
			owned = append(owned, n)
		}
		fgoShadowSlots.Store(f.fe.Func(), FgoShadow{Owned: owned, Tokens: p.tokens, Bufs: p.bufs})
	}
	f.invalidateCFG()
}

// splitCritical splits every edge from a block with several successors
// to a block with several predecessors, so a release needed on one edge
// always has a block of its own to go in.
func (p *fgoRCPass) splitCritical() {
	f := p.f
	for i := 0; i < len(f.Blocks); i++ {
		b := f.Blocks[i]
		if len(b.Succs) < 2 {
			continue
		}
		for si := range b.Succs {
			e := b.Succs[si]
			s := e.b
			if len(s.Preds) < 2 {
				continue
			}
			d := f.NewBlock(BlockPlain)
			d.Pos = s.Pos
			d.Preds = append(d.Preds, Edge{b, si})
			d.Succs = append(d.Succs, Edge{s, e.i})
			b.Succs[si] = Edge{d, 0}
			s.Preds[e.i] = Edge{d, 0}
		}
	}
	f.invalidateCFG()
}

// stripFreshZero removes the zeroing that composite literals emit right
// after allocating: the allocator already returns zeroed memory. (The
// generic rules only drop it after runtime.newobject.)
func (p *fgoRCPass) stripFreshZero() {
	for _, b := range p.f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpZero {
				continue
			}
			base, off := fgoAddrBase(v.Args[0])
			if base == nil || off != 0 || v.AuxInt != base.Type.Elem().Size() {
				continue
			}
			m := v.MemoryArg()
			if base.Op == OpSelectN && fgoIsCall(base.Args[0]) && p.isNew(base.Args[0].Aux.(*AuxCall).Fn) &&
				m.Op == OpSelectN && m.Args[0] == base.Args[0] {
				v.copyOf(m)
			}
		}
	}
}

// lowerStores turns each store into a nogc pointer field, other than
// those initializing a new object, into an fgoRCStore call: the object
// may be a counted one, which takes the stored reference, or anything
// else, which holds it as a GC value. A store into a field through which
// the object can reach itself again calls fgoRCStoreGC, which hands the
// object to the GC first. The stores initializing new objects stay, with
// no write barrier (nothing else can see the object yet), and an
// fgoRCVet call after them hands the object off if any stored a pointer
// it cannot count.
func (p *fgoRCPass) lowerStores() {
	f := p.f
	memOf := map[*Value]*Value{}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpSelectN && v.Type.IsMemory() && fgoIsCall(v.Args[0]) {
				memOf[v.Args[0]] = v
			}
		}
	}
	fresh := func(st *Value) *Value {
		base, off := fgoAddrBase(st.Args[0])
		if base == nil || off < 0 || base.Op != OpSelectN {
			return nil
		}
		call := base.Args[0]
		if !fgoIsCall(call) || !p.isNew(call.Aux.(*AuxCall).Fn) {
			return nil
		}
		// Everything since the allocation must only have written into
		// the object (or allocated and filled in others): nothing else
		// has seen it yet.
		for m := st.MemoryArg(); ; m = m.MemoryArg() {
			if m == memOf[call] {
				return base
			}
			if m.Block != st.Block {
				return nil
			}
			switch m.Op {
			case OpCopy:
				continue
			case OpStore, OpMove, OpZero:
				// Into the object, or into another new one.
				if b2, _ := fgoAddrBase(m.Args[0]); b2 != base && !(b2 != nil && b2.Op == OpSelectN && fgoIsCall(b2.Args[0]) && p.isNew(b2.Args[0].Aux.(*AuxCall).Fn)) {
					return nil
				}
				if m.Op == OpStore && fgoAlias(m.Args[1]) == base || m.Op == OpMove && fgoAddrBaseOrNil(m.Args[1]) == base {
					return nil
				}
				continue
			case OpSelectN:
				if !fgoIsCall(m.Args[0]) {
					return nil
				}
				continue
			case OpStaticLECall, OpClosureLECall, OpInterLECall:
				c := m
				if fgoFillCall(c) {
					continue // it does not keep the object
				}
				for _, a := range c.Args {
					if a.Type.IsPtrShaped() && fgoAddrBaseOrNil(a) == base {
						return nil // it may keep the object
					}
				}
				continue
			}
			return nil
		}
	}
	// Fresh stores of pointers the object may not be able to count are
	// vetted before anything that can run the GC (a call), and after
	// the object's last such store.
	untrusted := map[*Value]int64{} // store -> its word, for vetting
	objOf := map[*Value]*Value{}    // store -> its object
	lastOf := map[*Value]*Value{}   // object -> its last fresh store
	var objs []*Value
	ptr := int64(f.Config.PtrSize)
	for _, b := range f.Blocks {
		for _, st := range slices.Clone(b.Values) {
			if st.Op != OpStore || !st.Aux.(*types.Type).FgoIsRCPtr() || !fgoRCFieldAt(st.Args[0]) {
				continue
			}
			base, off := fgoAddrBase(st.Args[0])
			if obj := fresh(st); obj != nil {
				if f.fgoNoWB == nil {
					f.fgoNoWB = map[ID]bool{}
				}
				f.fgoNoWB[st.ID] = true
				if _, ok := lastOf[obj]; !ok {
					lastOf[obj] = nil
					objs = append(objs, obj)
				}
				objOf[st] = obj
				if v := fgoAlias(st.Args[1]); v.Op != OpConstNil && !(v.Op == OpSelectN && fgoIsCall(v.Args[0]) && p.isNew(v.Args[0].Aux.(*AuxCall).Fn)) && off/ptr < 64 {
					untrusted[st] = 1 << (off / ptr)
				}
				continue
			}
			fn := p.syms.store
			var fld *types.Field
			if base != nil && off >= 0 {
				fld = fgoFieldAt(base.Type.Elem(), off)
			}
			if tf := fgoTaggedAddr(st.Args[0]); tf != nil {
				fld = tf // maybe nested in a field or array element
			}
			tagged := fld != nil && types.FgoTagged(fld)
			if base == nil {
				base = st.Args[0] // a tagged field of memory nogc code does not count
			}
			fromNogc := p.fromNogc(base, map[*Value]bool{})
			if fld != nil && base.Type.Elem().IsStruct() && types.FgoRecursiveField(base.Type.Elem(), fld) {
				fn = p.syms.storeGC
				if tagged {
					fn = p.syms.storeTagGC // counted unless base is a counted object
				}
				if p.strict && st.Args[1].Op != OpConstNil && (!tagged || fromNogc) {
					p.refuseAt(st.Pos, st.Args[1], "assigned to field "+fld.Sym.Name+", through which the object can reach itself (reference cycles are the GC's)")
				}
			} else if tagged {
				// Counted wherever base is: a GC object holds the
				// reference itself.
				fn = p.syms.storeTag
			} else if p.strict && st.Args[1].Op != OpConstNil && !fromNogc {
				p.refuseAt(st.Pos, st.Args[1], "stored in an object nogc code did not allocate, which may be garbage collected (tag the field `fgo:\"nogc\"` to count it there)")
			}
			_, mem, _ := p.callAt(b, st.Pos, fn, []*Value{base, st.Args[0], st.Args[1]}, nil, st.MemoryArg())
			st.copyOf(mem)
		}
	}
	if len(untrusted) == 0 {
		return
	}
	// The next memory operation after each memory state, in each block.
	next := map[*Value]*Value{}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpPhi {
				continue
			}
			if m := v.MemoryArg(); m != nil && (v.Type.IsMemory() || fgoIsCall(v)) {
				next[m] = v
			}
		}
	}
	for st, obj := range objOf {
		if l := lastOf[obj]; l == nil || p.chainAfter(next, l, st) {
			lastOf[obj] = st
		}
	}
	type vetPoint struct {
		obj, m *Value // vet obj right after memory state m
		mask   int64
		vals   []*Value // the values stored, to keep alive until then
		calls  bool     // whether calls come between the stores and m
	}
	var points []vetPoint
	for _, obj := range objs {
		call := obj.Args[0]
		var pt vetPoint
		pt.obj = obj
		vetAt := func(m *Value) {
			pt.m = m
			points = append(points, pt)
			pt = vetPoint{obj: obj}
		}
		for m := memOf[call]; m != nil; {
			op := next[m]
			if op == nil {
				break
			}
			if fgoIsCall(op) {
				if pt.mask != 0 {
					if !p.isNew(op.Aux.(*AuxCall).Fn) {
						vetAt(m) // before a call that may keep obj
					} else {
						pt.calls = true
					}
				}
				m = p.memOfCall(op)
				continue
			}
			if objOf[op] == obj {
				if bit := untrusted[op]; bit != 0 {
					pt.mask |= bit
					pt.vals = append(pt.vals, op.Args[1])
				}
				if op == lastOf[obj] {
					if pt.mask != 0 {
						vetAt(op)
					}
					break
				}
			}
			m = op
		}
	}
	// Move each check past what follows it of the same composite literal
	// (stores into new objects, and their allocations), so that a nested
	// literal gets one check at its end and stays in one block.
	at := map[*Value]int{}
	for _, pt := range points {
		m := pt.m
		for {
			n := next[m]
			if n == nil {
				break
			}
			if fgoIsCall(n) && p.isNew(n.Aux.(*AuxCall).Fn) {
				pt.calls = true
				m = p.memOfCall(n)
				continue
			}
			if n.Op != OpStore && n.Op != OpCopy || n.Op == OpStore && objOf[n] == nil {
				break
			}
			m = n
		}
		i, ok := at[m]
		if !ok {
			i = len(p.vets)
			at[m] = i
			p.vets = append(p.vets, fgoVet{m: m})
		}
		v := &p.vets[i]
		v.objs = append(v.objs, pt.obj)
		v.masks = append(v.masks, pt.mask)
		if pt.calls {
			v.keep = append(v.keep, pt.vals...)
		}
	}
}

// fgoArenaSize is the size of the runtime's nogc arena
// (runtime.fgoArenaSize).
const fgoArenaSize = 64 << 30

// fgoVet is a check that new objects hold only pointers they can count,
// made once counting is planned (so that it does not split the blocks
// the pass looks at).
type fgoVet struct {
	m     *Value // right after this memory state
	objs  []*Value
	masks []int64
	keep  []*Value // stored values to keep alive until the check
}

// vetIf calls fgoRCVet(obj, mask) right after memory state m if any of
// obj's words in mask may hold a pointer obj cannot count: not nil and
// not a nogc object. The check is inline: a pointer into a chunk of the
// arena that holds no handed-off objects is a counted object (see
// runtime.fgoState). The call, which checks exactly, happens only for
// others.
func (p *fgoRCPass) vetIf(objs []*Value, m *Value, masks []int64, keep []*Value) {
	f := p.f
	b := m.Block
	pos := m.Pos.WithNotStmt()
	type use struct {
		v *Value
		i int
	}
	var uses []use
	for _, bb := range f.Blocks {
		for _, v := range bb.Values {
			for i, a := range v.Args {
				if a == m {
					uses = append(uses, use{v, i})
				}
			}
		}
	}
	// Calls came between some stores and here: the GC must still see
	// what they stored.
	m0 := m
	var kept []*Value
	for _, v := range keep {
		m = b.NewValue2(pos, OpKeepAlive, types.TypeMem, v, m)
		kept = append(kept, m)
	}
	vets := func(b *Block, mem *Value) *Value {
		for i, obj := range objs {
			_, mem, _ = p.callAt(b, pos, p.syms.vet, []*Value{obj, p.uintptrConst(masks[i])}, nil, mem)
		}
		return mem
	}
	if f.Config.PtrSize != 8 {
		mem := vets(b, m)
		for _, u := range uses {
			u.v.SetArg(u.i, mem)
		}
		return
	}
	ts := f.Config.Types
	ours := map[*Value]bool{}
	nv := func(v *Value) *Value { ours[v] = true; return v }
	for _, k := range kept {
		ours[k] = true
	}
	sb := nv(b.NewValue0(pos, OpSB, ts.Uintptr))
	arena := nv(b.NewValue1A(pos, OpAddr, ts.UintptrPtr, f.Config.ctxt.Lookup("runtime.fgoArena"), sb))
	ld := func(off int64, t *types.Type) *Value {
		a := nv(b.NewValue1I(pos, OpOffPtr, types.NewPtr(t), off, arena))
		return nv(b.NewValue2(pos, OpLoad, t, a, m))
	}
	lo, size, chunks := ld(0, ts.Uintptr), ld(8, ts.Uintptr), ld(16, ts.Uintptr)
	var bad *Value
	for i, obj := range objs {
		for k := masks[i]; k != 0; k &= k - 1 {
			// The word as stored, read back as an integer, which the
			// counting pass does not follow.
			off := int64(bits.TrailingZeros64(uint64(k))) * 8
			u := nv(b.NewValue2(pos, OpLoad, ts.Uintptr, nv(b.NewValue1I(pos, OpOffPtr, ts.UintptrPtr, off, obj)), m))
			d := nv(b.NewValue2(pos, OpSub64, ts.Uintptr, u, lo))
			in := nv(b.NewValue2(pos, OpLess64U, ts.Bool, d, size))
			in64 := nv(b.NewValue1(pos, OpZeroExt8to64, ts.Uintptr, nv(b.NewValue1(pos, OpCvtBoolToUint8, ts.UInt8, in))))
			idx := nv(b.NewValue2(pos, OpMul64, ts.Uintptr, nv(b.NewValue2(pos, OpLsh64x64, ts.Uintptr, nv(b.NewValue2(pos, OpRsh64Ux64, ts.Uintptr, d, p.uintptrConst(16))), p.uintptrConst(2))), in64))
			cp := nv(b.NewValue2(pos, OpAdd64, ts.Uintptr, chunks, idx))
			flag := nv(b.NewValue2(pos, OpLoad, ts.UInt32, nv(b.NewValue2(pos, OpConvert, ts.UInt32Ptr, cp, m)), m))
			plain := nv(b.NewValue2(pos, OpEq32, ts.Bool, flag, nv(f.ConstInt32(ts.UInt32, 0))))
			ok := nv(b.NewValue2(pos, OpAndB, ts.Bool, in, plain))
			nonNil := nv(b.NewValue2(pos, OpNeq64, ts.Bool, u, p.uintptrConst(0)))
			bv := nv(b.NewValue2(pos, OpAndB, ts.Bool, nonNil, nv(b.NewValue1(pos, OpNot, ts.Bool, ok))))
			if bad == nil {
				bad = bv
			} else {
				bad = nv(b.NewValue2(pos, OpOrB, ts.Bool, bad, bv))
			}
		}
	}

	// b: ...; if bad { vet } ; cont: the rest of b.
	cont := p.splitAt(b, m0, ours)
	b.Kind = BlockIf
	b.SetControl(bad)
	b.Likely = BranchUnlikely
	then, els := f.NewBlock(BlockPlain), f.NewBlock(BlockPlain)
	then.Pos, els.Pos = pos, pos
	b.AddEdgeTo(then)
	b.AddEdgeTo(els)
	then.AddEdgeTo(cont)
	els.AddEdgeTo(cont)
	vmem := vets(then, m)
	mphi := cont.NewValue2(pos, OpPhi, types.TypeMem, vmem, m)
	for _, u := range uses {
		u.v.SetArg(u.i, mphi)
	}
	f.invalidateCFG()
}

// splitAt moves the values of b that use seed, and those depending on
// them (other than skip's), into a new block that takes over b's kind,
// controls and successors, and returns it. b is left with no successors.
func (p *fgoRCPass) splitAt(b *Block, seed *Value, skip map[*Value]bool) *Block {
	f := p.f
	cont := f.NewBlock(b.Kind)
	cont.Pos, cont.Likely, cont.AuxInt, cont.Aux = b.Pos, b.Likely, b.AuxInt, b.Aux
	for i, e := range b.Succs {
		cont.Succs = append(cont.Succs, e)
		e.b.Preds[e.i] = Edge{cont, i}
	}
	cont.CopyControls(b)
	b.ResetControls()
	b.Succs = b.Succs[:0]
	moved := map[*Value]bool{}
	for changed := true; changed; {
		changed = false
		for _, v := range b.Values {
			if moved[v] || skip[v] || v.Op == OpPhi {
				continue
			}
			for _, a := range v.Args {
				if a == seed || moved[a] {
					moved[v] = true
					changed = true
					break
				}
			}
		}
	}
	stay := b.Values[:0]
	for _, v := range b.Values {
		if moved[v] {
			v.Block = cont
			cont.Values = append(cont.Values, v)
		} else {
			stay = append(stay, v)
		}
	}
	clear(b.Values[len(stay):])
	b.Values = stay
	return cont
}

// inlineCounts turns the fgoRCInc and fgoRCDec calls the pass made into
// inline code for counted objects, the common case: a nogc pointer into
// a chunk of the arena that holds no handed-off objects (see
// runtime.fgoState) has its count right before it, which only this
// goroutine changes. Anything else still calls the runtime.
func (p *fgoRCPass) inlineCounts() {
	f := p.f
	if f.Config.PtrSize != 8 {
		return
	}
	var calls []*Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpStaticLECall {
				if fn := v.Aux.(*AuxCall).Fn; fn == p.syms.inc || fn == p.syms.dec {
					calls = append(calls, v)
				}
			}
		}
	}
	if len(calls) == 0 {
		return
	}
	ts := f.Config.Types
	release := p.lookup("fgoRCRelease")
	for _, call := range calls {
		b := call.Block
		pos := call.Pos.WithNotStmt()
		ptr, m := call.Args[0], call.Args[1]
		mo := p.memOfCall(call)
		inc := call.Aux.(*AuxCall).Fn == p.syms.inc
		ours := map[*Value]bool{}
		nv := func(v *Value) *Value { ours[v] = true; return v }

		// fast := ptr is in the arena, in a chunk with no handed-off
		// objects (the chunk flag read is from chunk 0 otherwise).
		sb := nv(b.NewValue0(pos, OpSB, ts.Uintptr))
		arena := nv(b.NewValue1A(pos, OpAddr, ts.UintptrPtr, f.Config.ctxt.Lookup("runtime.fgoArena"), sb))
		ld := func(off int64) *Value {
			a := nv(b.NewValue1I(pos, OpOffPtr, ts.UintptrPtr, off, arena))
			return nv(b.NewValue2(pos, OpLoad, ts.Uintptr, a, m))
		}
		lo, chunks := ld(0), ld(16)
		u := nv(b.NewValue2(pos, OpConvert, ts.Uintptr, ptr, m))
		d := nv(b.NewValue2(pos, OpSub64, ts.Uintptr, u, lo))
		in := nv(b.NewValue2(pos, OpLess64U, ts.Bool, d, p.uintptrConst(fgoArenaSize)))
		in64 := nv(b.NewValue1(pos, OpZeroExt8to64, ts.Uintptr, nv(b.NewValue1(pos, OpCvtBoolToUint8, ts.UInt8, in))))
		idx := nv(b.NewValue2(pos, OpMul64, ts.Uintptr, nv(b.NewValue2(pos, OpLsh64x64, ts.Uintptr, nv(b.NewValue2(pos, OpRsh64Ux64, ts.Uintptr, d, p.uintptrConst(16))), p.uintptrConst(2))), in64))
		flag := nv(b.NewValue2(pos, OpLoad, ts.UInt32, nv(b.NewValue2(pos, OpConvert, ts.UInt32Ptr, nv(b.NewValue2(pos, OpAdd64, ts.Uintptr, chunks, idx)), m)), m))
		fast := nv(b.NewValue2(pos, OpAndB, ts.Bool, in, nv(b.NewValue2(pos, OpEq32, ts.Bool, flag, nv(f.ConstInt32(ts.UInt32, 0))))))
		nonNil := nv(b.NewValue1(pos, OpIsNonNil, ts.Bool, ptr))

		// Users of the call's memory now follow the new blocks.
		type use struct {
			v *Value
			i int
		}
		var uses []use
		for _, bb := range f.Blocks {
			for _, v := range bb.Values {
				for i, a := range v.Args {
					if a == mo {
						uses = append(uses, use{v, i})
					}
				}
			}
			for i, c := range bb.ControlValues() {
				if c == mo {
					uses = append(uses, use{nil, i})
					_ = bb
				}
			}
		}
		skip := map[*Value]bool{call: true, mo: true}
		for v := range ours {
			skip[v] = true
		}
		cont := p.splitAt(b, mo, skip)

		// b: if ptr != nil { nn } else { cont }
		// nn: if fast { fastB } else { slow }
		nn, fastB, slow, skipB := f.NewBlock(BlockIf), f.NewBlock(BlockPlain), f.NewBlock(BlockPlain), f.NewBlock(BlockPlain)
		for _, x := range []*Block{nn, fastB, slow, skipB} {
			x.Pos = pos
		}
		b.Kind = BlockIf
		b.SetControl(nonNil)
		b.AddEdgeTo(nn)
		b.AddEdgeTo(skipB)
		nn.SetControl(fast)
		nn.Likely = BranchLikely
		nn.AddEdgeTo(fastB)
		nn.AddEdgeTo(slow)

		_, smem, _ := p.callAt(slow, pos, call.Aux.(*AuxCall).Fn, []*Value{ptr}, nil, m)
		slow.AddEdgeTo(cont)
		skipB.AddEdgeTo(cont)

		hdr := fastB.NewValue1I(pos, OpOffPtr, ts.UintptrPtr, -16, ptr)
		rc := fastB.NewValue2(pos, OpLoad, ts.Uintptr, hdr, m)
		var mems []*Value
		if inc {
			st := fastB.NewValue3A(pos, OpStore, types.TypeMem, ts.Uintptr, hdr, fastB.NewValue2(pos, OpAdd64, ts.Uintptr, rc, p.uintptrConst(1)), m)
			fastB.AddEdgeTo(cont)
			mems = []*Value{st, smem, m}
		} else {
			nrc := fastB.NewValue2(pos, OpSub64, ts.Uintptr, rc, p.uintptrConst(1))
			st := fastB.NewValue3A(pos, OpStore, types.TypeMem, ts.Uintptr, hdr, nrc, m)
			fastB.Kind = BlockIf
			fastB.SetControl(fastB.NewValue2(pos, OpEq64, ts.Bool, nrc, p.uintptrConst(0)))
			fastB.Likely = BranchUnlikely
			rel, keep := f.NewBlock(BlockPlain), f.NewBlock(BlockPlain)
			rel.Pos, keep.Pos = pos, pos
			fastB.AddEdgeTo(rel)
			fastB.AddEdgeTo(keep)
			_, rmem, _ := p.callAt(rel, pos, release, []*Value{ptr, f.ConstBool(ts.Bool, true)}, nil, st)
			rel.AddEdgeTo(cont)
			keep.AddEdgeTo(cont)
			mems = []*Value{smem, m, rmem, st}
		}
		// cont's predecessors, in order: for inc fastB, slow, skipB; for
		// dec slow, skipB, rel, keep.
		mphi := cont.NewValue0(pos, OpPhi, types.TypeMem)
		for _, e := range cont.Preds {
			var mem *Value
			switch e.b {
			case slow:
				mem = smem
			case skipB:
				mem = m
			default:
				mem = mems[0]
				if !inc {
					if e.b.Preds[0].b == fastB && e.b == fastB.Succs[0].b {
						mem = mems[2] // rel
					} else {
						mem = mems[3] // keep
					}
				}
			}
			mphi.AddArg(mem)
		}
		for _, u := range uses {
			if u.v == nil {
				continue
			}
			u.v.SetArg(u.i, mphi)
		}
		// The call is gone: its memory result becomes the merged memory.
		for i, v := range b.Values {
			if v == call {
				b.Values = append(b.Values[:i], b.Values[i+1:]...)
				break
			}
		}
		for i, v := range b.Values {
			if v == mo {
				b.Values = append(b.Values[:i], b.Values[i+1:]...)
				break
			}
		}
		mo.resetArgs()
		mo.Op = OpCopy
		mo.AuxInt = 0
		mo.Block = cont
		mo.AddArg(mphi)
		cont.Values = append(cont.Values, mo)
		call.resetArgs()
		call.Op = OpInvalid
		call.Aux = nil
		f.freeValue(call)
	}
	f.invalidateCFG()
}

// chainAfter reports whether memory op y comes after x in their block's
// memory chain.
func (p *fgoRCPass) chainAfter(next map[*Value]*Value, x, y *Value) bool {
	for v := next[x]; v != nil; {
		if v == y {
			return true
		}
		if fgoIsCall(v) {
			v = next[p.memOfCall(v)]
			continue
		}
		v = next[v]
	}
	return false
}

func (p *fgoRCPass) memOfCall(call *Value) *Value {
	for _, v := range call.Block.Values {
		if v.Op == OpSelectN && v.Args[0] == call && v.Type.IsMemory() {
			return v
		}
	}
	return nil
}

// fgoAddrBaseOrNil is fgoAddrBase's base alone.
func fgoAddrBaseOrNil(v *Value) *Value {
	b, _ := fgoAddrBase(v)
	return b
}

// fgoFillCall reports whether call is one the compiler emits to fill in
// part of an object (copying a value into a field) or to allocate one.
func fgoFillCall(call *Value) bool {
	if call.Op != OpStaticLECall {
		return false
	}
	switch call.Aux.(*AuxCall).Fn.Name {
	case "runtime.typedmemmove", "runtime.wbMove", "runtime.memmove", "runtime.typedmemclr", "runtime.memclrNoHeapPointers",
		"runtime.fgoStrSet", "runtime.fgoSliceSet", "runtime.fgoPtrsPut", "runtime.fgoStrPut", "runtime.fgoSlicePut",
		"runtime.fgoRCNew", "runtime.fgoRCNewMixed":
		return true
	}
	return false
}

// fgoFieldAt returns the field of struct t at offset off.
func fgoFieldAt(t *types.Type, off int64) *types.Field {
	for _, fd := range t.Fields() {
		if fd.Offset == off {
			return fd
		}
	}
	return nil
}

// hoistImmutable moves loads of fields that never change after
// construction as early as they can go: into a dominating block that
// already loads from the same object (so it is known to be a valid,
// non-nil object there), and then to the start of their block. The value
// read cannot differ, and the object then needs to stay alive only up to
// that earlier point, so it is released (and its memory offered for
// reuse) before calls that would otherwise see it still shared.
func (p *fgoRCPass) hoistImmutable() {
	sdom := p.f.Sdom()
	type move struct {
		v   *Value
		i   int
		dst *Block
		mem *Value
	}
	derefs := map[*Value][]*Value{} // object -> loads from it
	var loads []*Value
	for _, b := range p.f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpLoad {
				continue
			}
			base, off := fgoAddrBase(v.Args[0])
			if base == nil || off < 0 {
				continue
			}
			derefs[base] = append(derefs[base], v)
			if fgoImmutableAt(base.Type.Elem(), off, v.Type) {
				loads = append(loads, v)
			}
		}
	}
	var moves []move
	for _, L := range loads {
		base, off := fgoAddrBase(L.Args[0])
		var best *Value
		for _, d := range derefs[base] {
			if d.Block != L.Block && sdom.IsAncestorEq(d.Block, L.Block) && (best == nil || sdom.IsAncestorEq(d.Block, best.Block)) {
				best = d
			}
		}
		if best == nil {
			continue
		}
		for i, v := range L.Block.Values {
			if v == L {
				addr := best.Block.NewValue1I(L.Pos, OpOffPtr, L.Args[0].Type, off, base)
				L.SetArg(0, addr)
				moves = append(moves, move{L, i, best.Block, best.MemoryArg()})
				break
			}
		}
	}
	// Apply moves from the highest index down, as moveTo swaps in the last value.
	slices.SortFunc(moves, func(x, y move) int { return y.i - x.i })
	for _, m := range moves {
		for i, v := range m.v.Block.Values {
			if v == m.v {
				m.v.moveTo(m.dst, i)
				break
			}
		}
		m.v.SetArg(1, m.mem)
	}
	// Within a block, read as early as the first load from the same
	// object, which has already shown it is non-nil there.
	depth := func(m *Value, b *Block) int {
		n := 0
		for m.Block == b && m.Op != OpPhi && m.Op != OpInitMem {
			in := fgoMemIn(fgoHolder(m))
			if in == nil {
				break
			}
			m = in
			n++
		}
		return n
	}
	for _, L := range loads {
		base, off := fgoAddrBase(L.Args[0])
		b := L.Block
		var first *Value
		for _, d := range derefs[base] {
			if d != L && d.Block == b && depth(d.MemoryArg(), b) < depth(L.MemoryArg(), b) &&
				(first == nil || depth(d.MemoryArg(), b) < depth(first.MemoryArg(), b)) {
				first = d
			}
		}
		if first == nil {
			continue
		}
		L.SetArg(0, b.NewValue1I(L.Pos, OpOffPtr, L.Args[0].Type, off, base))
		L.SetArg(1, first.MemoryArg())
	}
	// Loads hoisted out of different branches now read the same field
	// under the same memory: keep one, so the field is counted once rather
	// than once per branch on every path.
	type key struct {
		base, mem *Value
		off       int64
		t         *types.Type
	}
	seen := map[key]*Value{}
	for _, L := range loads {
		base, off := fgoAddrBase(L.Args[0])
		k := key{base, L.MemoryArg(), off, L.Type}
		if first := seen[k]; first != nil && first.Block == L.Block && types.Identical(first.Type, L.Type) {
			L.copyOf(first)
			continue
		}
		seen[k] = L
	}
}

// fgoImmutableAt reports whether the field of struct type t at offset off,
// loaded as type lt, is never reassigned after construction.
func fgoImmutableAt(t *types.Type, off int64, lt *types.Type) bool {
	for _, fld := range t.Fields() {
		if fld.Offset == off && types.Identical(fld.Type, lt) {
			return types.FgoFieldImmutable(t, fld)
		}
	}
	return false
}

// hoistLoads lets loads (and nil checks) from an existing nogc object read
// the memory from before allocations of other nogc objects and the stores
// initializing them, which cannot alias it. Composite literals allocate
// before evaluating their fields, so this is what lets the object a field
// was read from die before the allocation, and hand its memory over.
func (p *fgoRCPass) hoistLoads() {
	for _, b := range p.f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpLoad && v.Op != OpNilCheck {
				continue
			}
			base, _ := fgoAddrBase(v.Args[0])
			if base == nil || base.Block == b && base.Op != OpPhi && base.Op != OpArg {
				continue
			}
			m := v.MemoryArg()
			for m.Block == b {
				if next := p.skippable(m, base); next != nil {
					m = next
					continue
				}
				break
			}
			v.SetArg(len(v.Args)-1, m)
		}
	}
}

// skippable returns the memory before m if m cannot change base's object.
func (p *fgoRCPass) skippable(m, base *Value) *Value {
	switch m.Op {
	case OpCopy:
		return m.Args[0]
	case OpStore, OpZero:
		b2, _ := fgoAddrBase(m.Args[0])
		if b2 != nil && b2 != base && p.isAlloc(b2) {
			return m.MemoryArg()
		}
	case OpSelectN:
		if c := m.Args[0]; fgoIsCall(c) && p.isNew(c.Aux.(*AuxCall).Fn) {
			return c.MemoryArg()
		}
	}
	return nil
}

func (p *fgoRCPass) isAlloc(v *Value) bool {
	return v.Op == OpSelectN && fgoIsCall(v.Args[0]) && p.isNew(v.Args[0].Aux.(*AuxCall).Fn)
}

func fgoIsCall(v *Value) bool {
	switch v.Op {
	case OpStaticLECall, OpInterLECall, OpClosureLECall, OpTailLECall:
		return true
	}
	return false
}

// fgoHolder returns the value whose last argument is m's input memory.
func fgoHolder(m *Value) *Value {
	if m.Op == OpSelectN || m.Op == OpSelect1 {
		return m.Args[0]
	}
	return m
}

func fgoMemIn(h *Value) *Value {
	if len(h.Args) == 0 {
		return nil
	}
	a := h.Args[len(h.Args)-1]
	if a.Type != types.TypeMem {
		return nil
	}
	return a
}

func (p *fgoRCPass) buildChains() {
	f := p.f
	p.chain = map[*Block][]*Value{}
	p.idx = map[*Value]int{}
	p.callMem = map[*Value]*Value{}
	p.callRes = map[*Value]*Value{}
	p.tupMem = map[*Value]*Value{}
	for _, b := range f.Blocks {
		var mems []*Value
		in := map[*Value]bool{}
		for _, v := range b.Values {
			switch {
			case v.Op == OpInitMem:
				p.initMem = v
			case v.Type == types.TypeMem && v.Op != OpPhi:
				mems = append(mems, v)
				in[v] = true
				if v.Op == OpSelect1 {
					p.tupMem[v.Args[0]] = v
				}
			}
			if v.Op == OpSelectN && len(v.Args) == 1 && fgoIsCall(v.Args[0]) {
				if v.Type == types.TypeMem {
					p.callMem[v.Args[0]] = v
				} else if v.Type.FgoIsRCPtr() {
					p.callRes[v.Args[0]] = v
				}
			}
		}
		if len(mems) == 0 {
			continue
		}
		next := map[*Value]*Value{}
		var first *Value
		for _, m := range mems {
			src := fgoMemIn(fgoHolder(m))
			if src != nil && in[src] {
				if next[src] != nil {
					f.Fatalf("fgo rc: memory forks at %v in %v:\n%s", src, b, fgoDump(b))
				}
				next[src] = m
				continue
			}
			if first != nil {
				f.Fatalf("fgo rc: two memory chains in %v (%v, %v):\n%s", b, first, m, fgoDump(b))
			}
			first = m
		}
		var ch []*Value
		for m := first; m != nil; m = next[m] {
			ch = append(ch, m)
			p.idx[m] = len(ch)
		}
		if len(ch) != len(mems) {
			f.Fatalf("fgo rc: memory chain in %v is not linear", b)
		}
		p.chain[b] = ch
	}
	if p.initMem == nil {
		f.Fatalf("fgo rc: no initial memory")
	}
}

// memPos is the position of a reader of memory m in block b.
func (p *fgoRCPass) memPos(b *Block, m *Value) int {
	if m != nil && m.Block == b {
		return 2*p.idx[m] + 1
	}
	return 1
}

// opPos is the position of a memory-producing value (or call) v.
func (p *fgoRCPass) opPos(v *Value) int {
	if fgoIsCall(v) {
		m := p.callMem[v]
		if m == nil {
			return fgoEnd
		}
		return 2 * p.idx[m]
	}
	if m := p.tupMem[v]; m != nil {
		return 2 * p.idx[m]
	}
	return 2 * p.idx[v]
}

func (p *fgoRCPass) endSlot(b *Block) int { return len(p.chain[b]) }

// slotAfter is the insertion slot right after position pos.
func (p *fgoRCPass) slotAfter(b *Block, pos int) int {
	if pos >= fgoEnd {
		return p.endSlot(b)
	}
	return pos / 2
}

// slotBefore is the insertion slot right before a consuming use at pos.
func (p *fgoRCPass) slotBefore(b *Block, pos int) int {
	if pos >= fgoEnd {
		return p.endSlot(b)
	}
	return pos/2 - 1
}

func fgoAlias(v *Value) *Value {
	for (v.Op == OpCopy || v.Op == OpNilCheck) && v.Type.FgoIsRCPtr() && v.Args[0].Type.FgoIsRCPtr() {
		v = v.Args[0]
	}
	return v
}

// fgoIsRef reports whether v is a nogc pointer the pass counts: a
// pointer to a struct that is not an address computed into some other
// object, a global or the stack.
func fgoIsRef(v *Value) bool {
	if !v.Type.FgoIsRCPtr() {
		return false
	}
	switch fgoAlias(v).Op {
	case OpOffPtr, OpAddPtr, OpPtrIndex, OpAddr, OpLocalAddr, OpSP, OpSB, OpGetClosurePtr:
		return false
	}
	return true
}

// fgoAddrBase returns the nogc pointer an address is computed from and
// the offset into its object (-1 if not constant).
func fgoAddrBase(v *Value) (*Value, int64) {
	off := int64(0)
	for {
		switch v.Op {
		case OpCopy, OpNilCheck:
			if fgoIsRef(v) {
				return fgoAlias(v), off
			}
			v = v.Args[0]
		case OpOffPtr:
			if off >= 0 {
				off += v.AuxInt
			}
			v = v.Args[0]
		case OpAddPtr, OpPtrIndex:
			off = -1
			v = v.Args[0]
		default:
			if fgoIsRef(v) {
				return v, off
			}
			return nil, 0
		}
	}
}

func (p *fgoRCPass) collectDefs() {
	p.defs = map[*Value]*fgoDef{}
	for _, b := range p.f.Blocks {
		for _, v := range b.Values {
			if !fgoIsRef(v) {
				continue
			}
			d := &fgoDef{v: v, block: b}
			switch v.Op {
			case OpCopy, OpNilCheck:
				if v.Args[0].Type.FgoIsRCPtr() {
					continue
				}
				p.external(d) // converted from a shape or other pointer type
			case OpConstNil:
				continue
			case OpIData, OpConvert:
				p.external(d) // out of an interface, or from unsafe.Pointer
			case OpSelectN:
				if !fgoIsCall(v.Args[0]) {
					p.external(d)
					break
				}
				m := p.callMem[v.Args[0]]
				if m == nil {
					continue // result of a call that never returns
				}
				d.kind = fgoOwned
				d.slot = p.idx[m]
				d.pos = 2 * p.idx[m]
				if !p.ownedResults(v.Args[0]) {
					// From code that does not count: take a reference
					// of our own (if it is a nogc object at all).
					p.reqs = append(p.reqs, &fgoReq{b: d.block, slot: d.slot, inc: true, v: v})
				}
			case OpPhi:
				d.kind = fgoOwned
			case OpArg:
				d.kind = fgoParam
				if fgoArgOwned(v) {
					d.kind = fgoOwned // slot 0 and position 0 of the entry block
				}
			case OpLoad:
				// classified once uses are known
			default:
				p.external(d)
			}
			p.defs[v] = d
			p.order = append(p.order, d)
		}
	}
}

// external makes d, a pointer taken from GC managed data (an interface,
// a conversion), a counted reference, if it is a nogc object at all: the
// GC keeps the object alive until then.
func (p *fgoRCPass) external(d *fgoDef) {
	d.kind = fgoOwned
	d.pos = p.valPos(d.v)
	d.slot = d.pos / 2
	p.reqs = append(p.reqs, &fgoReq{b: d.block, slot: d.slot, inc: true, v: d.v})
}

// valPos is the position in its block's memory chain from which v can
// be computed.
func (p *fgoRCPass) valPos(v *Value) int {
	switch {
	case v.Op == OpLoad:
		return p.memPos(v.Block, v.MemoryArg())
	case v.Op == OpSelectN && fgoIsCall(v.Args[0]):
		return p.opPos(v.Args[0])
	case v.Op == OpPhi, v.Op == OpArg:
		return 0
	}
	pos := 0
	if m := v.MemoryArg(); m != nil {
		pos = p.memPos(v.Block, m)
	}
	for _, a := range v.Args {
		if a.Block == v.Block && !a.Type.IsMemory() && a.Op != OpPhi {
			pos = max(pos, p.valPos(a))
		}
	}
	return pos
}

// escapeAt hands root's object to the GC (runtime.fgoRCHandoff) at slot
// of b, for a use that takes it where the counting pass cannot follow.
func (p *fgoRCPass) escapeAt(root *Value, b *Block, slot int) {
	if p.strict && p.cur != nil {
		p.refuse(root, p.cur)
	}
	p.reqs = append(p.reqs, &fgoReq{b: b, slot: slot, order: fgoOrderInc, escape: true, v: root})
}

// rcFieldAt reports whether address a is a nogc pointer field of a nogc
// object, which the counting pass tracks; anything else is GC managed.
func fgoRCFieldAt(a *Value) bool {
	if fgoTaggedAddr(a) != nil {
		return true
	}
	base, off := fgoAddrBase(a)
	if base == nil || off < 0 {
		return false
	}
	for _, f := range base.Type.Elem().Fields() {
		if f.Offset == off {
			return f.Type.FgoIsRCPtr()
		}
	}
	return false
}

// fgoTaggedAddr returns the field tagged `fgo:"nogc"` that address a
// points to, found from the struct (or array) pointers a is computed
// from, or nil. Such a field may be nested in fields and array elements
// of its object; a counted object counts it all the same.
func fgoTaggedAddr(a *Value) *types.Field {
	off := int64(0)
	for {
		if t := a.Type; t.IsPtr() && (t.Elem().IsStruct() || t.Elem().IsArray()) {
			if f := types.FgoTaggedAt(t.Elem(), off); f != nil {
				return f
			}
		}
		switch a.Op {
		case OpOffPtr:
			off += a.AuxInt
			a = a.Args[0]
		case OpCopy, OpNilCheck:
			a = a.Args[0]
		default:
			return nil
		}
	}
}

// markCall marks root's most recent use as a call argument.
func (p *fgoRCPass) markCall(root *Value) {
	if d := p.defs[root]; d != nil && len(d.uses) > 0 {
		d.uses[len(d.uses)-1].call = true
	}
}

func (p *fgoRCPass) addUse(root *Value, u fgoUse) {
	if d := p.defs[root]; d != nil {
		d.uses = append(d.uses, u)
	}
}

func (p *fgoRCPass) collectUses() {
	p.users = map[*Value][]*Value{}
	p.elems = map[*Value]fgoElem{}
	for _, b := range p.f.Blocks {
		for _, v := range b.Values {
			for _, a := range v.Args {
				p.users[a] = append(p.users[a], v)
			}
		}
	}
	for _, b := range p.f.Blocks {
		for _, u := range b.Values {
			switch u.Op {
			case OpCopy:
				if u.Type.FgoIsRCPtr() || !u.Args[0].Type.FgoIsRCPtr() {
					continue // followed through to their users
				}
				// A conversion to another pointer type: escapes (see use).
			case OpNilCheck, OpOffPtr, OpAddPtr, OpPtrIndex:
				continue // followed through to their users
			}
			for i, a := range u.Args {
				if a.Type.IsMemory() {
					continue
				}
				var root *Value
				direct := false
				if fgoIsRef(a) {
					root, direct = fgoAlias(a), true
				} else if a.Type.IsPtrShaped() {
					root, _ = fgoAddrBase(a)
				}
				if root == nil || root.Op == OpConstNil {
					continue
				}
				p.use(u, i, root, direct)
			}
		}
	}
}

func (p *fgoRCPass) use(u *Value, i int, root *Value, direct bool) {
	b := u.Block
	p.cur = u
	defer func() { p.cur = nil }()
	// handOff hands root's object to the GC right before (or, for a
	// value, right after) u, which takes the pointer, or one into the
	// object, where the pass cannot follow it.
	handOff := func(pos int, before bool) {
		slot := p.slotAfter(b, pos)
		if before {
			slot = p.slotBefore(b, pos)
		}
		p.escapeAt(root, b, slot)
		p.addUse(root, fgoUse{b: b, pos: pos, noreturn: pos >= fgoEnd && fgoIsCall(u)})
	}
	switch {
	case u.Op == OpLoad && i == 0:
		p.addUse(root, fgoUse{b: b, pos: p.memPos(b, u.MemoryArg())})
		switch t := u.Type; {
		case t.IsString() || t.IsSlice() && (types.FgoScalar(t.Elem()) || types.FgoPtrSliceType(t)):
			p.view(u, root)
		case (t.IsStruct() || t.IsArray()) && fgoHoldsRef(t):
			// A copy of counted references nothing counts.
			p.escapeAt(root, b, p.slotAfter(b, p.memPos(b, u.MemoryArg())))
		}
	case u.Op == OpStore && i == 0, u.Op == OpZero && i == 0, u.Op == OpMove && i <= 1:
		pos := p.opPos(u)
		var t *types.Type
		if u.Op == OpStore {
			t = u.Aux.(*types.Type)
		} else {
			t, _ = u.Aux.(*types.Type)
		}
		if t != nil && !t.FgoIsRCPtr() && (t.IsStruct() || t.IsArray()) && fgoHoldsRef(t) {
			// Copying counted references in or out wholesale: let the
			// GC have the object instead.
			handOff(pos, true)
			return
		}
		p.addUse(root, fgoUse{b: b, pos: pos})
	case u.Op == OpStore && i == 1:
		if !direct {
			handOff(p.opPos(u), true) // a pointer into the object, stored
			return
		}
		if !u.Aux.(*types.Type).FgoIsRCPtr() || !fgoRCFieldAt(u.Args[0]) {
			// Into GC managed memory: the GC holds the object.
			handOff(p.opPos(u), true)
			return
		}
		p.addUse(root, fgoUse{b: b, pos: p.opPos(u), consuming: true})
	case direct && (u.Op == OpConvert || u.Op == OpCopy) && p.onlyToNogcPkg(u):
		// Converted for fgo/nogc's own functions, which only look.
		p.addUse(root, fgoUse{b: b, pos: p.valPos(u)})
		for _, w := range p.users[u] {
			if fgoIsCall(w) {
				p.addUse(root, fgoUse{b: w.Block, pos: p.opPos(w)})
			}
		}
	case direct && (u.Op == OpIMake || u.Op == OpStructMake || u.Op == OpArrayMake1 ||
		u.Op == OpConvert || u.Op == OpCopy || u.Op == OpNilCheck && !u.Type.FgoIsRCPtr()):
		// Into an interface, an aggregate, or another pointer type: GC
		// managed from here on.
		handOff(p.valPos(u), false)
	case fgoIsCall(u) && fgoNogcPkgCallee(u):
		p.addUse(root, fgoUse{b: b, pos: p.opPos(u)})
	case fgoIsCall(u) && u.Op != OpTailLECall && (direct && !fgoArgRC(u, i) || p.handsOff(u, i, direct)):
		// A parameter of another type (a shape, any, unsafe.Pointer), or
		// of a function that does not count and may keep or change it.
		handOff(p.opPos(u), true)
	case fgoIsCall(u):
		pos := p.opPos(u)
		consuming := direct && fgoArgOwnedAt(u, i)
		defer func() { p.markCall(root) }()
		if u.Op == OpTailLECall && !consuming && direct && p.defs[root] != nil && p.defs[root].kind == fgoOwned {
			p.errorf(u.Pos, "cannot pass an owned nogc pointer to a tail call")
			return
		}
		p.addUse(root, fgoUse{b: b, pos: pos, noreturn: pos >= fgoEnd, consuming: consuming})
	case u.Op == OpMakeResult:
		if !direct || !p.nogcABI {
			// A pointer into the object, or a result for code that
			// does not count: the GC takes the object.
			handOff(fgoEnd, true)
			return
		}
		p.addUse(root, fgoUse{b: b, pos: fgoEnd, consuming: true})
	case u.Op == OpPhi:
		if !direct || !fgoIsRef(u) {
			p.escapeAt(root, b.Preds[i].b, p.endSlot(b.Preds[i].b))
			p.addUse(root, fgoUse{b: b.Preds[i].b, pos: fgoEnd})
			return
		}
		p.addUse(root, fgoUse{b: b.Preds[i].b, pos: fgoEnd, consuming: true})
	case u.Op == OpIsNonNil:
	case u.Op == OpEqPtr || u.Op == OpNeqPtr:
		if u.Args[1-i].Op == OpConstNil {
			return
		}
		p.addUse(root, fgoUse{b: b, pos: fgoEnd})
	case !direct && i == 0 && fgoScalarAtomic(u):
		// An atomic operation on a scalar field.
		p.addUse(root, fgoUse{b: b, pos: p.opPos(u)})
	default:
		if u.Type.IsMemory() || fgoIsCall(u) {
			handOff(p.opPos(u), true)
			return
		}
		handOff(p.valPos(u), false)
	}
}

// refuse reports, in strict nogc code, that use u would hand root's
// object to the garbage collector.
func (p *fgoRCPass) refuse(root, u *Value) {
	pos := u.Pos
	if !pos.IsKnown() {
		pos = root.Pos
	}
	if !p.fromNogc(root, map[*Value]bool{}) {
		return
	}
	why := fgoEscapeReason(u)
	if u.Op == OpStore || u.Op == OpMove {
		why = p.storeReason(u.Args[0])
	}
	p.refuseAt(pos, root, why)
}

// refuseAt reports at pos, in strict nogc code, that root's object
// escapes to the garbage collector for the reason why.
func (p *fgoRCPass) refuseAt(pos src.XPos, root *Value, why string) {
	pos = pos.WithNotStmt()
	if p.refused[pos] {
		return
	}
	if p.refused == nil {
		p.refused = map[src.XPos]bool{}
		p.names = map[*Value]string{}
		for _, n := range p.f.Names {
			for _, v := range p.f.NamedValues[*n] {
				if _, ok := p.names[v]; !ok && n.N != nil {
					p.names[v] = n.N.Sym().Name
				}
			}
		}
	}
	p.refused[pos] = true
	what := "a nogc object"
	if name := p.names[root]; name != "" && !strings.HasPrefix(name, "~") && !strings.Contains(name, ".") {
		what = name
	}
	p.errorf(pos, "strict: %s escapes to the garbage collector: %s", what, why)
}

// fromNogc reports whether v points to an object nogc code allocated or
// was given by other nogc code, rather than one it loaded from a global
// or got from code that is not nogc.
func (p *fgoRCPass) fromNogc(v *Value, seen map[*Value]bool) bool {
	if seen[v] {
		return false
	}
	seen[v] = true
	switch v.Op {
	case OpSelectN:
		if aux, ok := v.Args[0].Aux.(*AuxCall); ok && aux.Fn != nil {
			name := aux.Fn.Name
			return strings.HasPrefix(name, "runtime.fgoRCNew") || strings.Contains(name, ir.FgoNogcPrefix)
		}
	case OpArg:
		return p.nogcABI
	case OpPhi:
		for _, a := range v.Args {
			if p.fromNogc(a, seen) {
				return true
			}
		}
	case OpCopy, OpConvert, OpNilCheck:
		return p.fromNogc(v.Args[0], seen)
	case OpLoad:
		b := fgoAddrRoot(v.Args[0])
		return b != nil && b.Type.IsPtrShaped() && p.fromNogc(b, seen)
	}
	return false
}

// fgoAddrRoot follows address arithmetic on a back to the pointer or
// symbol address it starts from.
func fgoAddrRoot(a *Value) *Value {
	for {
		switch a.Op {
		case OpOffPtr, OpAddPtr, OpPtrIndex, OpCopy, OpNilCheck:
			a = a.Args[0]
		default:
			return a
		}
	}
}

// storeReason says where a store to addr puts a nogc object.
func (p *fgoRCPass) storeReason(addr *Value) string {
	return p.storeReasonAt(fgoAddrRoot(addr), map[*Value]bool{})
}

func (p *fgoRCPass) storeReasonAt(base *Value, seen map[*Value]bool) string {
	const generic = "stored in garbage collected memory"
	if seen[base] {
		return generic
	}
	seen[base] = true
	switch {
	case base.Op == OpPhi:
		for _, a := range base.Args {
			if why := p.storeReasonAt(fgoAddrRoot(a), seen); why != generic {
				return why
			}
		}
	case base.Op == OpAddr:
		if sym, ok := base.Aux.(*obj.LSym); ok {
			return "stored in global " + sym.Name[strings.LastIndex(sym.Name, ".")+1:]
		}
	case base.Op == OpSelectN && fgoIsCall(base.Args[0]):
		if aux, ok := base.Args[0].Aux.(*AuxCall); ok && aux.Fn != nil {
			switch {
			case strings.HasPrefix(aux.Fn.Name, "runtime.mapassign"):
				return "stored in a map"
			case aux.Fn.Name == "runtime.growslice":
				return "appended to a slice"
			}
		}
		if why := p.passedOn(base); why != "" {
			return why
		}
		return "stored in a garbage collected object"
	case base.Op == OpLocalAddr:
		if why := p.passedOn(base); why != "" {
			return why
		}
	}
	if base.Type.IsPtr() && base.Type.Elem().IsArray() {
		return "stored in a slice"
	}
	for base.Op == OpSlicePtr || base.Op == OpLoad {
		base = fgoAddrRoot(base.Args[0])
	}
	if sym, ok := base.Aux.(*obj.LSym); ok && base.Op == OpAddr {
		return "stored in memory reached from global " + sym.Name[strings.LastIndex(sym.Name, ".")+1:]
	}
	return "stored in garbage collected memory"
}

// passedOn says where memory at ptr, holding a nogc object, is taken:
// to a goroutine, a channel or a closure.
func (p *fgoRCPass) passedOn(ptr *Value) string {
	seen := map[*Value]bool{}
	work := []*Value{ptr}
	for len(work) > 0 {
		v := work[len(work)-1]
		work = work[:len(work)-1]
		if seen[v] {
			continue
		}
		seen[v] = true
		for _, w := range p.users[v] {
			switch {
			case w.Op == OpClosureLECall || w.Op == OpClosureCall:
				return "captured by a closure"
			case fgoIsCall(w):
				if aux, ok := w.Aux.(*AuxCall); ok && aux.Fn != nil {
					switch name := aux.Fn.Name; {
					case strings.HasPrefix(name, "runtime.newproc"):
						return "captured by a goroutine"
					case strings.HasPrefix(name, "runtime.chansend"), strings.HasPrefix(name, "runtime.selectnbsend"):
						return "sent on a channel"
					}
				}
			case w.Op == OpStore && w.Args[1] == v:
				// The closure's address, stored: follow where it goes.
				if b, _ := fgoAddrBase(w.Args[0]); b != nil {
					work = append(work, b)
				}
			case w.Op == OpOffPtr || w.Op == OpCopy || w.Op == OpConvert:
				work = append(work, w)
			}
		}
	}
	return ""
}

// fgoEscapeReason says where use u takes a nogc object that nogc code
// cannot follow.
func fgoEscapeReason(u *Value) string {
	switch u.Op {
	case OpStore, OpMove, OpZero:
		return "stored in garbage collected memory"
	case OpIMake:
		return "converted to an interface"
	case OpMakeResult:
		return "returned to code that is not nogc"
	case OpPhi:
		return "merged with a value nogc code does not count"
	case OpLoad:
		return "copied out of the object with counted references"
	case OpClosureLECall, OpClosureCall:
		return "passed to a func value"
	case OpInterLECall, OpInterCall:
		return "passed to an interface method"
	}
	if aux, ok := u.Aux.(*AuxCall); ok && aux.Fn != nil {
		name := aux.Fn.Name
		switch {
		case strings.HasPrefix(name, "runtime.newproc"):
			return "passed to a goroutine"
		case strings.HasPrefix(name, "runtime.chansend"):
			return "sent on a channel"
		case strings.HasPrefix(name, "runtime.mapassign"):
			return "stored in a map"
		case strings.HasPrefix(name, "runtime.growslice"):
			return "appended to a slice"
		}
		return "passed to " + name + ", which may keep it"
	}
	if fgoIsCall(u) {
		return "passed to code that may keep it"
	}
	return "used where nogc code cannot follow it"
}

// fgoNogcPkgCallee reports whether call calls a function of package
// fgo/nogc, which only inspects the objects it is given.
func fgoNogcPkgCallee(call *Value) bool {
	aux, ok := call.Aux.(*AuxCall)
	return ok && aux.Fn != nil && strings.HasPrefix(aux.Fn.Name, "fgo/nogc.")
}

// onlyToNogcPkg reports whether u, a pointer conversion, flows only into
// calls to fgo/nogc's functions.
func (p *fgoRCPass) onlyToNogcPkg(u *Value) bool {
	us := p.users[u]
	if len(us) == 0 {
		return false
	}
	for _, w := range us {
		if !fgoIsCall(w) || !fgoNogcPkgCallee(w) {
			return false
		}
	}
	return true
}

// fgoHoldsRef reports whether a value of aggregate type t holds nogc
// pointers directly (not through other pointers).
func fgoHoldsRef(t *types.Type) bool {
	switch {
	case t.FgoIsRCPtr():
		return true
	case t.IsArray():
		return t.NumElem() > 0 && fgoHoldsRef(t.Elem())
	case t.IsStruct():
		for _, f := range t.Fields() {
			if fgoHoldsRef(f.Type) || types.FgoPtrSliceType(f.Type) {
				return true
			}
		}
	}
	return false
}

// fgoScalarAtomic reports whether u is an atomic operation whose operands
// other than the address are not pointers.
func fgoScalarAtomic(u *Value) bool {
	if !strings.HasPrefix(u.Op.String(), "Atomic") {
		return false
	}
	for _, a := range u.Args[1:] {
		if a.Type.IsPtrShaped() {
			return false
		}
	}
	return true
}

// view handles string or slice L, loaded from a field of root's object.
// It points into a buffer the object owns, so the object must outlive
// every use of it. Uses the pass can follow (reading and writing
// elements, slicing, and calls, which escape analysis has already shown
// do not keep it) extend root's life over them. If L flows anywhere else (into memory, or a phi), it is
// copied onto the GC heap right after the load instead.
func (p *fgoRCPass) view(L, root *Value) {
	var uses []fgoUse
	seen := map[*Value]bool{L: true}
	work := []*Value{L}
	for len(work) > 0 {
		w := work[len(work)-1]
		work = work[:len(work)-1]
		for _, u := range p.users[w] {
			switch {
			case u.Op == OpStringPtr, u.Op == OpStringMake, u.Op == OpSliceMake, u.Op == OpCopy,
				u.Op == OpSlicePtr, u.Op == OpSlicePtrUnchecked,
				u.Op == OpOffPtr, u.Op == OpAddPtr, u.Op == OpPtrIndex:
				if !seen[u] {
					seen[u] = true
					work = append(work, u)
				}
			case u.Op == OpStringLen, u.Op == OpSliceLen, u.Op == OpSliceCap, u.Op == OpIsNonNil,
				u.Op == OpEqPtr, u.Op == OpNeqPtr:
			case u.Op == OpLoad && u.Args[0] == w:
				uses = append(uses, fgoUse{b: u.Block, pos: p.memPos(u.Block, u.MemoryArg())})
				if u.Type.FgoIsRCPtr() {
					p.elems[u] = fgoElem{root, p.immutableSlice(L, root)}
				}
			case (u.Op == OpStore || u.Op == OpZero || u.Op == OpMove) && u.Args[0] == w,
				u.Op == OpMove && u.Args[1] == w:
				uses = append(uses, fgoUse{b: u.Block, pos: p.opPos(u)})
			case w == L && fgoIsCall(u) && p.isSet(u.Aux.(*AuxCall).Fn) && u.Args[1] == L && u.Block == L.Block:
				// Stored into another field: copy it right here, while
				// the object is surely alive, and let the store take
				// the copy. The object can then die (and give its
				// memory to the allocation the store may be filling)
				// before everything up to the store. (Only in the
				// load's block: a path that left it without reaching
				// the store would leak the copy.)
				p.dup(L, u)
			case fgoIsCall(u):
				pos := p.opPos(u)
				uses = append(uses, fgoUse{b: u.Block, pos: pos, noreturn: pos >= fgoEnd})
			case types.FgoPtrSliceType(L.Type) && (u.Op == OpStore || u.Op == OpZero || u.Op == OpMove) && u.Args[0] == w:
				p.errorf(u.Pos, "cannot assign to an element of a slice of nogc pointers read from a field (rebuild the slice instead)")
				return
			default:
				pos := p.slotAfter(L.Block, p.memPos(L.Block, L.MemoryArg()))
				p.reqs = append(p.reqs, &fgoReq{b: L.Block, slot: pos, order: fgoOrderClone, clone: true, v: L})
				return
			}
		}
	}
	for _, u := range uses {
		p.addUse(root, u)
	}
}

// discardedResults recreates the values of nogc pointer results that a
// caller ignores (dead code elimination removed them), so the counting
// pass releases them like any other owned result.
func (p *fgoRCPass) discardedResults() {
	for _, b := range p.f.Blocks {
		for _, call := range slices.Clone(b.Values) {
			if !fgoIsCall(call) {
				continue
			}
			aux := call.Aux.(*AuxCall)
			have := map[int64]bool{}
			for _, v := range b.Values {
				if v.Op == OpSelectN && v.Args[0] == call {
					have[v.AuxInt] = true
				}
			}
			for i := int64(0); i < aux.NResults(); i++ {
				if t := aux.TypeOfResult(i); t.FgoIsRCPtr() && !have[i] {
					b.NewValue1I(call.Pos, OpSelectN, t, i, call)
				}
			}
		}
	}
}

// takeElems moves elements out of slices where Perceus would otherwise
// count them. When a function passes t.Kids[j] to an owned parameter of
// call C, with t owned, the element is counted and t's slice keeps its
// own reference, so the callee never sees it unique and can't reuse its
// memory. If nothing reads slot j after C (the rebuild around C splices
// t.Kids[:j] and t.Kids[j+1:]), C can take fgoPtrsTake's result instead:
// it moves the element out of the slice when t holds the only reference
// to its object, and counts it otherwise.
func (p *fgoRCPass) takeElems() {
	var users map[*Value][]*Value
	for _, b := range p.f.Blocks {
		for _, C := range b.Values {
			if C.Op != OpStaticLECall {
				continue
			}
			for i, E := range C.Args {
				if E.Op != OpLoad || !E.Type.FgoIsRCPtr() || !fgoArgOwnedAt(C, i) {
					continue
				}
				P0, J, jc := fgoElemAddr(E.Args[0])
				if P0 == nil || P0.Op != OpSlicePtr || P0.Args[0].Op != OpLoad || !types.FgoPtrSliceType(P0.Args[0].Type) {
					continue
				}
				root, off := fgoAddrBase(P0.Args[0].Args[0])
				if root == nil || off < 0 || !(root.Op == OpArg && fgoArgOwned(root) || root.Op == OpSelectN && fgoIsCall(root.Args[0])) {
					continue
				}
				if users == nil {
					users = map[*Value][]*Value{}
					for _, b := range p.f.Blocks {
						for _, v := range b.Values {
							for _, a := range v.Args {
								users[a] = append(users[a], v)
							}
						}
					}
				}
				if !p.takeable(E, root, off, J, jc, C, users) {
					continue
				}
				_, mem, res := p.callAt(b, C.Pos, p.syms.take, []*Value{root, E.Args[0]}, []*types.Type{E.Type}, C.MemoryArg())
				C.SetArg(i, res[0])
				C.SetArg(len(C.Args)-1, mem)
				users = nil
			}
		}
	}
}

// takeable reports whether element E (slot J of slice field off of root's
// object), passed to call C, can be moved out of the slice just before C:
// nothing may read the slot, or use E, after that.
func (p *fgoRCPass) takeable(E, root *Value, off int64, J *Value, jc int64, C *Value, users map[*Value][]*Value) bool {
	// Reads that happen before C, or that cannot follow it, are all
	// right. C must not be in a loop: a later iteration could read the
	// slot again.
	reach := map[*Block]bool{}
	work := []*Block{C.Block}
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		for _, e := range b.Succs {
			if !reach[e.b] {
				reach[e.b] = true
				work = append(work, e.b)
			}
		}
	}
	if reach[C.Block] {
		return false
	}
	reach[C.Block] = true
	before := fgoMemBefore(C.MemoryArg())
	early := func(v *Value) bool { return before[v.MemoryArg()] || !reach[v.Block] }

	// E itself is handed to C: nothing after may use it.
	ev := []*Value{E}
	for k := 0; k < len(ev); k++ {
		for _, u := range users[ev[k]] {
			switch {
			case u == C:
			case u.Op == OpNilCheck, u.Op == OpCopy, u.Op == OpOffPtr:
				ev = append(ev, u)
			case u.Op == OpLoad, fgoIsCall(u):
				if !early(u) {
					return false
				}
			case u.Op == OpIsNonNil, u.Op == OpEqPtr, u.Op == OpNeqPtr:
			default:
				return false
			}
		}
	}

	// root must not reach a call after C (which could read the slot),
	// nor live on beyond this function.
	aliases := []*Value{root}
	for k := 0; k < len(aliases); k++ {
		for _, u := range users[aliases[k]] {
			switch {
			case (u.Op == OpNilCheck || u.Op == OpCopy) && u.Type.FgoIsRCPtr():
				aliases = append(aliases, u)
			case u.Op == OpOffPtr:
				aliases = append(aliases, u)
			case u.Op == OpLoad, u.Op == OpStore && u.Args[0] == aliases[k]:
				if u.Op == OpLoad && u.Type.IsSlice() && u.Args[0].Op == OpOffPtr && u.Args[0].AuxInt == off {
					if !p.spliceOnly(u, E, J, jc, early, users) {
						return false
					}
				}
			case u.Op == OpIsNonNil, u.Op == OpEqPtr, u.Op == OpNeqPtr:
			case u.Op == OpPhi, u.Op == OpMakeResult:
				if reach[u.Block] {
					return false
				}
			case fgoIsCall(u):
				for i, a := range u.Args {
					if a == aliases[k] && fgoArgOwnedAt(u, i) && reach[u.Block] {
						return false // root handed on
					}
				}
				if !early(u) {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// spliceOnly reports whether slice V, loaded from the same field as
// element E (at index J), reads the field's elements only before E's
// load or only around index J.
func (p *fgoRCPass) spliceOnly(V, E, J *Value, jc int64, early func(*Value) bool, users map[*Value][]*Value) bool {
	// allEarly reports whether every read through v (a slice, pointer
	// into the slice, or value loaded from it) happens early.
	var allEarly func(v *Value) bool
	allEarly = func(v *Value) bool {
		for _, u := range users[v] {
			switch {
			case u.Op == OpLoad || fgoIsCall(u):
				if u != E && !early(u) {
					return false
				}
			case u.Op == OpSliceLen, u.Op == OpSliceCap, u.Op == OpIsNonNil:
			case u.Op == OpSlicePtr, u.Op == OpSliceMake, u.Op == OpAddPtr, u.Op == OpPtrIndex, u.Op == OpCopy:
				if !allEarly(u) {
					return false
				}
			default:
				if !early(u) && u.Block != nil && u.Op != OpIsSliceInBounds && u.Op != OpIsInBounds {
					return false
				}
			}
		}
		return true
	}
	// splice: a slice made around J, read only by appends and len.
	splice := func(sm *Value) bool {
		for _, u := range users[sm] {
			switch {
			case u.Op == OpSliceLen, u.Op == OpSliceCap:
			case fgoIsCall(u) && u.Aux.(*AuxCall).Fn == p.syms.appendN:
			case fgoIsCall(u) && early(u):
			default:
				return false
			}
		}
		return true
	}
	for _, u := range users[V] {
		switch u.Op {
		case OpSliceLen, OpSliceCap:
			continue
		case OpCopy:
			// The pass may replace a reload of the field by the first load.
			if !p.spliceOnly(u, E, J, jc, early, users) {
				return false
			}
			continue
		case OpSlicePtr:
		default:
			if !allEarly(u) && !(fgoIsCall(u) || u.Op == OpLoad) || (fgoIsCall(u) || u.Op == OpLoad) && !early(u) {
				return false
			}
			continue
		}
		for _, w := range users[u] {
			switch {
			case w.Op == OpSliceMake && w.Args[0] == u && (J != nil && w.Args[1] == J || J == nil && w.Args[1].Op == OpConst64 && w.Args[1].AuxInt <= jc) && splice(w):
			case (w.Op == OpAddPtr || w.Op == OpPtrIndex) && fgoAfter(w, J, jc) && func() bool {
				for _, x := range users[w] {
					if x.Op != OpSliceMake || x.Args[0] != w || !splice(x) {
						return false
					}
				}
				return true
			}():
			case w.Op == OpLoad:
				if w != E && !early(w) {
					return false
				}
			default:
				if !allEarly(w) {
					return false
				}
			}
		}
	}
	return true
}

// fgoElemAddr matches the address of a slice element of pointer size,
// P + J*size or P + c*size, and returns P with J (nil for a constant
// index) and c.
func fgoElemAddr(a *Value) (P, J *Value, c int64) {
	switch a.Op {
	case OpSlicePtr:
		return a, nil, 0
	case OpPtrIndex:
		if a.Args[1].Op == OpConst64 || a.Args[1].Op == OpConst32 {
			return a.Args[0], nil, a.Args[1].AuxInt
		}
		return a.Args[0], a.Args[1], 0
	case OpOffPtr:
		if a.Args[0].Op == OpSlicePtr && a.AuxInt%int64(types.PtrSize) == 0 {
			return a.Args[0], nil, a.AuxInt / int64(types.PtrSize)
		}
	case OpAddPtr:
		x := a.Args[1]
		if x.Op == OpConst64 || x.Op == OpConst32 {
			if x.AuxInt%int64(types.PtrSize) == 0 {
				return a.Args[0], nil, x.AuxInt / int64(types.PtrSize)
			}
			return nil, nil, 0
		}
		if (x.Op == OpLsh64x64 || x.Op == OpLsh32x32) && x.Args[1].Op == OpConst64 {
			return a.Args[0], x.Args[0], 0
		}
		if x.Op == OpMul64 || x.Op == OpMul32 {
			if x.Args[1].Op == OpConst64 || x.Args[1].Op == OpConst32 {
				return a.Args[0], x.Args[0], 0
			}
			return a.Args[0], x.Args[1], 0
		}
	}
	return nil, nil, 0
}

// fgoAfter reports whether address w is P + i*size for an index i past
// element (J, c) of fgoElemAddr: J+k with k ≥ 1, or a constant above c.
// The offset may be masked, as slicing does for an empty result.
func fgoAfter(w, J *Value, c int64) bool {
	if w.Op != OpAddPtr {
		return false
	}
	x := w.Args[1]
	if x.Op == OpAnd64 || x.Op == OpAnd32 {
		if x.Args[0].Op == OpSlicemask {
			x = x.Args[1]
		} else {
			x = x.Args[0]
		}
	}
	if x.Op == OpConst64 || x.Op == OpConst32 {
		return J == nil && x.AuxInt%int64(types.PtrSize) == 0 && x.AuxInt/int64(types.PtrSize) > c
	}
	if J == nil || (x.Op != OpLsh64x64 && x.Op != OpLsh32x32 && x.Op != OpMul64 && x.Op != OpMul32) || len(x.Args) != 2 {
		return false
	}
	i := x.Args[0]
	if i.Op == OpConst64 || i.Op == OpConst32 {
		i = x.Args[1]
	}
	if i.Op != OpAdd64 && i.Op != OpAdd32 {
		return false
	}
	a, k := i.Args[0], i.Args[1]
	if a.Op == OpConst64 || a.Op == OpConst32 {
		a, k = k, a
	}
	return a == J && (k.Op == OpConst64 || k.Op == OpConst32) && k.AuxInt >= 1
}

// fgoMemBefore returns the memory states at or before mem.
func fgoMemBefore(mem *Value) map[*Value]bool {
	seen := map[*Value]bool{}
	work := []*Value{mem}
	for len(work) > 0 {
		m := work[len(work)-1]
		work = work[:len(work)-1]
		if m == nil || seen[m] {
			continue
		}
		seen[m] = true
		switch {
		case m.Op == OpPhi:
			work = append(work, m.Args...)
		case m.Op == OpSelectN:
			work = append(work, m.Args[0].MemoryArg())
		case m.Op == OpInitMem:
		default:
			work = append(work, m.MemoryArg())
		}
	}
	return seen
}

// immutableSlice reports whether slice L, loaded from a field of root's
// object, comes from a field that is never reassigned.
func (p *fgoRCPass) immutableSlice(L, root *Value) bool {
	base, off := fgoAddrBase(L.Args[0])
	if base != root || off < 0 {
		return false
	}
	t := root.Type.Elem()
	for _, fld := range t.Fields() {
		if fld.Offset == off {
			return types.FgoFieldImmutable(t, fld)
		}
	}
	return false
}

// isNew reports whether fn allocates a fresh, zeroed nogc object.
func (p *fgoRCPass) isNew(fn *obj.LSym) bool {
	return fn == p.syms.new || fn == p.syms.newMixed
}

func (p *fgoRCPass) isSet(fn *obj.LSym) bool {
	return fn == p.syms.strSet || fn == p.syms.sliceSet
}

// dup copies string L, loaded from a nogc object, right after the load,
// for fgoStrSet call set to take instead of copying it later. The copy
// is kept in a shadow slot until set takes it, so a panic in between
// frees it.
func (p *fgoRCPass) dup(L, set *Value) {
	b := L.Block
	slot := p.slotAfter(b, p.memPos(b, L.MemoryArg()))
	r := &fgoReq{b: b, slot: slot, order: fgoOrderClone, clone: true, dup: set, v: L}
	n := p.f.NewLocal(L.Pos, p.f.Config.Types.BytePtr)
	p.bufs = append(p.bufs, n)
	p.reqs = append(p.reqs, r,
		&fgoReq{b: b, slot: slot, order: fgoOrderSet, shadow: n, tokOf: r},
		&fgoReq{b: set.Block, slot: p.slotBefore(set.Block, p.opPos(set)), order: fgoOrderClear, shadow: n})
}

// classifyLoads decides, for each nogc pointer loaded from a field,
// whether it must be counted.
func (p *fgoRCPass) classifyLoads() {
	done := map[*fgoDef]bool{}
	var classify func(d *fgoDef)
	classify = func(d *fgoDef) {
		if d.v.Op != OpLoad || done[d] {
			return
		}
		done[d] = true
		L := d.v
		pos := p.memPos(L.Block, L.MemoryArg())
		elided := true
		for _, u := range d.uses {
			if u.consuming || u.b != L.Block || u.pos != pos {
				elided = false
				break
			}
		}
		if elided {
			d.kind = fgoElided
			return
		}
		base, off := fgoAddrBase(L.Args[0])
		elem, isElem := p.elems[L]
		if isElem {
			base = elem.root
		}
		var bd *fgoDef
		if base != nil {
			bd = p.defs[base]
		}
		if bd != nil && (off >= 0 || isElem) {
			classify(bd)
			owner := bd
			if bd.kind == fgoDerived {
				owner = bd.owner
			}
			local := true
			for _, u := range d.uses {
				local = local && !u.consuming && u.b == L.Block
			}
			// Borrowing from an owned value keeps the owner alive over
			// the load's uses. Do it only for uses close by (not consumed,
			// same block); otherwise count the load, so the owner can die
			// early and give its memory to a following allocation.
			immutable := isElem && elem.immutable || !isElem && fgoImmutableField(base.Type.Elem(), off)
			if owner != nil && (owner.kind == fgoParam || owner.kind == fgoOwned && local) && immutable {
				d.kind = fgoDerived
				d.owner = owner
				return
			}
		}
		d.kind = fgoOwned
		d.slot = pos / 2
		d.pos = pos
		p.reqs = append(p.reqs, &fgoReq{b: L.Block, slot: d.slot, inc: true, v: L})
	}
	for _, d := range p.order {
		classify(d)
	}
	for _, d := range p.order {
		if d.kind == fgoDerived && d.owner.kind == fgoOwned {
			d.owner.extra = append(d.owner.extra, d.uses...)
		}
	}
}

func fgoImmutableField(t *types.Type, off int64) bool {
	for _, fld := range t.Fields() {
		if fld.Offset == off && fld.Type.FgoIsRCPtr() {
			return types.FgoFieldImmutable(t, fld)
		}
	}
	return false
}

// planStores releases the value a nogc pointer field held before each
// store, unless the store initializes a freshly allocated object.
func (p *fgoRCPass) planStores() {
	// lowerStores has turned every store that replaces a reference into
	// an fgoRCStore call, which releases the old one.
}

func (p *fgoRCPass) freshStore(st *Value) bool {
	base, off := fgoAddrBase(st.Args[0])
	if base == nil || off < 0 || base.Op != OpSelectN {
		return false
	}
	call := base.Args[0]
	if !fgoIsCall(call) || !p.isNew(call.Aux.(*AuxCall).Fn) {
		return false
	}
	alloc := p.callMem[call]
	for m := st.MemoryArg(); ; m = m.MemoryArg() {
		if m == alloc {
			return true
		}
		if m.Op == OpCopy && m.Block == st.Block {
			continue // the stripped zeroing of this object
		}
		if m.Op != OpStore || m.Block != st.Block {
			return false
		}
		b2, off2 := fgoAddrBase(m.Args[0])
		if b2 != base || off2 < 0 {
			return false
		}
		if off2 == off {
			return m.Args[1].Op == OpConstNil // zero-initialized just before
		}
	}
}

func (p *fgoRCPass) planDefs() {
	for _, d := range p.order {
		switch d.kind {
		case fgoOwned:
			p.planOwned(d)
		case fgoParam, fgoDerived:
			for _, u := range d.uses {
				if u.consuming {
					p.reqs = append(p.reqs, &fgoReq{b: u.b, slot: p.slotBefore(u.b, u.pos), inc: true, v: d.v})
				}
			}
		}
	}
}

func (p *fgoRCPass) planOwned(d *fgoDef) {
	if d.v.Op == OpSelectN && p.callMem[d.v.Args[0]] == nil {
		return
	}
	p.shadowSet(d)
	uses := append(slices.Clip(d.uses), d.extra...)
	byBlock := map[*Block][]int{}
	for i, u := range uses {
		if u.b == d.block && u.pos < d.pos {
			p.f.Fatalf("fgo rc: use of %v before its definition", d.v)
		}
		byBlock[u.b] = append(byBlock[u.b], i)
	}
	liveIn := map[*Block]bool{}
	liveOut := map[*Block]bool{}
	var work []*Block
	for b := range byBlock {
		if b != d.block && !liveIn[b] {
			liveIn[b] = true
			work = append(work, b)
		}
	}
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		for _, e := range b.Preds {
			liveOut[e.b] = true
			if e.b != d.block && !liveIn[e.b] {
				liveIn[e.b] = true
				work = append(work, e.b)
			}
		}
	}

	blocks := []*Block{d.block}
	for b := range liveIn {
		blocks = append(blocks, b)
	}
	slices.SortFunc(blocks, func(x, y *Block) int { return int(x.ID - y.ID) })
	for _, b := range blocks {
		us := byBlock[b]
		slices.SortStableFunc(us, func(x, y int) int { return uses[x].pos - uses[y].pos })
		if liveOut[b] {
			for _, i := range us {
				if uses[i].consuming && i < len(d.uses) {
					p.incBefore(d.v, uses[i])
				}
			}
			for _, e := range b.Succs {
				s := e.b
				if liveIn[s] || s.Kind == BlockExit {
					continue
				}
				if len(s.Preds) != 1 {
					p.f.Fatalf("fgo rc: release on critical edge %v -> %v", b, s)
				}
				p.release(d, s, 0)
			}
			continue
		}
		if len(us) == 0 {
			if b.Kind != BlockExit {
				p.release(d, b, d.slot)
			}
			continue
		}
		last := uses[us[len(us)-1]].pos
		moved := false
		noreturn := false
		for j := len(us) - 1; j >= 0 && uses[us[j]].pos == last; j-- {
			u := &uses[us[j]]
			noreturn = noreturn || u.noreturn
			if u.consuming && !moved && us[j] < len(d.uses) {
				u.moved = true
				moved = true
				p.shadowClearAt(d, *u)
			}
		}
		for _, i := range us {
			if uses[i].consuming && !uses[i].moved && i < len(d.uses) {
				p.incBefore(d.v, uses[i])
			}
		}
		if !moved && !noreturn && b.Kind != BlockExit {
			p.release(d, b, p.slotAfter(b, last))
		}
	}
}

// release releases owned value d at slot in b, forgetting it first.
func (p *fgoRCPass) release(d *fgoDef, b *Block, slot int) {
	clear := &fgoReq{b: b, slot: slot, order: fgoOrderClear, shadow: p.shadowOf(d)}
	p.reqs = append(p.reqs, clear, &fgoReq{b: b, slot: slot, order: fgoOrderRelease, v: d.v, clear: clear})
}

// planSink moves each release that found no allocation to reuse its
// memory into the successors of its block, when that is sinkable, and
// tries again from there. Perceus pushes drops into branches the same
// way, so that each branch's own allocation can take the memory.
func (p *fgoRCPass) planSink() {
	for {
		sunk := false
		for _, r := range slices.Clone(p.reqs) {
			if r.inc || r.clone || r.escape || r.shadow != nil || r.reuse != nil || !p.sinkable(r.b, r.slot) {
				continue
			}
			if r.sunkTo == nil {
				r.sunkTo = map[*Block]bool{}
			}
			if r.sunkTo[r.b] {
				continue // around a loop with no calls in it
			}
			r.sunkTo[r.b] = true
			sunk = true
			first := true
			for _, e := range r.b.Succs {
				if e.b.Kind == BlockExit {
					continue
				}
				q, c := r, r.clear
				if !first {
					q = &fgoReq{order: r.order, v: r.v, sunkTo: maps.Clone(r.sunkTo)}
					if c != nil {
						c = &fgoReq{order: c.order, shadow: c.shadow}
						p.reqs = append(p.reqs, c)
					}
					q.clear = c
					p.reqs = append(p.reqs, q)
				}
				first = false
				q.b, q.slot = e.b, 0
				if c != nil {
					c.b, c.slot = e.b, 0
				}
			}
		}
		if !sunk {
			return
		}
		p.planReuse()
		p.planReuseAcross()
	}
}

// sinkable reports whether a release at slot in b can move into each of
// b's successors instead: b branches, nothing after slot calls out, and
// each successor is entered only from b.
func (p *fgoRCPass) sinkable(b *Block, slot int) bool {
	if b.Kind != BlockIf {
		return false
	}
	for _, e := range b.Succs {
		if len(e.b.Preds) != 1 {
			return false
		}
	}
	for _, m := range p.chain[b][slot:] {
		if fgoIsCall(m) || m.Op == OpSelectN && fgoIsCall(m.Args[0]) {
			return false
		}
	}
	return true
}

// planKeep folds the counting of loads of an object's nogc pointer
// fields into the releases of that object that follow them with nothing
// writing memory in between (in the same slot, or at the start of
// successors entered only from there): when a release frees the object,
// the loads take its references to those fields instead of counting
// them while it drops them. Every release the load reaches must do so.
func (p *fgoRCPass) planKeep() {
	type at struct {
		b    *Block
		slot int
		v    *Value
	}
	rels := map[at]*fgoReq{}
	for _, r := range p.reqs {
		if r.order == fgoOrderRelease && r.shadow == nil && !r.inc && !r.clone && !r.escape {
			rels[at{r.b, r.slot, fgoAlias(r.v)}] = r
		}
	}
	if len(rels) == 0 {
		return
	}
	// reach collects the releases of v that every path from slot in b
	// meets before anything writes memory or leaves the function.
	var reach func(b *Block, slot int, v *Value, depth int, out []*fgoReq) []*fgoReq
	reach = func(b *Block, slot int, v *Value, depth int, out []*fgoReq) []*fgoReq {
		if r := rels[at{b, slot, v}]; r != nil {
			return append(out, r)
		}
		if slot != p.endSlot(b) || depth == 0 || b.Kind != BlockIf && b.Kind != BlockPlain {
			return nil
		}
		for _, e := range b.Succs {
			if len(e.b.Preds) != 1 {
				return nil
			}
			if out = reach(e.b, 0, v, depth-1, out); out == nil {
				return nil
			}
		}
		return out
	}
	ptr := p.f.Config.PtrSize
	var fused []*fgoReq
	for _, r := range p.reqs {
		if !r.inc || r.order != fgoOrderInc || r.v.Op != OpLoad {
			continue
		}
		base, off := fgoAddrBase(r.v.Args[0])
		if base == nil || off < 0 || off%ptr != 0 || off/ptr >= int64(ptr*8) || !fgoRCPtrFieldAt(base.Type.Elem(), off, r.v.Type) {
			continue
		}
		bit := int64(1) << (off / ptr)
		to := reach(r.b, r.slot, base, 4, nil)
		if to == nil || slices.ContainsFunc(to, func(rel *fgoReq) bool { return rel.keepOf&bit != 0 }) {
			continue
		}
		for _, rel := range to {
			rel.keepOf |= bit
		}
		r.fused = true
		fused = append(fused, to...)
	}
	if len(fused) == 0 {
		return
	}
	// A release that keeps fields goes before the slot's other releases,
	// which may drop the loads themselves: the release has to see the
	// fields still counted if it does not free the object.
	slices.SortStableFunc(p.reqs, func(x, y *fgoReq) int {
		return fgoBool(y.keepOf != 0) - fgoBool(x.keepOf != 0)
	})
}

// fgoRCOnlyPtrs returns the words of struct t holding nogc pointers
// when they are all the references its objects hold (no strings,
// slices or GC values), and 0 otherwise.
func fgoRCOnlyPtrs(t *types.Type) int64 {
	if !t.IsStruct() {
		return 0
	}
	var m int64
	for _, fd := range t.Fields() {
		switch {
		case fd.Type.FgoIsRCPtr() && fd.Offset/int64(types.PtrSize) < int64(types.PtrSize*8):
			m |= 1 << (fd.Offset / int64(types.PtrSize))
		case fd.Type.HasPointers():
			return 0
		}
	}
	return m
}

// fullInits finds the allocations (fgoRCNew calls) followed directly
// by stores writing every field of the new object, with nothing reading
// it in between (other allocations and their stores may come between).
// Small fields left out (zero in a composite literal) get zero stores
// after the others, returned in zeros by the slot to insert them at.
func (p *fgoRCPass) fullInits() (inits map[*Value]bool, zeros map[*Block]map[int][]*Value) {
	inits = map[*Value]bool{}
	zeros = map[*Block]map[int][]*Value{}
	fresh := func(v *Value) bool {
		v = fgoAlias(v)
		return v.Op == OpSelectN && fgoIsCall(v.Args[0]) && p.callRes[v.Args[0]] == v && v.Args[0].Aux.(*AuxCall).Fn == p.syms.new
	}
	for call, res := range p.callRes {
		if call.Aux.(*AuxCall).Fn != p.syms.new {
			continue
		}
		t := res.Type.Elem()
		mem := p.callMem[call]
		if !t.IsStruct() || mem == nil || t.Size() > 1<<13 {
			continue
		}
		covered := make([]bool, t.Size())
		seq := map[*Value]bool{mem: true}
		last := mem
		type span struct{ off, end int64 }
		var stores []span        // in order
		upTo := map[*Value]int{} // seq memory -> stores done by then
	scan:
		for _, m := range p.chain[call.Block][p.idx[mem]:] {
			switch h := fgoHolder(m); {
			case m.Op == OpCopy:
			case m.Op == OpSelectN && fgoIsCall(h) && (h.Aux.(*AuxCall).Fn == p.syms.new || h.Aux.(*AuxCall).Fn == p.syms.vet):
				seq[h] = true
			case m.Op == OpStore:
				base, off := fgoAddrBase(m.Args[0])
				if base != nil && base != res && fresh(base) {
					break // another new object's field
				}
				if base != res || off < 0 {
					break scan
				}
				sz := m.Aux.(*types.Type).Size()
				if off+sz > t.Size() {
					break scan
				}
				for i := off; i < off+sz; i++ {
					covered[i] = true
				}
				stores = append(stores, span{off, off + sz})
			default:
				break scan
			}
			seq[m] = true
			last = m
			upTo[m] = len(stores)
		}
		var missing []*types.Field
		for _, fd := range t.Fields() {
			whole, none := true, true
			for i := fd.Offset; i < fd.Offset+fd.Type.Size(); i++ {
				whole = whole && covered[i]
				none = none && !covered[i]
			}
			switch {
			case whole:
			case none && fgoZeroable(fd.Type):
				missing = append(missing, fd)
			default:
				goto next
			}
		}
		// Nothing may read the object, or fault, before it is complete.
		for _, v := range call.Block.Values {
			if v.Op == OpPhi {
				continue
			}
			m := v.MemoryArg()
			if m == nil || !seq[m] || seq[v] || m == last && len(missing) == 0 {
				continue
			}
			if v.Op == OpNilCheck && fresh(v.Args[0]) || v.Op == OpSelectN && fgoIsCall(m) {
				continue
			}
			if fgoIsCall(v) && v.Aux.(*AuxCall).Fn == p.syms.vet {
				continue // it reads the object once complete
			}
			if v.Op == OpLoad {
				// A load from an object that already existed cannot see
				// the new ones (nor fault: nil checks come first), one
				// from another new object is that object's concern, and
				// one from this object must read fields already written.
				base, off := fgoAddrBase(v.Args[0])
				if base != nil && base != res {
					continue
				}
				if base == res && off >= 0 && slices.ContainsFunc(stores[:upTo[m]], func(sp span) bool {
					return sp.off <= off && off+v.Type.Size() <= sp.end
				}) {
					continue
				}
			}
			if m == last && (v.Op == OpMakeResult || v.Op == OpPhi) {
				continue // the block's end, which the zero stores go before
			}
			if m == last && p.idx[m] < len(p.chain[call.Block]) && fgoHolder(p.chain[call.Block][p.idx[m]]) == v {
				continue // the operation after them, which the zero stores go before
			}
			goto next
		}
		inits[call] = true
		if len(missing) > 0 {
			b := call.Block
			slot := p.idx[last]
			if zeros[b] == nil {
				zeros[b] = map[int][]*Value{}
			}
			pos := res.Pos.WithNotStmt()
			for _, fd := range missing {
				addr := b.NewValue1I(pos, OpOffPtr, types.NewPtr(fd.Type), fd.Offset, res)
				z := b.NewValue3A(pos, OpStore, types.TypeMem, fd.Type, addr, p.zeroOf(fd.Type), p.initMem)
				if p.f.fgoNoWB == nil {
					p.f.fgoNoWB = map[ID]bool{}
				}
				p.f.fgoNoWB[z.ID] = true // a new object: nothing sees it yet
				zeros[b][slot] = append(zeros[b][slot], z)
			}
		}
	next:
	}
	return inits, zeros
}

// fgoZeroable reports whether a zero of t is one constant.
func fgoZeroable(t *types.Type) bool {
	return t.IsBoolean() || t.IsInteger() || t.IsFloat() || t.IsPtrShaped() && !t.IsComplex()
}

func (p *fgoRCPass) zeroOf(t *types.Type) *Value {
	f := p.f
	switch {
	case t.IsBoolean():
		return f.ConstBool(t, false)
	case t.IsFloat() && t.Size() == 4:
		return f.ConstFloat32(t, 0)
	case t.IsFloat():
		return f.ConstFloat64(t, 0)
	case t.IsInteger():
		switch t.Size() {
		case 1:
			return f.ConstInt8(t, 0)
		case 2:
			return f.ConstInt16(t, 0)
		case 4:
			return f.ConstInt32(t, 0)
		}
		return f.ConstInt64(t, 0)
	}
	return f.ConstNil(t)
}

func fgoBool(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fgoRCPtrFieldAt reports whether struct t has a nogc pointer field of
// type ft at offset off.
func fgoRCPtrFieldAt(t *types.Type, off int64, ft *types.Type) bool {
	if !t.IsStruct() {
		return false
	}
	for _, fd := range t.Fields() {
		if fd.Offset == off {
			return fd.Type.FgoIsRCPtr() && types.Identical(fd.Type, ft)
		}
	}
	return false
}

func (p *fgoRCPass) shadowOf(d *fgoDef) *ir.Name {
	if p.shadow == nil {
		p.shadow = map[*fgoDef]*ir.Name{}
	}
	n := p.shadow[d]
	if n == nil {
		n = p.f.NewLocal(d.v.Pos, d.v.Type)
		p.shadow[d] = n
	}
	return n
}

// shadowSet records d in its shadow slot as soon as it is owned.
func (p *fgoRCPass) shadowSet(d *fgoDef) {
	order := fgoOrderOwn
	if d.v.Op != OpSelectN && d.v.Op != OpPhi && d.v.Op != OpArg {
		order = fgoOrderSet // a load or conversion: owned from its increment on
	}
	p.reqs = append(p.reqs, &fgoReq{b: d.block, slot: d.slot, order: order, v: d.v, shadow: p.shadowOf(d)})
}

// shadowClearAt forgets d where use u hands it on: just after a store
// takes it (a faulting store leaves it ours), just before a call takes it
// (the callee owns it while it runs), or at the end of the block for a
// result or phi.
func (p *fgoRCPass) shadowClearAt(d *fgoDef, u fgoUse) {
	slot := p.slotAfter(u.b, u.pos)
	if u.call || u.pos >= fgoEnd {
		slot = p.slotBefore(u.b, u.pos)
	}
	p.reqs = append(p.reqs, &fgoReq{b: u.b, slot: slot, order: fgoOrderClear, shadow: p.shadowOf(d)})
}

// planTokens keeps each reuse token in a shadow slot from the release
// that produced it to just before the allocation that takes it, so the
// memory is freed if a panic unwinds the frame in between.
func (p *fgoRCPass) planTokens() {
	n := len(p.reqs)
	for _, r := range p.reqs[:n] {
		if r.reuse == nil {
			continue
		}
		t := p.f.NewLocal(r.v.Pos, p.f.Config.Types.BytePtr)
		p.tokens = append(p.tokens, t)
		p.reqs = append(p.reqs,
			&fgoReq{b: r.b, slot: r.slot, order: fgoOrderRelease, shadow: t, tokOf: r},
			&fgoReq{b: r.reuseB, slot: r.reuseS, order: fgoOrderClear, shadow: t})
	}
}

// planShadows zeroes every shadow slot on entry and keeps it alive to
// each return, so the stores into it stay and the runtime can read it.
func (p *fgoRCPass) planShadows() {
	all := slices.Concat(p.tokens, p.bufs)
	for _, n := range p.shadow {
		all = append(all, n)
	}
	for _, n := range all {
		p.reqs = append(p.reqs, &fgoReq{b: p.f.Entry, slot: 0, order: fgoOrderZero, shadow: n})
		for _, b := range p.f.Blocks {
			if b.Kind == BlockRet || b.Kind == BlockRetJmp {
				p.reqs = append(p.reqs, &fgoReq{b: b, slot: p.endSlot(b), order: fgoOrderKeep, shadow: n, keep: true})
			}
		}
	}
}

func (p *fgoRCPass) incBefore(v *Value, u fgoUse) {
	p.reqs = append(p.reqs, &fgoReq{b: u.b, slot: p.slotBefore(u.b, u.pos), inc: true, v: v})
}

// planReuse pairs a release with a following allocation of the same
// size class in the same block.
func (p *fgoRCPass) planReuse() {
	byBlock := map[*Block][]*fgoReq{}
	paired := map[*Value]bool{}
	for _, r := range p.reqs {
		if !r.inc && !r.clone && !r.escape && r.shadow == nil {
			byBlock[r.b] = append(byBlock[r.b], r)
			paired[r.reuse] = true
		}
	}
	for b, rels := range byBlock {
		slices.SortStableFunc(rels, func(x, y *fgoReq) int { return x.slot - y.slot })
		for j, m := range p.chain[b] {
			if m.Op != OpSelectN || !fgoIsCall(m.Args[0]) {
				continue
			}
			call := m.Args[0]
			res := p.callRes[call]
			if paired[call] || call.Aux.(*AuxCall).Fn != p.syms.new || res == nil {
				continue
			}
			class := fgoSizeClass(res.Type.Elem())
			for k := len(rels) - 1; k >= 0; k-- {
				r := rels[k]
				if r.slot <= j && r.reuse == nil && fgoSizeClass(r.v.Type.Elem()) == class {
					r.reuse, r.reuseB, r.reuseS = call, b, j
					paired[call] = true
					break
				}
			}
		}
	}
}

func fgoSizeClass(t *types.Type) int64 { return (t.Size() + 15) / 16 }

// planReuseAcross pairs releases still unpaired with an allocation in a
// later block, when every path from the release reaches that allocation
// exactly once: the release's block dominates the allocation's, no path
// leaves the function or loops back without passing the allocation, and
// the allocation cannot run again without passing the release first.
func (p *fgoRCPass) planReuseAcross() {
	paired := map[*Value]bool{}
	var rels []*fgoReq
	for _, r := range p.reqs {
		switch {
		case r.inc, r.clone, r.escape, r.shadow != nil:
		case r.reuse != nil:
			paired[r.reuse] = true
		default:
			rels = append(rels, r)
		}
	}
	if len(rels) == 0 {
		return
	}
	sdom := p.f.Sdom()
	for _, b := range p.f.Blocks {
		for j, m := range p.chain[b] {
			if m.Op != OpSelectN || !fgoIsCall(m.Args[0]) {
				continue
			}
			call := m.Args[0]
			res := p.callRes[call]
			if paired[call] || call.Aux.(*AuxCall).Fn != p.syms.new || res == nil {
				continue
			}
			class := fgoSizeClass(res.Type.Elem())
			for _, r := range rels {
				if r.reuse != nil || r.b == b || fgoSizeClass(r.v.Type.Elem()) != class ||
					!sdom.IsAncestorEq(r.b, b) || !fgoAlwaysReaches(r.b, b) || fgoReachesAvoiding(b, b, r.b) {
					continue
				}
				r.reuse, r.reuseB, r.reuseS = call, b, j
				paired[call] = true
				break
			}
		}
	}
}

// fgoAlwaysReaches reports whether every path leaving from reaches to
// before returning, exiting, or coming back to from.
func fgoAlwaysReaches(from, to *Block) bool {
	seen := map[*Block]bool{from: true}
	work := []*Block{from}
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		if len(b.Succs) == 0 {
			return false
		}
		for _, e := range b.Succs {
			s := e.b
			if s == to {
				continue
			}
			if s == from {
				return false
			}
			if !seen[s] {
				seen[s] = true
				work = append(work, s)
			}
		}
	}
	return true
}

// fgoReachesAvoiding reports whether some path from from's successors
// reaches to without passing through avoid.
func fgoReachesAvoiding(from, to, avoid *Block) bool {
	seen := map[*Block]bool{}
	work := []*Block{from}
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		for _, e := range b.Succs {
			s := e.b
			if s == to {
				return true
			}
			if s == avoid || seen[s] {
				continue
			}
			seen[s] = true
			work = append(work, s)
		}
	}
	return false
}

func (p *fgoRCPass) call(b *Block, pos src.XPos, fn *obj.LSym, args []*Value, results []*types.Type) (call, mem *Value, res []*Value) {
	return p.callAt(b, pos, fn, args, results, p.initMem)
}

// callAt is call with the memory state the call takes.
func (p *fgoRCPass) callAt(b *Block, pos src.XPos, fn *obj.LSym, args []*Value, results []*types.Type, mem0 *Value) (call, mem *Value, res []*Value) {
	f := p.f
	off := f.Config.ctxt.Arch.FixedFrameSize
	argTypes := make([]*types.Type, len(args))
	for i, a := range args {
		t := a.Type
		argTypes[i] = t
		off = types.RoundUp(off, t.Alignment())
		off += t.Size()
	}
	off = types.RoundUp(off, int64(types.RegSize))
	aux := StaticAuxCall(fn, f.ABIDefault.ABIAnalyzeTypes(argTypes, results))
	call = b.NewValue0A(pos, OpStaticLECall, aux.LateExpansionResultType(), aux)
	call.AddArgs(args...)
	call.AddArg(mem0)
	mem = b.NewValue1I(pos, OpSelectN, types.TypeMem, int64(len(results)), call)
	for i, t := range results {
		off = types.RoundUp(off, t.Alignment())
		res = append(res, b.NewValue1I(pos, OpSelectN, t, int64(i), call))
		off += t.Size()
	}
	call.AuxInt = types.RoundUp(off, int64(types.PtrSize))
	return
}

func (p *fgoRCPass) uintptrConst(c int64) *Value {
	t := p.f.Config.Types.Uintptr
	if p.f.Config.PtrSize == 4 {
		return p.f.ConstInt32(t, int32(c))
	}
	return p.f.ConstInt64(t, c)
}

// elemSize is the element size of slice v, as a call argument.
func (p *fgoRCPass) elemSize(v *Value) *Value {
	t := p.f.Config.Types.Uintptr
	if p.f.Config.PtrSize == 4 {
		return p.f.ConstInt32(t, int32(v.Type.Elem().Size()))
	}
	return p.f.ConstInt64(t, v.Type.Elem().Size())
}

func (p *fgoRCPass) materialize() {
	f := p.f
	bytePtr := f.Config.Types.BytePtr

	inits, zeros := p.fullInits()

	// Create the inserted calls, slot by slot, incs before releases.
	ins := map[*Block]map[int][]*Value{}
	slices.SortStableFunc(p.reqs, func(x, y *fgoReq) int { return x.order - y.order })
	reuse := map[*Value]*fgoReq{}
	isSet := map[*Value]bool{}    // shadow stores recording a newly owned value
	clones := map[*Value]*Value{} // string load -> its copy
	cloneCalls := map[*Value]bool{}
	for _, r := range p.reqs {
		pos := r.b.Pos.WithNotStmt()
		var mem *Value
		switch {
		case r.shadow != nil:
			mem = p.shadowOp(r)
		case r.escape:
			_, mem, _ = p.call(r.b, pos, p.syms.escape, []*Value{r.v}, nil)
		case r.fused:
			continue
		case r.inc:
			_, mem, _ = p.call(r.b, pos, p.syms.inc, []*Value{r.v}, nil)
		case r.clone && r.dup != nil:
			var res []*Value
			aux := *r.dup.Aux.(*AuxCall)
			if r.v.Type.IsSlice() {
				_, mem, res = p.call(r.b, pos, p.syms.sliceDup, []*Value{r.v, p.elemSize(r.v)}, []*types.Type{r.v.Type})
				aux.Fn = p.syms.slicePut
				r.tok = r.b.NewValue1(pos, OpSlicePtr, bytePtr, res[0])
			} else {
				_, mem, res = p.call(r.b, pos, p.syms.strDup, []*Value{r.v}, []*types.Type{r.v.Type})
				aux.Fn = p.syms.strPut
				r.tok = r.b.NewValue1(pos, OpStringPtr, bytePtr, res[0])
			}
			r.dup.SetArg(1, res[0])
			r.dup.Aux = &aux
		case r.clone:
			var call *Value
			var res []*Value
			if types.FgoPtrSliceType(r.v.Type) {
				call, mem, res = p.call(r.b, pos, p.syms.ptrsClone, []*Value{r.v}, []*types.Type{r.v.Type})
			} else if r.v.Type.IsSlice() {
				call, mem, res = p.call(r.b, pos, p.syms.sliceClone, []*Value{r.v, p.elemSize(r.v)}, []*types.Type{r.v.Type})
			} else {
				call, mem, res = p.call(r.b, pos, p.syms.strClone, []*Value{r.v}, []*types.Type{r.v.Type})
			}
			clones[r.v] = res[0]
			cloneCalls[call] = true
		case r.reuse != nil && r.keepOf != 0:
			fn := p.syms.dropReuseKeep
			if r.keepOf == fgoRCOnlyPtrs(r.v.Type.Elem()) {
				fn = p.syms.dropReuseAll
			}
			var res []*Value
			_, mem, res = p.call(r.b, pos, fn, []*Value{r.v, p.uintptrConst(r.keepOf)}, []*types.Type{bytePtr})
			r.tok = res[0]
			reuse[r.reuse] = r
		case r.reuse != nil:
			var res []*Value
			_, mem, res = p.call(r.b, pos, p.syms.dropReuse, []*Value{r.v}, []*types.Type{bytePtr})
			r.tok = res[0]
			reuse[r.reuse] = r
		case r.keepOf != 0:
			fn := p.syms.decKeep
			if r.keepOf == fgoRCOnlyPtrs(r.v.Type.Elem()) {
				fn = p.syms.decAll
			}
			_, mem, _ = p.call(r.b, pos, fn, []*Value{r.v, p.uintptrConst(r.keepOf)}, nil)
		default:
			_, mem, _ = p.call(r.b, pos, p.syms.dec, []*Value{r.v}, nil)
		}
		if r.order == fgoOrderOwn {
			isSet[mem] = true
		}
		if ins[r.b] == nil {
			ins[r.b] = map[int][]*Value{}
		}
		ins[r.b][r.slot] = append(ins[r.b][r.slot], mem)
	}
	for b, slots := range zeros {
		if ins[b] == nil {
			ins[b] = map[int][]*Value{}
		}
		for slot, vs := range slots {
			ins[b][slot] = append(vs, ins[b][slot]...)
		}
	}

	if len(clones) > 0 {
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if cloneCalls[v] {
					continue
				}
				for i, a := range v.Args {
					if c := clones[a]; c != nil {
						v.SetArg(i, c)
					}
				}
			}
		}
	}

	// Allocations whose fields are all written right away need not
	// clear their memory.
	for call := range inits {
		if reuse[call] == nil {
			aux := *call.Aux.(*AuxCall)
			aux.Fn = p.syms.newInit
			call.Aux = &aux
		}
	}

	// Turn paired allocations into reusing ones.
	for call, r := range reuse {
		typ := call.Args[0]
		res := p.callRes[call]
		newAt := p.syms.newAt
		if inits[call] {
			newAt = p.syms.newAtInit
		}
		aux := StaticAuxCall(newAt, f.ABIDefault.ABIAnalyzeTypes([]*types.Type{bytePtr, typ.Type}, []*types.Type{res.Type}))
		mem := call.MemoryArg()
		call.resetArgs()
		call.Aux = aux
		call.Type = aux.LateExpansionResultType()
		call.AddArgs(r.tok, typ, mem)
		off := types.RoundUp(f.Config.ctxt.Arch.FixedFrameSize+2*int64(types.PtrSize), int64(types.RegSize))
		call.AuxInt = types.RoundUp(off+int64(types.PtrSize), int64(types.PtrSize))
	}

	// Rebuild memory: thread every block's chain, including the new
	// calls, from its start memory, adding phis where paths now carry
	// different memory.
	start := map[*Block]*Value{}
	end := map[*Block]*Value{}
	phis := map[*Block]*Value{}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpPhi && v.Type.IsMemory() {
				phis[b] = v
			}
		}
	}
	po := f.postorder()
	for i := len(po) - 1; i >= 0; i-- {
		b := po[i]
		var s *Value
		switch {
		case phis[b] != nil:
			s = phis[b]
		case b == f.Entry:
			s = p.initMem
		case len(b.Preds) == 1:
			s = end[b.Preds[0].b]
			if s == nil {
				f.Fatalf("fgo rc: predecessor of %v not yet threaded", b)
			}
		default:
			s = b.NewValue0(b.Pos, OpPhi, types.TypeMem)
			phis[b] = s
		}
		start[b] = s

		orig := p.chain[b]
		var ch []*Value
		ch = append(ch, ins[b][0]...)
		for k, m := range orig {
			ch = append(ch, m)
			ch = append(ch, ins[b][k+1]...)
		}
		holders := map[*Value]bool{}
		prev := s
		for _, m := range ch {
			h := fgoHolder(m)
			holders[h] = true
			h.SetArg(len(h.Args)-1, prev)
			prev = m
		}
		end[b] = prev

		// Values reading the memory a slot follows read it after the
		// slot's shadow stores, so a fault there (say, a load through
		// a nil pointer) already finds the values just owned recorded.
		lastSet := func(list []*Value) *Value {
			var m *Value
			for _, x := range list {
				if isSet[x] {
					m = x
				}
			}
			return m
		}
		startRead := s
		if m := lastSet(ins[b][0]); m != nil {
			startRead = m
		}
		readMem := map[*Value]*Value{}
		for k, m := range orig {
			if x := lastSet(ins[b][k+1]); x != nil {
				readMem[m] = x
			}
		}

		for _, v := range b.Values {
			if v.Op == OpPhi || v.Op == OpInitMem || holders[v] {
				continue
			}
			ma := fgoMemIn(v)
			if ma == nil {
				continue
			}
			switch {
			case v.Op == OpMakeResult || fgoIsCall(v) && p.callMem[v] == nil:
				v.SetArg(len(v.Args)-1, end[b])
			case ma.Block == b && p.idx[ma] > 0:
				if r := readMem[ma]; r != nil && fgoCanFault(v) {
					v.SetArg(len(v.Args)-1, r)
				}
			case fgoCanFault(v):
				v.SetArg(len(v.Args)-1, startRead)
			default:
				v.SetArg(len(v.Args)-1, s)
			}
		}
		for ci, c := range b.ControlValues() {
			if c.Type == types.TypeMem {
				b.ReplaceControl(ci, end[b])
			}
		}
	}
	for b, phi := range phis {
		if len(phi.Args) == 0 {
			for _, e := range b.Preds {
				phi.AddArg(end[e.b])
			}
			continue
		}
		for i, e := range b.Preds {
			phi.SetArg(i, end[e.b])
		}
	}
}

// fgoCanFault reports whether v, which reads memory, can fault.
func fgoCanFault(v *Value) bool { return v.Op == OpLoad || v.Op == OpNilCheck }

func fgoDump(b *Block) string {
	s := ""
	for _, v := range b.Values {
		s += v.LongString() + "\n"
	}
	return s
}

func fgoIsZeroConst(v *Value) bool {
	switch v.Op {
	case OpConstNil:
		return true
	case OpConst8, OpConst16, OpConst32, OpConst64:
		return v.AuxInt == 0
	}
	return false
}

// fgoOwnedCallees maps a callee's symbol name to which of its arguments
// (receiver first) it takes ownership of. ssagen records it as it builds
// each static call, from the parameter tags escape analysis wrote (or
// export data carried).
var fgoOwnedCallees sync.Map

// FgoNoteCallee records the argument ownership of static callee sym,
// whose declared type is t.
func FgoNoteCallee(sym *obj.LSym, t *types.Type) {
	var owned []bool
	any := false
	for _, f := range t.RecvParams() {
		o := types.FgoParamOwned(f)
		owned = append(owned, o)
		any = any || o
	}
	if any {
		fgoOwnedCallees.Store(sym.Name, owned)
	}
}

// ownedResults reports whether call returns references it owns: calls to
// nogc copies of functions, and to the runtime's allocators.
func (p *fgoRCPass) ownedResults(call *Value) bool {
	if call.Op != OpStaticLECall && call.Op != OpTailLECall {
		return false
	}
	fn := call.Aux.(*AuxCall).Fn
	return fgoNogcCallee(fn) || p.isNew(fn) || fn == p.syms.newAt || fn == p.syms.take || fn == p.syms.newInit || fn == p.syms.newAtInit
}

// fgoNogcCallee reports whether fn is the nogc copy of a function.
func fgoNogcCallee(fn *obj.LSym) bool {
	return fn != nil && strings.Contains(fn.Name, "._fgonogc_")
}

// fgoRuntimeCallee reports whether fn is one of the runtime's nogc
// helpers, which do not keep the pointers they get.
func fgoRuntimeCallee(fn *obj.LSym) bool {
	return fn != nil && strings.HasPrefix(fn.Name, "runtime.fgo")
}

// handsOff reports whether nogc code must hand the object behind the
// i'th argument of call (the argument itself if direct, else an address
// inside the object) to the GC before the call: the callee is not nogc
// code, and it may keep the pointer, call through it, or change pointers
// in the memory it points to. The callee's escape analysis tags tell,
// where it is known.
func (p *fgoRCPass) handsOff(call *Value, i int, direct bool) bool {
	var fn *obj.LSym
	if call.Op == OpStaticLECall || call.Op == OpTailLECall {
		fn = call.Aux.(*AuxCall).Fn
	}
	if fgoNogcCallee(fn) || fgoRuntimeCallee(fn) {
		return false
	}
	if fn == nil {
		return true
	}
	k, ok := fgoCalleeKeeps.Load(fn.Name)
	if !ok {
		return true
	}
	keeps := k.([]fgoKeep)
	if i >= len(keeps) {
		return true
	}
	kp := keeps[i]
	if kp.heap || kp.callee || kp.result {
		// (A result may point into the object: nogc code only holds
		// pointers into objects handed off to the GC.)
		return true
	}
	if kp.mutator {
		// Changing scalars is fine; changing pointers is not.
		t := call.Args[i].Type
		return !t.IsPtr() || t.Elem().HasPointers()
	}
	return false
}

// fgoKeep is what a function may do with one of its parameters, from
// its escape analysis tag.
type fgoKeep struct {
	heap, callee, mutator, result bool
}

// fgoCalleeKeeps maps the names of statically called functions to
// what they may do with each parameter (receiver first).
var fgoCalleeKeeps sync.Map

// FgoNoteKeeps records, for a statically called function, what it may do
// with each parameter.
func FgoNoteKeeps(sym *obj.LSym, keeps []FgoKeep) {
	ks := make([]fgoKeep, len(keeps))
	for i, k := range keeps {
		ks[i] = fgoKeep{k.Heap, k.Callee, k.Mutator, k.Result}
	}
	fgoCalleeKeeps.Store(sym.Name, ks)
}

// FgoKeep is fgoKeep, for ssagen.
type FgoKeep struct {
	Heap, Callee, Mutator, Result bool
}

// fgoArgRC reports whether call's i'th parameter is a nogc pointer.
func fgoArgRC(call *Value, i int) bool {
	aux, ok := call.Aux.(*AuxCall)
	if !ok || int64(i) >= aux.NArgs() {
		return true // closure pointer and the like
	}
	return aux.TypeOfArg(int64(i)).FgoIsRCPtr()
}

// fgoArgOwnedAt reports whether call takes ownership of its i'th argument.
func fgoArgOwnedAt(call *Value, i int) bool {
	if call.Op != OpStaticLECall && call.Op != OpTailLECall {
		return false
	}
	switch call.Aux.(*AuxCall).Fn.Name {
	case "runtime.fgoRCStore", "runtime.fgoRCStoreGC", "runtime.fgoRCStoreTag", "runtime.fgoRCStoreTagGC":
		return i == 2
	}
	o, ok := fgoOwnedCallees.Load(call.Aux.(*AuxCall).Fn.Name)
	if !ok {
		return false
	}
	owned := o.([]bool)
	return i < len(owned) && owned[i]
}

// fgoArgOwned reports whether parameter v of the function being compiled
// is owned.
func fgoArgOwned(v *Value) bool {
	name, ok := v.Aux.(*ir.Name)
	if !ok || name.Curfn == nil {
		return false
	}
	for _, f := range name.Curfn.Type().RecvParams() {
		if f.Nname == name {
			return types.FgoParamOwned(f)
		}
	}
	return false
}

// shadowOp builds the memory operation for shadow slot request r.
func (p *fgoRCPass) shadowOp(r *fgoReq) *Value {
	b, n := r.b, r.shadow
	pos := b.Pos.WithNotStmt()
	if r.keep {
		return b.NewValue1A(pos, OpVarLive, types.TypeMem, n, p.initMem)
	}
	if p.sp == nil {
		for _, v := range p.f.Entry.Values {
			if v.Op == OpSP {
				p.sp = v
			}
		}
		if p.sp == nil {
			p.sp = p.f.Entry.NewValue0(src.NoXPos, OpSP, p.f.Config.Types.Uintptr)
		}
	}
	addr := b.NewValue2A(pos, OpLocalAddr, types.NewPtr(n.Type()), n, p.sp, p.initMem)
	v := r.v
	if r.tokOf != nil {
		v = r.tokOf.tok
	}
	if v == nil {
		v = b.NewValue0(pos, OpConstNil, n.Type())
	}
	return b.NewValue3A(pos, OpStore, types.TypeMem, n.Type(), addr, v, p.initMem)
}

// fgoShadowSlots maps a function (*ir.Func) to the shadow slots its
// reference counting uses, for ssagen to describe in FUNCDATA_FgoShadow.
var fgoShadowSlots sync.Map

// FgoShadow lists a function's shadow slots: those holding owned nogc
// pointers (released on unwind) and those holding reuse tokens (whose
// memory is freed on unwind).
type FgoShadow struct {
	Owned, Tokens, Bufs []*ir.Name
}

// FgoTakeShadowSlots returns and forgets the shadow slots recorded for fn.
func FgoTakeShadowSlots(fn *ir.Func) FgoShadow {
	v, ok := fgoShadowSlots.LoadAndDelete(fn)
	if !ok {
		return FgoShadow{}
	}
	return v.(FgoShadow)
}
