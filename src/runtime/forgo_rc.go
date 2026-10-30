// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// Reference-counted ("nogc") objects. Code compiled under //fgo:nogc
// (see cmd/compile/internal/noder's forgo_nogc.go) allocates the structs
// it takes the address of with fgoRCNew instead of the GC heap, and
// counts the pointers it holds to them with fgoRCInc and fgoRCDec, which
// free each object when its last reference dies. Any struct type can be
// allocated this way: the count lives in a header just before the
// object, not in the type, and the same type is garbage collected when
// other code allocates it.
//
// Only nogc code counts. So that other code can still be handed a nogc
// object, the object is first handed off to the garbage collector
// (fgoRCHandoff), along with every nogc object it can reach: from then
// on its fields hold ordinary GC values, which nogc code no longer
// counts, and the GC frees it once nothing points to it any more (see
// "Handing objects to the GC" below). Counted objects are therefore only
// ever reachable from one goroutine, and their counts change without
// atomics; the counts of handed-off objects, which nogc code on any
// goroutine may hold, change atomically.
//
// A counted object's nogc pointer fields (pointers to structs) hold
// references to counted or handed-off objects, or nil, and nothing else:
// a store of any other pointer hands the object off first. Its other
// fields hold scalars, buffers it owns (string and scalar slice fields,
// and slices of nogc pointers, see below), or GC values, which the GC
// scans while the object lives (mixed objects).
type fgoRCHeader struct {
	rc  uintptr // reference count; reused as a free/release-list link
	typ *_type
}

const fgoHdr = unsafe.Sizeof(fgoRCHeader{})

const (
	fgoRCAlign    = 16
	fgoRCMaxSize  = 32 << 10
	fgoRCClasses  = fgoRCMaxSize / fgoRCAlign
	fgoRCCached   = 64 // size classes (up to 1 KiB) served from the per-M cache
	fgoRCChunk    = 64 << 10
	fgoRCKeep     = 256 << 10 // most free bytes an M keeps per size class
	fgoRCSpares   = 4096      // spare batches per size class; more are merged
	fgoRCHdrWords = fgoHdr / goarch.PtrSize
)

// fgoHdrOf returns the header of the object at p.
func fgoHdrOf(p unsafe.Pointer) *fgoRCHeader {
	return (*fgoRCHeader)(unsafe.Pointer(uintptr(p) - fgoHdr))
}

// Object states. Counting ignores fgoNone: a pointer into the GC heap,
// a global, a stack, or into the middle of an object.
const (
	fgoNone    = 0
	fgoCounted = 1 // counted object
	fgoHanded  = 2 // object handed off to the GC
	fgoBufData = 4 // (in fgoArena.state) a buffer handed off to the GC: bytes
	fgoBufPtrs = 5 // a buffer handed off to the GC: nogc pointers
)

// A pointer that nogc code holds into the arena points to the start of an
// object, except where it came from code that is not nogc, which can only
// have pointers into objects handed off to the GC (nogc code hands off
// any object it lets such code take the address of a field of, or return
// a pointer into). So fgoArena.chunks keeps a count for each fgoRCChunk
// bytes of the arena that hold any handed-off object, and fgoArena.state
// marks the start of each handed-off object (one byte per fgoRCAlign
// bytes). Elsewhere, a pointer into the arena is a counted object.

// fgoState returns the state of the object at p, or fgoNone if p does
// not point to the start of a nogc object.
//
//go:nosplit
func fgoState(p unsafe.Pointer) uint8 {
	if s := fgoQuick(p); s != fgoMaybe {
		return s
	}
	return fgoStateSlow(uintptr(p))
}

// fgoMaybe is what fgoQuick returns for a pointer into a chunk holding
// handed-off objects, for fgoStateSlow to decide.
const fgoMaybe = 3

// fgoQuick is fgoState, short enough to inline, for the common cases.
//
//go:nosplit
func fgoQuick(p unsafe.Pointer) uint8 {
	d := uintptr(p) - fgoArena.lo
	if d >= fgoArenaSize { // (nothing points into the unmapped part)
		return fgoNone
	}
	if *(*uint32)(unsafe.Pointer(fgoArena.chunks + d/fgoRCChunk*4)) != 0 {
		return fgoMaybe
	}
	return fgoCounted
}

// fgoStateSlow is fgoState for a pointer into a chunk holding handed-off
// objects.
//
//go:nosplit
func fgoStateSlow(p uintptr) uint8 {
	d := p - fgoArena.lo
	if atomic.Load8((*uint8)(unsafe.Pointer(fgoArena.state+d/fgoRCAlign))) == fgoHanded {
		return fgoHanded
	}
	if fgoHandedObj(p) != 0 {
		return fgoNone // into the middle of one
	}
	return fgoCounted
}

// fgoMarkHanded records that p's object (or buffer) was handed off (s is
// fgoHanded, fgoBufData or fgoBufPtrs) or freed after that (fgoNone),
// counting it in its chunk's entry in fgoArena.chunks.
func fgoMarkHanded(p unsafe.Pointer, s uint8) {
	d := uintptr(p) - fgoArena.lo
	c := (*uint32)(unsafe.Pointer(fgoArena.chunks + d/fgoRCChunk*4))
	if s != fgoNone {
		atomic.Xadd(c, 1)
	}
	atomic.Store8((*uint8)(unsafe.Pointer(fgoArena.state+d/fgoRCAlign)), s)
	if s == fgoNone {
		atomic.Xadd(c, -1)
	}
}

// fgoRCCache is an M's private allocator state. Counted objects never
// cross goroutines, but a goroutine may move between Ms, so memory freed
// on one M may be reused on another; that is fine, it is only memory.
// Allocation and freeing run with the M acquired, so no lock is needed.
// So that memory freed on one M is not stranded there while another M
// carves new chunks, a free list that grows past fgoRCKeep bytes moves,
// whole, onto the shared stack of spare batches for its class, and an M
// whose list is empty takes one batch from there before carving. Lists
// keep their tails, so moving one touches none of its objects, and no M
// ever holds much more than fgoRCKeep bytes of free memory of a class.
// Free lists link blocks (headers, not objects) through their first word.
type fgoRCCache struct {
	free   [fgoRCCached + 1]uintptr // per size class, linked through rc
	tail   [fgoRCCached + 1]uintptr
	nfree  [fgoRCCached + 1]uint32
	keep   [fgoRCCached + 1]uint32 // fgoRCKeep bytes, in objects of each class
	chunk  uintptr
	end    uintptr
	live   int64 // objects allocated minus objects freed on this M
	reuses int64 // allocations satisfied by fgoRCNewAt reuse on this M
	descs  [64]*fgoRCDesc
}

// fgoRCDesc lists where a type holds what a counted object's release
// must drop: the offset of each nogc pointer field, and of each string
// field with its low bit set (offsets are pointer aligned). Slice fields
// are listed like string fields: both start with a data pointer and a
// length. Slices of nogc pointers have bit 1 set instead: each element
// holds a reference. Descriptors are built once per type and never
// freed; each M caches the ones it uses.
type fgoRCDesc struct {
	typ   *_type
	mixed bool // allocated by fgoRCNewMixed
	next  *fgoRCDesc
	n     uintptr
	offs  [1 << 16]uintptr // really n entries
}

var fgoRCDescs struct {
	lock mutex
	list *fgoRCDesc
}

func fgoRCDescOf(mp *m, typ *_type) *fgoRCDesc {
	rc := fgoRCCacheOf(mp)
	slot := &rc.descs[uintptr(unsafe.Pointer(typ))/8%uintptr(len(rc.descs))]
	if d := *slot; d != nil && d.typ == typ {
		return d
	}
	lock(&fgoRCDescs.lock)
	d := fgoRCDescs.list
	for d != nil && d.typ != typ {
		d = d.next
	}
	if d == nil {
		fields := (*structtype)(unsafe.Pointer(typ)).Fields
		d = (*fgoRCDesc)(persistentalloc(unsafe.Offsetof(fgoRCDesc{}.offs)+uintptr(len(fields))*goarch.PtrSize, goarch.PtrSize, &memstats.other_sys))
		d.typ = typ
		for _, f := range fields {
			switch f.Typ.Kind() {
			case abi.Pointer:
				if fgoIsRCPtrType(f.Typ) {
					d.offs[d.n] = f.Offset
					d.n++
				}
			case abi.String:
				d.offs[d.n] = f.Offset | 1
				d.n++
			case abi.Slice:
				switch e := (*slicetype)(unsafe.Pointer(f.Typ)).Elem; {
				case fgoIsRCPtrType(e):
					d.offs[d.n] = f.Offset | 2 // nogc pointers
					d.n++
				case e.PtrBytes == 0:
					d.offs[d.n] = f.Offset | 1
					d.n++
				}
			}
			// Everything else holds GC values, which the GC manages.
		}
		d.next = fgoRCDescs.list
		fgoRCDescs.list = d
	}
	unlock(&fgoRCDescs.lock)
	*slot = d
	return d
}

// fgoIsRCPtrType reports whether t is a nogc pointer type: a pointer to a
// struct. The compiler counts the same types (types.Type.FgoIsRCPtr).
func fgoIsRCPtrType(t *_type) bool {
	return t.Kind() == abi.Pointer && (*ptrtype)(unsafe.Pointer(t)).Elem.Kind() == abi.Struct
}

// fgoRC serves the size classes too large for the per-M caches.
var fgoRC struct {
	lock       mutex
	free       [fgoRCClasses + 1]uintptr
	chunk, end uintptr
	live       int64
	spare      [fgoRCCached + 1]*[fgoRCSpares]fgoRCBatch
	nspare     [fgoRCCached + 1]uint32
}

// fgoRCBatch is a free list handed back by an M.
type fgoRCBatch struct {
	head, tail uintptr
	n          uint32
}

func fgoRCCacheOf(mp *m) *fgoRCCache {
	if mp.fgoRC == nil {
		fgoRCNewCache(mp)
	}
	return mp.fgoRC
}

//go:noinline
func fgoRCNewCache(mp *m) {
	rc := (*fgoRCCache)(persistentalloc(unsafe.Sizeof(fgoRCCache{}), goarch.PtrSize, &memstats.other_sys))
	for c := uintptr(1); c <= fgoRCCached; c++ {
		rc.keep[c] = uint32(fgoRCKeep / (c * fgoRCAlign))
	}
	mp.fgoRC = rc
}

func fgoRCClass(size uintptr) uintptr {
	return (size + fgoRCAlign - 1) / fgoRCAlign
}

// fgoRCTake pops a free block of class c, or carves a new one from the
// current chunk. fresh reports whether the memory is still zero.
func fgoRCTake(free []uintptr, chunk, end *uintptr, c uintptr) (p uintptr, fresh bool) {
	if free == nil {
		// carve only
	} else if p = free[c]; p != 0 {
		free[c] = (*fgoRCHeader)(unsafe.Pointer(p)).rc
		return p, false
	}
	n := c * fgoRCAlign
	if *chunk+n > *end {
		*chunk = uintptr(fgoArenaAlloc(fgoRCChunk))
		*end = *chunk + fgoRCChunk
	}
	p = *chunk
	*chunk += n
	return p, true
}

// fgoRCTakeCached is fgoRCTake for a size class served from rc.
func fgoRCTakeCached(rc *fgoRCCache, c uintptr) (p uintptr, fresh bool) {
	if p = rc.free[c]; p == 0 {
		return fgoRCRefill(rc, c)
	}
	if rc.free[c] = (*fgoRCHeader)(unsafe.Pointer(p)).rc; rc.free[c] == 0 {
		rc.tail[c] = 0
	}
	rc.nfree[c]--
	return p, false
}

// fgoRCRefill takes the spare list of class c when rc's own is empty, or
// else carves new memory.
//
//go:noinline
func fgoRCRefill(rc *fgoRCCache, c uintptr) (p uintptr, fresh bool) {
	if atomic.Load(&fgoRC.nspare[c]) != 0 {
		lock(&fgoRC.lock)
		if n := fgoRC.nspare[c]; n != 0 {
			b := fgoRC.spare[c][n-1]
			rc.free[c], rc.tail[c], rc.nfree[c] = b.head, b.tail, b.n
			atomic.Store(&fgoRC.nspare[c], n-1)
		}
		unlock(&fgoRC.lock)
		if rc.free[c] != 0 {
			return fgoRCTakeCached(rc, c)
		}
	}
	return fgoRCTake(nil, &rc.chunk, &rc.end, c)
}

// fgoRCAllocZ allocates an object of type typ with count 1, leaving its
// memory as it is unless zero is set: the caller writes every field
// right away.
func fgoRCAllocZ(typ *_type, zero bool) unsafe.Pointer {
	size := typ.Size_
	if size > fgoRCMaxSize-fgoHdr {
		throw("nogc: object too large")
	}
	c := fgoRCClass(fgoHdr + size)
	var b uintptr
	var fresh bool
	if c <= fgoRCCached {
		mp := acquirem()
		rc := fgoRCCacheOf(mp)
		b, fresh = fgoRCTakeCached(rc, c)
		rc.live++
		releasem(mp)
	} else {
		lock(&fgoRC.lock)
		b, fresh = fgoRCTake(fgoRC.free[:], &fgoRC.chunk, &fgoRC.end, c)
		fgoRC.live++
		unlock(&fgoRC.lock)
	}
	p := unsafe.Pointer(b + fgoHdr)
	if !fresh && zero {
		memclrNoHeapPointers(p, size)
	}
	h := (*fgoRCHeader)(unsafe.Pointer(b))
	h.rc = 1
	h.typ = typ
	if fgoWantGC {
		fgoStartGC()
	}
	return p
}

// fgoRCFree returns p's memory to mp's cache (or the shared lists).
func fgoRCFree(mp *m, p unsafe.Pointer) {
	h := fgoHdrOf(p)
	c := fgoRCClass(fgoHdr + h.typ.Size_)
	h.typ = nil
	fgoRCFreeClass(mp, unsafe.Pointer(h), c)
}

// fgoRCSpare moves rc's free list of class c onto the spare batches.
//
//go:noinline
func fgoRCSpare(rc *fgoRCCache, c uintptr) {
	lock(&fgoRC.lock)
	if fgoRC.spare[c] == nil {
		fgoRC.spare[c] = (*[fgoRCSpares]fgoRCBatch)(persistentalloc(unsafe.Sizeof([fgoRCSpares]fgoRCBatch{}), goarch.PtrSize, &memstats.other_sys))
	}
	if n := fgoRC.nspare[c]; n < fgoRCSpares {
		fgoRC.spare[c][n] = fgoRCBatch{rc.free[c], rc.tail[c], rc.nfree[c]}
		atomic.Store(&fgoRC.nspare[c], n+1)
	} else {
		top := &fgoRC.spare[c][n-1]
		(*fgoRCHeader)(unsafe.Pointer(rc.tail[c])).rc = top.head
		top.head = rc.free[c]
		top.n += rc.nfree[c]
	}
	unlock(&fgoRC.lock)
	rc.free[c], rc.tail[c], rc.nfree[c] = 0, 0, 0
}

// fgoRCFreeClass frees block b of class c.
func fgoRCFreeClass(mp *m, b unsafe.Pointer, c uintptr) {
	h := (*fgoRCHeader)(b)
	if c <= fgoRCCached {
		rc := fgoRCCacheOf(mp)
		if h.rc = rc.free[c]; h.rc == 0 {
			rc.tail[c] = uintptr(b)
		}
		rc.free[c] = uintptr(b)
		rc.live--
		if rc.nfree[c]++; rc.nfree[c] > rc.keep[c] {
			fgoRCSpare(rc, c)
		}
		return
	}
	lock(&fgoRC.lock)
	h.rc = fgoRC.free[c]
	fgoRC.free[c] = uintptr(b)
	fgoRC.live--
	unlock(&fgoRC.lock)
}

// fgoRCNew allocates a zeroed nogc object of type typ with count 1.
func fgoRCNew(typ *_type) unsafe.Pointer {
	return fgoRCAllocZ(typ, true)
}

// fgoRCInc and fgoRCDec count a reference to p, which may be any pointer:
// only nogc objects are counted.
func fgoRCInc(p unsafe.Pointer) {
	s := fgoQuick(p)
	if s == fgoCounted {
		fgoHdrOf(p).rc++
		return
	}
	if s == fgoMaybe {
		fgoRCIncSlow(p)
	}
}

//go:noinline
func fgoRCIncSlow(p unsafe.Pointer) {
	switch fgoState(p) {
	case fgoCounted:
		fgoHdrOf(p).rc++
	case fgoHanded:
		atomic.Xadduintptr(&fgoHdrOf(p).rc, 1)
		if debug.fgoesc > 3 {
			println("fgoesc: inc", p, "count", fgoHdrOf(p).rc&^fgoHeld)
		}
	}
}

func fgoRCDec(p unsafe.Pointer) {
	s := fgoQuick(p)
	if s == fgoCounted {
		h := fgoHdrOf(p)
		if h.rc--; h.rc == 0 {
			fgoRCRelease(p, true)
		}
		return
	}
	if s == fgoMaybe {
		fgoRCDecSlow(p)
	}
}

//go:noinline
func fgoRCDecSlow(p unsafe.Pointer) {
	switch fgoState(p) {
	case fgoCounted:
		h := fgoHdrOf(p)
		if h.rc--; h.rc == 0 {
			fgoRCRelease(p, true)
		}
	case fgoHanded:
		atomic.Xadduintptr(&fgoHdrOf(p).rc, ^uintptr(0))
		if debug.fgoesc > 3 {
			println("fgoesc: dec", p, "count", fgoHdrOf(p).rc&^fgoHeld)
		}
	}
}

// fgoUnique reports whether the caller holds the only reference to p's
// object, a counted one, so that nothing else can change it.
//
// Callers write it out, with fgoQuick's result, so that the common case
// inlines.
func fgoUnique(p unsafe.Pointer) bool {
	return fgoUniqueQuick(p) || fgoQuick(p) == fgoMaybe && fgoUniqueSlow(p)
}

// fgoUniqueQuick is fgoUnique for the common case, a pointer into a chunk
// that holds no handed-off objects; it is false otherwise.
func fgoUniqueQuick(p unsafe.Pointer) bool {
	return fgoQuick(p) == fgoCounted && fgoHdrOf(p).rc == 1
}

// fgoUniqueSlow is fgoUnique for a pointer into a chunk that holds
// handed-off objects.
//
//go:noinline
func fgoUniqueSlow(p unsafe.Pointer) bool {
	return fgoStateSlow(uintptr(p)) == fgoCounted && fgoHdrOf(p).rc == 1
}

// fgoRCDecKeep is fgoRCDec for a caller that has just loaded the nogc
// pointer fields of p at the word offsets set in keep and keeps them:
// when p goes, their references pass to the caller instead of being
// dropped, and otherwise they are counted first. (Perceus calls this
// drop specialization.)
func fgoRCDecKeep(p unsafe.Pointer, keep uintptr) {
	if s := fgoQuick(p); s == fgoCounted && fgoHdrOf(p).rc == 1 || s == fgoMaybe && fgoUniqueSlow(p) {
		fgoTakeFields(p, keep)
		fgoRCRelease(p, true)
		return
	}
	fgoIncFields(p, keep)
	fgoRCDec(p)
}

// fgoTakeFields clears p's words set in keep, whose references the
// caller takes over from p, which is going.
func fgoTakeFields(p unsafe.Pointer, keep uintptr) {
	for ; keep != 0; keep &= keep - 1 {
		*(*uintptr)(add(p, uintptr(sys.TrailingZeros64(uint64(keep)))*goarch.PtrSize)) = 0
	}
}

// fgoIncFields counts the nogc pointers in p's words set in keep.
func fgoIncFields(p unsafe.Pointer, keep uintptr) {
	if fgoState(p) != fgoCounted {
		for ; keep != 0; keep &= keep - 1 {
			fgoRCInc(*(*unsafe.Pointer)(add(p, uintptr(sys.TrailingZeros64(uint64(keep)))*goarch.PtrSize)))
		}
		return
	}
	for ; keep != 0; keep &= keep - 1 {
		fgoIncObj(*(*unsafe.Pointer)(add(p, uintptr(sys.TrailingZeros64(uint64(keep)))*goarch.PtrSize)))
	}
}

// fgoIncObj counts a reference to c, a nogc object or nil.
func fgoIncObj(c unsafe.Pointer) {
	if c == nil {
		return
	}
	if h := fgoHdrOf(c); h.rc&fgoHeld == 0 {
		h.rc++
	} else {
		atomic.Xadduintptr(&h.rc, 1)
	}
}

// fgoDecChild drops a counted object's reference to c, threading c onto
// pending (through its rc word) if that was the last one. A counted
// object's nogc pointer fields hold only nogc objects (or nil), so c
// needs no checking.
func fgoDecChild(c unsafe.Pointer, pending uintptr) uintptr {
	if c == nil {
		return pending
	}
	ch := fgoHdrOf(c)
	if ch.rc&fgoHeld != 0 {
		atomic.Xadduintptr(&ch.rc, ^uintptr(0))
		return pending
	}
	if ch.rc--; ch.rc == 0 {
		ch.rc = pending
		pending = uintptr(c)
	}
	return pending
}

// fgoRCRelease drops the children of p, a counted object whose count has
// reached zero, and frees p if free is set. It runs iteratively,
// threading objects whose counts also reach zero through their (now
// unused) rc words, so dropping a long list does not grow the stack.
func fgoRCRelease(p unsafe.Pointer, free bool) {
	mp := acquirem()
	var pending uintptr
	var d *fgoRCDesc
	for {
		h := fgoHdrOf(p)
		if typ := h.typ; typ.PtrBytes != 0 {
			if d == nil || d.typ != typ {
				d = fgoRCDescOf(mp, typ)
			}
			for _, off := range d.offs[:d.n] {
				if off&3 == 0 {
					pending = fgoDecChild(*(*unsafe.Pointer)(add(p, off)), pending)
					continue
				}
				pending = fgoReleaseBuf(mp, add(p, off&^3), off&2 != 0, pending)
			}
		}
		if free {
			if d != nil && d.typ == h.typ && d.mixed {
				fgoRCFreeMixed(mp, p)
			} else {
				fgoRCFree(mp, p)
			}
		}
		free = true
		if pending == 0 {
			releasem(mp)
			return
		}
		p = unsafe.Pointer(pending)
		pending = fgoHdrOf(p).rc
	}
}

// fgoReleaseBuf frees the buffer of the string or slice field at f,
// first dropping the references it holds if ptrs is set. Elements whose
// counts reach zero are threaded onto pending, as in fgoRCRelease.
//
//go:noinline
func fgoReleaseBuf(mp *m, f unsafe.Pointer, ptrs bool, pending uintptr) uintptr {
	s := (*slice)(f) // a string's header starts the same way
	if s.len == 0 {
		return pending
	}
	if ptrs {
		for _, c := range unsafe.Slice((*unsafe.Pointer)(s.array), s.len) {
			pending = fgoDecChild(c, pending)
		}
	}
	fgoBufFree(mp, s.array)
	return pending
}

// Mixed objects, of types with fields holding GC managed values, live in
// buffers with two extra words before the header, linking every live one
// into fgoMixed. The garbage collector cannot see nogc memory, so it
// scans the objects on that list as roots (fgoMarkMixed), and an object
// freed while it is marking first shades the values it held, as the
// deletion half of the write barrier would.
type fgoMixedLink struct {
	prev, next unsafe.Pointer // the neighbouring live objects
}

var fgoMixed struct {
	lock mutex
	head unsafe.Pointer
}

func fgoMixedLinkOf(p unsafe.Pointer) *fgoMixedLink {
	return (*fgoMixedLink)(unsafe.Pointer(uintptr(p) - fgoHdr - unsafe.Sizeof(fgoMixedLink{})))
}

func fgoRCIsMixed(typ *_type) bool {
	if typ.PtrBytes == 0 {
		return false
	}
	mp := acquirem()
	d := fgoRCDescOf(mp, typ)
	releasem(mp)
	return d.mixed
}

// fgoRCNewMixed is fgoRCNew for a mixed type.
func fgoRCNewMixed(typ *_type) unsafe.Pointer {
	const pre = unsafe.Sizeof(fgoMixedLink{}) + fgoHdr
	b := fgoBufAlloc(pre + typ.Size_)
	memclrNoHeapPointers(b, pre+typ.Size_)
	p := add(b, pre)
	h := fgoHdrOf(p)
	h.rc = 1
	h.typ = typ
	mp := acquirem()
	if d := fgoRCDescOf(mp, typ); !d.mixed {
		d.mixed = true
	}
	releasem(mp)
	l := (*fgoMixedLink)(b)
	lock(&fgoMixed.lock)
	l.next = fgoMixed.head
	if l.next != nil {
		fgoMixedLinkOf(l.next).prev = p
	}
	fgoMixed.head = p
	unlock(&fgoMixed.lock)
	return p
}

// fgoRCFreeMixed unlinks mixed object p and frees its memory.
func fgoRCFreeMixed(mp *m, p unsafe.Pointer) {
	h := fgoHdrOf(p)
	if writeBarrier.enabled {
		bulkBarrierBitmap(uintptr(p), 0, h.typ.PtrBytes, 0, getGCMask(h.typ))
	}
	l := fgoMixedLinkOf(p)
	b := unsafe.Pointer(l)
	lock(&fgoMixed.lock)
	if l.prev != nil {
		fgoMixedLinkOf(l.prev).next = l.next
	} else {
		fgoMixed.head = l.next
	}
	if l.next != nil {
		fgoMixedLinkOf(l.next).prev = l.prev
	}
	unlock(&fgoMixed.lock)
	h.typ = nil
	fgoBufFree(mp, b)
}

// fgoMarkMixed scans the live mixed objects, and the handed-off objects
// that counted references keep alive, as GC roots.
func fgoMarkMixed(gcw *gcWork) {
	lock(&fgoMixed.lock)
	for p := fgoMixed.head; p != nil; p = fgoMixedLinkOf(p).next {
		typ := fgoHdrOf(p).typ
		scanblock(uintptr(p), typ.PtrBytes, getGCMask(typ), gcw, nil)
	}
	unlock(&fgoMixed.lock)
	fgoMarkCounted(gcw)
}

// fgoBulkBarrier runs the write barriers for a typed copy into (or
// clear of) memory outside the Go heap that is not a global: nogc
// objects. Upstream skips these, as such memory can only be a stack.
func fgoBulkBarrier(dst, src, size uintptr, typ *_type) {
	if typ == nil || typ.PtrBytes == 0 {
		return
	}
	mask := getGCMask(typ)
	for off := uintptr(0); off < size; off += typ.Size_ {
		s := uintptr(0)
		if src != 0 {
			s = src + off
		}
		bulkBarrierBitmap(dst+off, s, min(typ.PtrBytes, size-off), 0, mask)
	}
}

// fgoArena holds the memory of every nogc object, so that the GC can
// tell a pointer to one by its address alone, and the state of each,
// one byte per fgoRCAlign bytes (see fgoState).
//
// The compiler loads lo, size and chunks, at offsets 0, 8 and 16, for
// its inline checks (see ssa's fgoRCPass.vetIf).
var fgoArena = struct {
	lo, size uintptr // size: of the part mapped so far
	chunks   uintptr // one uint32 per fgoRCChunk bytes from lo: how many handed-off objects it holds
	state    uintptr // one byte per fgoRCAlign bytes from lo: a handed-off object starts here
	next     uintptr
	used     uintptr
	lock     mutex
}{lo: fgoNoArena, chunks: uintptr(unsafe.Pointer(&fgoNoChunks))}

// fgoNoChunks is fgoArena.chunks before the arena is reserved: the inline
// checks read its first entry for pointers outside the arena.
var fgoNoChunks [1]uint32

// fgoNoArena is fgoArena.lo before the arena is reserved: no pointer is
// within fgoArenaSize above it.
const fgoNoArena = 1 << (goarch.PtrSize*8 - 1)

const fgoArenaStep = 4 << 20 // mapped at a time

// fgoArenaSize is the address space reserved for nogc objects: 64 GiB, or
// 512 MiB on 32-bit systems. (ssa's inline checks assume 64 GiB, and run
// only on 64-bit systems.)
const fgoArenaSize = goarch.PtrSize/8*(64<<30) + (1-goarch.PtrSize/8)*(512<<20)

func fgoArenaAlloc(n uintptr) unsafe.Pointer {
	lock(&fgoArena.lock)
	if fgoArena.lo == fgoNoArena {
		v := sysReserve(nil, fgoArenaSize, "nogc")
		s := sysReserve(nil, fgoArenaSize/fgoRCAlign, "nogc")
		c := sysAlloc(fgoArenaSize/fgoRCChunk*4, &memstats.other_sys, "nogc")
		if v == nil || s == nil || c == nil {
			throw("nogc: cannot reserve address space")
		}
		fgoArena.next = uintptr(v)
		fgoArena.state = uintptr(s)
		fgoArena.chunks = uintptr(c)
		atomic.Storeuintptr(&fgoArena.lo, uintptr(v))
	}
	if fgoArena.next+n > fgoArena.lo+fgoArenaSize {
		throw("nogc: out of address space")
	}
	v := unsafe.Pointer(fgoArena.next)
	fgoArena.next += n
	if mapped := fgoArena.lo + fgoArena.size; fgoArena.next > mapped {
		m := alignUp(fgoArena.next-mapped, fgoArenaStep)
		sysMap(unsafe.Pointer(mapped), m, &memstats.other_sys, "nogc")
		sysUsed(unsafe.Pointer(mapped), m, m)
		s := unsafe.Pointer(fgoArena.state + fgoArena.size/fgoRCAlign)
		sysMap(s, m/fgoRCAlign, &memstats.other_sys, "nogc")
		sysUsed(s, m/fgoRCAlign, m/fgoRCAlign)
		atomic.Storeuintptr(&fgoArena.size, fgoArena.size+m)
	}
	// While the GC holds nogc objects, their memory counts toward when
	// it should run again: it cannot see it otherwise.
	grown := fgoArena.next - fgoArena.lo
	trigger := fgoEsc.started && grown > fgoArena.used+max(fgoArena.used/4, 4<<20)
	if trigger {
		fgoArena.used = grown
	}
	unlock(&fgoArena.lock)
	if trigger {
		fgoWantGC = true // the allocator starts it once it lets go of its M
	}
	return v
}

var fgoWantGC bool

//go:noinline
func fgoStartGC() {
	fgoWantGC = false
	if gcphase == _GCoff {
		gcStart(gcTrigger{kind: gcTriggerCycle, n: work.cycles.Load() + 1})
	}
}

// Handing objects to the GC.
//
// A nogc object reaching code that does not count it — stored in an
// interface, a map, a slice, a global, a closure, a channel, a GC
// object's field, or passed to a function that is not nogc code and may
// keep or change it — is handed off to the garbage collector first
// (fgoRCHandoff). That gives it fgoHeld, a reference the GC holds,
// records it in fgoEsc, and turns its fields into ordinary GC values:
// the objects it points to are handed off too, and its strings and
// slices move onto the GC heap. Nogc code still counts its references to
// handed-off objects (atomically, as other goroutines may hold them),
// and the GC treats those with any as roots (fgoMarkCounted). Otherwise
// it traces them like heap objects: fgoEscMark, called for every pointer
// the GC finds outside the Go heap, stamps the object it points into with
// the cycle number and scans it. After each cycle, fgoEscSweeper frees
// every handed-off object the cycle did not reach and nothing counts.

// fgoHeld marks the count of a handed-off object.
const fgoHeld = uintptr(1) << (goarch.PtrSize*8 - 1)

type fgoEscRec struct {
	obj  uintptr
	mark uint32 // last GC cycle that reached obj (or that it was handed off in)
	next *fgoEscRec
	scan uintptr         // *fgoEscRec: next record an M has yet to scan (see fgoEscReach)
	weak *atomic.Uintptr // weak handle to obj, cleared when it is freed
}

// fgoEscTab is an open-addressing set of records keyed by object. The
// GC reads it without locking; updates hold fgoEsc.lock, and growing it
// publishes a new table (records stay where they are).
type fgoEscTab struct {
	mask  uintptr
	used  uintptr                                    // live and deleted slots
	slots [1 << (26 + goarch.PtrSize/8*4)]*fgoEscRec // really mask+1 (the bound fits 32-bit systems)
}

var fgoEscTomb fgoEscRec

var fgoEsc struct {
	lock    mutex
	tab     unsafe.Pointer // *fgoEscTab
	n       uintptr        // live records
	added   uintptr        // records added since the last sweep
	kept    uintptr        // live records after the last sweep
	free    *fgoEscRec
	g       *g // the sweeper
	started bool
	parked  bool
	target  uint32 // last cycle to sweep for
	swept   uint32
}

func fgoEscHash(p uintptr) uintptr { return uintptr(uint64(p>>4) * 0x9E3779B97F4A7C15 >> 7) }

func fgoEscFind(t *fgoEscTab, p uintptr) *fgoEscRec {
	if t == nil {
		return nil
	}
	for i := fgoEscHash(p) & t.mask; ; i = (i + 1) & t.mask {
		r := (*fgoEscRec)(atomic.Loadp(unsafe.Pointer(&t.slots[i])))
		if r == nil {
			return nil
		}
		if r != &fgoEscTomb && r.obj == p {
			return r
		}
	}
}

func fgoEscNewTab(n uintptr) *fgoEscTab {
	t := (*fgoEscTab)(persistentalloc(2*goarch.PtrSize+n*goarch.PtrSize, goarch.PtrSize, &memstats.other_sys))
	t.mask = n - 1
	return t
}

// fgoEscInsert adds a record for p. Called with fgoEsc.lock held.
func fgoEscInsert(p uintptr, cyc uint32) {
	t := (*fgoEscTab)(fgoEsc.tab)
	if t == nil || (t.used+1)*2 > t.mask+1 {
		n := uintptr(1024)
		if t != nil {
			n = t.mask + 1
			if (fgoEsc.n+1)*4 > n {
				n *= 2
			}
		}
		nt := fgoEscNewTab(n)
		if t != nil {
			for _, r := range t.slots[:t.mask+1] {
				if r != nil && r != &fgoEscTomb {
					i := fgoEscHash(r.obj) & nt.mask
					for nt.slots[i] != nil {
						i = (i + 1) & nt.mask
					}
					nt.slots[i] = r
					nt.used++
				}
			}
		}
		atomic.StorepNoWB(unsafe.Pointer(&fgoEsc.tab), unsafe.Pointer(nt))
		t = nt
	}
	r := fgoEsc.free
	if r != nil {
		fgoEsc.free = r.next
	} else {
		r = (*fgoEscRec)(persistentalloc(unsafe.Sizeof(fgoEscRec{}), goarch.PtrSize, &memstats.other_sys))
	}
	r.obj, r.next, r.scan, r.weak = p, nil, 0, nil
	atomic.Store(&r.mark, cyc)
	i := fgoEscHash(p) & t.mask
	for t.slots[i] != nil && t.slots[i] != &fgoEscTomb {
		i = (i + 1) & t.mask
	}
	if t.slots[i] == nil {
		t.used++
	}
	atomic.StorepNoWB(unsafe.Pointer(&t.slots[i]), unsafe.Pointer(r))
	fgoEsc.n++
}

// fgoRCHandoff hands p's object, if it is a counted one, and every
// counted object it reaches to the garbage collector.
func fgoRCHandoff(p unsafe.Pointer) {
	if fgoState(p) != fgoCounted {
		return
	}
	if !fgoEsc.started {
		fgoEscStart()
	}
	fgoHandOne(p)
	work := []unsafe.Pointer{p}
	hand := func(c unsafe.Pointer) {
		if fgoState(c) == fgoCounted {
			fgoHandOne(c)
			work = append(work, c)
		}
	}
	for len(work) > 0 {
		x := work[len(work)-1]
		work = work[:len(work)-1]
		typ := fgoHdrOf(x).typ
		if typ.PtrBytes == 0 {
			continue
		}
		mp := acquirem()
		d := fgoRCDescOf(mp, typ)
		releasem(mp)
		for _, off := range d.offs[:d.n] {
			f := add(x, off&^3)
			switch {
			case off&3 == 0:
				c := *(*unsafe.Pointer)(f)
				hand(c)
				fgoRCDec(c) // x no longer counts it
			case off&2 != 0:
				s := *(*[]unsafe.Pointer)(f)
				if len(s) == 0 {
					continue
				}
				gs := make([]unsafe.Pointer, len(s))
				for i, c := range s {
					hand(c)
					gs[i] = c
				}
				*(*[]unsafe.Pointer)(f) = gs
				for _, c := range s {
					fgoRCDec(c)
				}
				fgoHandBuf(unsafe.Pointer(unsafe.SliceData(s)), fgoBufPtrs)
			default:
				s := (*slice)(f)
				if s.len == 0 {
					continue
				}
				old := s.array
				if fgoFieldIsString(typ, off&^3) {
					*(*string)(f) = fgoStrCopy(*(*string)(f))
				} else {
					*s = fgoSliceCopy(*s, fgoFieldElemSize(typ, off&^3))
				}
				fgoHandBuf(old, fgoBufData)
			}
		}
	}
}

// fgoHandBuf hands buffer data, which a field of an object being handed
// off held until now, to the GC: nogc code may still hold a view into it
// (it held the object), so it goes when the GC no longer finds one.
func fgoHandBuf(data unsafe.Pointer, kind uint8) {
	if data == unsafe.Pointer(&zerobase) || uintptr(data)-fgoArena.lo >= fgoArenaSize {
		return // empty, or a large buffer of its own, which stays (a leak, but views stay valid)
	}
	lock(&fgoEsc.lock)
	fgoMarkHanded(data, kind)
	fgoEscInsert(uintptr(data), work.cycles.Load())
	unlock(&fgoEsc.lock)
}

// fgoBufFreeM is fgoBufFree on the current M.
func fgoBufFreeM(data unsafe.Pointer) {
	mp := acquirem()
	fgoBufFree(mp, data)
	releasem(mp)
}

// fgoHandOne hands counted object p to the GC, without its fields.
func fgoHandOne(p unsafe.Pointer) {
	h := fgoHdrOf(p)
	lock(&fgoEsc.lock)
	if debug.fgoesc > 1 {
		println("fgoesc: handoff", p, "rc", h.rc)
		if debug.fgoesc == 3 {
			unlock(&fgoEsc.lock)
			throw("fgoesc: stopping at the first handoff")
		}
	}
	atomic.Storeuintptr(&h.rc, h.rc|fgoHeld)
	fgoMarkHanded(p, fgoHanded)
	fgoEscInsert(uintptr(p), work.cycles.Load())
	fgoEsc.added++
	// The GC paces itself by its own heap, which does not count nogc
	// memory: start a cycle when handed-off objects pile up.
	trigger := fgoEsc.added > max(4096, fgoEsc.kept)
	if trigger {
		fgoEsc.added = 0
	}
	unlock(&fgoEsc.lock)
	if trigger {
		fgoWantGC = true
	}
}

// fgoFieldIsString and fgoFieldElemSize describe the string or slice
// field of typ at off.
func fgoFieldIsString(typ *_type, off uintptr) bool {
	for _, f := range (*structtype)(unsafe.Pointer(typ)).Fields {
		if f.Offset == off {
			return f.Typ.Kind() == abi.String
		}
	}
	return false
}

func fgoFieldElemSize(typ *_type, off uintptr) uintptr {
	for _, f := range (*structtype)(unsafe.Pointer(typ)).Fields {
		if f.Offset == off {
			return (*slicetype)(unsafe.Pointer(f.Typ)).Elem.Size_
		}
	}
	return 1
}

// fgoGCPtr reports whether p must not be counted where a counted object
// keeps it: it is not nil, nor a nogc object, nor memory the GC never
// frees (globals, zerobase).
func fgoGCPtr(p unsafe.Pointer) bool {
	if p == nil || fgoState(p) != fgoNone {
		return false
	}
	return uintptr(p)-fgoArena.lo < fgoArena.size || spanOfHeap(uintptr(p)) != nil
}

// fgoRCStore stores v, a reference the caller hands over, into the nogc
// pointer field at slot of the object at base, releasing the reference
// the field held. A counted object takes v as a reference, unless v is
// a pointer it cannot count; anything else stores v as a GC value.
func fgoRCStore(base unsafe.Pointer, slot *unsafe.Pointer, v unsafe.Pointer) {
	if fgoState(base) == fgoCounted {
		if !fgoGCPtr(v) {
			old := *slot
			*(*uintptr)(unsafe.Pointer(slot)) = uintptr(v)
			fgoRCDec(old)
			return
		}
		fgoRCHandoff(base)
	}
	fgoRCHandoff(v)
	*slot = v
	fgoRCDec(v)
}

// fgoRCStoreGC is fgoRCStore for a field through which base's object can
// reach itself again: the object is handed off to the GC first, so that
// a cycle it may form is the GC's to collect.
func fgoRCStoreGC(base unsafe.Pointer, slot *unsafe.Pointer, v unsafe.Pointer) {
	fgoRCHandoff(base)
	fgoRCHandoff(v)
	*slot = v
	fgoRCDec(v)
}

// fgoRCVet checks the nogc pointer fields in keep (a mask of words, as in
// fgoRCDecKeep) of p, a new object that was just filled in, and hands p
// off if any holds a pointer it cannot count.
func fgoRCVet(p unsafe.Pointer, keep uintptr) {
	for k := keep; k != 0; k &= k - 1 {
		if fgoGCPtr(*(*unsafe.Pointer)(add(p, uintptr(sys.TrailingZeros64(uint64(k)))*goarch.PtrSize))) {
			fgoRCHandoff(p)
			return
		}
	}
}

// fgoEscWait waits until the sweep for cycle cyc is done.
func fgoEscWait(cyc uint32) {
	if !fgoEsc.started {
		return
	}
	for {
		lock(&fgoEsc.lock)
		done := int32(fgoEsc.swept-cyc) >= 0 || int32(fgoEsc.target-cyc) < 0
		unlock(&fgoEsc.lock)
		if done {
			return
		}
		Gosched()
	}
}

//go:noinline
func fgoEscStart() {
	lock(&fgoEsc.lock)
	start := !fgoEsc.started
	fgoEsc.started = true
	unlock(&fgoEsc.lock)
	if start {
		go fgoEscSweeper()
	}
}

// fgoHandedObj returns the handed-off object p points into, or 0.
func fgoHandedObj(p uintptr) uintptr {
	d := p - fgoArena.lo
	if d >= fgoArena.size {
		return 0
	}
	if atomic.Load((*uint32)(unsafe.Pointer(fgoArena.chunks+d/fgoRCChunk*4))) == 0 {
		return 0
	}
	g := d / fgoRCAlign
	first := d / fgoRCChunk * (fgoRCChunk / fgoRCAlign) // objects do not cross chunks
	for i := g; i+1 > first; i-- {
		k := atomic.Load8((*uint8)(unsafe.Pointer(fgoArena.state + i)))
		if k == fgoNone || k == fgoCounted {
			continue
		}
		obj := fgoArena.lo + i*fgoRCAlign
		if p < obj+fgoHandedSize(obj, k) || p == obj {
			return obj
		}
		return 0 // past the closest handed-off object
	}
	return 0
}

// fgoKindOf returns the state recorded for handed-off p.
func fgoKindOf(p uintptr) uint8 {
	return atomic.Load8((*uint8)(unsafe.Pointer(fgoArena.state + (p-fgoArena.lo)/fgoRCAlign)))
}

// fgoHandedSize returns the size of handed-off object (or buffer) obj of
// kind k.
func fgoHandedSize(obj uintptr, k uint8) uintptr {
	if k == fgoHanded {
		if typ := fgoHdrOf(unsafe.Pointer(obj)).typ; typ != nil {
			return typ.Size_
		}
		return 0
	}
	return (*fgoBufHeader)(unsafe.Pointer(obj-unsafe.Sizeof(fgoBufHeader{}))).size - unsafe.Sizeof(fgoBufHeader{})
}

// fgoEscMark is called by the GC for each pointer it finds while marking.
//
//go:nowritebarrierrec
func fgoEscMark(p uintptr, gcw *gcWork) {
	if p-fgoArena.lo >= fgoArena.size || !fgoEsc.started {
		return
	}
	obj := fgoHandedObj(p)
	if obj == 0 {
		return
	}
	if r := fgoEscFind((*fgoEscTab)(atomic.Loadp(unsafe.Pointer(&fgoEsc.tab))), obj); r != nil {
		fgoEscReach(r, gcw)
	}
}

// fgoEscReach stamps r with the current cycle and scans its object, the
// first time this cycle reaches it.
//
//go:nowritebarrierrec
func fgoEscReach(r *fgoEscRec, gcw *gcWork) {
	cyc := work.cycles.Load()
	for {
		old := atomic.Load(&r.mark)
		if old == cyc {
			return
		}
		if atomic.Cas(&r.mark, old, cyc) {
			break
		}
	}
	if gcw == nil {
		return
	}
	// Scanning finds more pointers, which come back here: queue them on
	// the M rather than recursing.
	mp := getg().m
	if mp.fgoScanning {
		r.scan = mp.fgoScanQ
		mp.fgoScanQ = uintptr(unsafe.Pointer(r))
		return
	}
	mp.fgoScanning = true
	for r != nil {
		switch atomic.Load8((*uint8)(unsafe.Pointer(fgoArena.state + (r.obj-fgoArena.lo)/fgoRCAlign))) {
		case fgoHanded:
			if typ := fgoHdrOf(unsafe.Pointer(r.obj)).typ; typ != nil && typ.PtrBytes != 0 {
				scanblock(r.obj, typ.PtrBytes, getGCMask(typ), gcw, nil)
			}
		case fgoBufPtrs:
			n := fgoHandedSize(r.obj, fgoBufPtrs) / goarch.PtrSize
			for i := uintptr(0); i < n; i += 8 * goarch.PtrSize {
				scanblock(r.obj+i*goarch.PtrSize, min(n-i, 8*goarch.PtrSize)*goarch.PtrSize, &fgoAllPtrs[0], gcw, nil)
			}
		}
		r = (*fgoEscRec)(unsafe.Pointer(mp.fgoScanQ))
		if r != nil {
			mp.fgoScanQ = r.scan
		}
	}
	mp.fgoScanning = false
}

// fgoAllPtrs is a pointer mask with every word a pointer.
var fgoAllPtrs = [8]uint8{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// fgoMarkCounted marks and scans the handed-off objects that nogc code
// still counts: their counted holders are invisible to the GC.
func fgoMarkCounted(gcw *gcWork) {
	if !fgoEsc.started {
		return
	}
	lock(&fgoEsc.lock)
	if t := (*fgoEscTab)(fgoEsc.tab); t != nil {
		for _, r := range t.slots[:t.mask+1] {
			if r != nil && r != &fgoEscTomb && fgoKindOf(r.obj) == fgoHanded && atomic.Loaduintptr(&fgoHdrOf(unsafe.Pointer(r.obj)).rc) != fgoHeld {
				fgoEscReach(r, gcw)
			}
		}
	}
	unlock(&fgoEsc.lock)
}

// fgoEscWake starts the sweep for cycle cyc, whose marking just ended.
// The world is stopped.
func fgoEscWake(cyc uint32) {
	if !fgoEsc.started {
		return
	}
	lock(&fgoEsc.lock)
	fgoEsc.target = cyc
	if fgoEsc.parked {
		fgoEsc.parked = false
		ready(fgoEsc.g, 0, true)
	}
	unlock(&fgoEsc.lock)
}

// fgoEscSweeper frees the handed-off objects that the last GC cycle did
// not reach and that nogc code no longer counts.
func fgoEscSweeper() {
	lock(&fgoEsc.lock)
	fgoEsc.g = getg()
	for {
		for fgoEsc.swept == fgoEsc.target {
			fgoEsc.parked = true
			goparkunlock(&fgoEsc.lock, waitReasonGCSweepWait, traceBlockGCSweep, 1)
			lock(&fgoEsc.lock)
		}
		cyc := fgoEsc.target
		var dropped *fgoEscRec
		if t := (*fgoEscTab)(fgoEsc.tab); t != nil {
			for i := range t.slots[:t.mask+1] {
				r := t.slots[i]
				if r == nil || r == &fgoEscTomb {
					continue
				}
				if int32(cyc-atomic.Load(&r.mark)) <= 0 {
					if debug.fgoesc > 1 {
						println("fgoesc: keep", unsafe.Pointer(r.obj), "reached, count", fgoHdrOf(unsafe.Pointer(r.obj)).rc&^fgoHeld)
					}
					continue
				}
				if fgoKindOf(r.obj) == fgoHanded && !atomic.Casuintptr(&fgoHdrOf(unsafe.Pointer(r.obj)).rc, fgoHeld, 0) {
					continue // still counted: the next cycle will see it as a root
				}
				atomic.StorepNoWB(unsafe.Pointer(&t.slots[i]), unsafe.Pointer(&fgoEscTomb))
				fgoEsc.n--
				if r.weak != nil {
					r.weak.Store(0)
				}
				r.next = dropped
				dropped = r
			}
			if fgoEsc.n*8 < t.used && t.used > 1024 {
				// Mostly deleted slots: rebuild, so lookups stay short.
				fgoEsc.tab = nil
				nt := fgoEscNewTab(t.mask + 1)
				for _, r := range t.slots[:t.mask+1] {
					if r != nil && r != &fgoEscTomb {
						i := fgoEscHash(r.obj) & nt.mask
						for nt.slots[i] != nil {
							i = (i + 1) & nt.mask
						}
						nt.slots[i] = r
						nt.used++
					}
				}
				atomic.StorepNoWB(unsafe.Pointer(&fgoEsc.tab), unsafe.Pointer(nt))
			}
		}
		fgoEsc.kept = fgoEsc.n
		if debug.fgoesc > 0 {
			nd := 0
			for r := dropped; r != nil; r = r.next {
				nd++
			}
			println("fgoesc: cycle", cyc, "freed", nd, "held", fgoEsc.n)
		}
		unlock(&fgoEsc.lock)
		var last *fgoEscRec
		mp := acquirem()
		for r := dropped; r != nil; r = r.next {
			p := unsafe.Pointer(r.obj)
			if debug.fgoesc > 1 {
				println("fgoesc: free", p)
			}
			k := fgoKindOf(r.obj)
			fgoMarkHanded(p, fgoNone)
			switch {
			case k != fgoHanded:
				fgoBufFree(mp, p)
			case fgoRCIsMixed(fgoHdrOf(p).typ):
				fgoRCFreeMixed(mp, p)
			default:
				fgoRCFree(mp, p)
			}
			last = r
		}
		releasem(mp)
		lock(&fgoEsc.lock)
		if last != nil {
			last.next = fgoEsc.free
			fgoEsc.free = dropped
		}
		fgoEsc.swept = cyc
	}
}

// fgoRCDropReuse drops a reference to p like fgoRCDec, but when that was
// the last reference it keeps p's memory and returns it as a reuse token
// for a following fgoRCNewAt. Otherwise it returns nil.
func fgoRCDropReuse(p unsafe.Pointer) unsafe.Pointer {
	if fgoState(p) != fgoCounted {
		fgoRCDec(p)
		return nil
	}
	h := fgoHdrOf(p)
	if h.rc--; h.rc != 0 {
		return nil
	}
	if fgoRCIsMixed(h.typ) {
		fgoRCRelease(p, true) // its memory is not like the others'
		return nil
	}
	fgoRCRelease(p, false)
	return p
}

// fgoRCDropReuseAll and fgoRCDecAll are fgoRCDropReuseKeep and
// fgoRCDecKeep when keep holds every reference p's object has.
func fgoRCDropReuseAll(p unsafe.Pointer, keep uintptr) unsafe.Pointer {
	if s := fgoQuick(p); s == fgoCounted && fgoHdrOf(p).rc == 1 || s == fgoMaybe && fgoUniqueSlow(p) {
		return p
	}
	fgoIncFields(p, keep)
	fgoRCDec(p)
	return nil
}

func fgoRCDecAll(p unsafe.Pointer, keep uintptr) {
	if s := fgoQuick(p); s == fgoCounted && fgoHdrOf(p).rc == 1 || s == fgoMaybe && fgoUniqueSlow(p) {
		mp := acquirem()
		fgoRCFree(mp, p)
		releasem(mp)
		return
	}
	fgoIncFields(p, keep)
	fgoRCDec(p)
}

// fgoRCDropReuseKeep is fgoRCDropReuse for a caller keeping the fields
// in keep, as in fgoRCDecKeep.
func fgoRCDropReuseKeep(p unsafe.Pointer, keep uintptr) unsafe.Pointer {
	if s := fgoQuick(p); !(s == fgoCounted && fgoHdrOf(p).rc == 1 || s == fgoMaybe && fgoUniqueSlow(p)) {
		fgoIncFields(p, keep)
		fgoRCDec(p)
		return nil
	}
	fgoTakeFields(p, keep)
	if fgoRCIsMixed(fgoHdrOf(p).typ) {
		fgoRCRelease(p, true)
		return nil
	}
	fgoRCRelease(p, false)
	return p
}

// fgoRCNewAt is fgoRCNew, reusing token's memory when it is non-nil and
// of the same size class.
func fgoRCNewAt(token unsafe.Pointer, typ *_type) unsafe.Pointer {
	if token == nil {
		return fgoRCAllocZ(typ, true)
	}
	h := fgoHdrOf(token)
	mp := acquirem()
	if fgoRCClass(fgoHdr+h.typ.Size_) != fgoRCClass(fgoHdr+typ.Size_) {
		fgoRCFree(mp, token)
		releasem(mp)
		return fgoRCAllocZ(typ, true)
	}
	fgoRCCacheOf(mp).reuses++
	releasem(mp)
	memclrNoHeapPointers(token, typ.Size_)
	h.rc = 1
	h.typ = typ
	return token
}

// fgoRCNewInit and fgoRCNewAtInit are fgoRCNew and fgoRCNewAt for a
// caller that writes every field of the object right away.
func fgoRCNewInit(typ *_type) unsafe.Pointer {
	return fgoRCAllocZ(typ, false)
}

func fgoRCNewAtInit(token unsafe.Pointer, typ *_type) unsafe.Pointer {
	if token == nil {
		return fgoRCAllocZ(typ, false)
	}
	h := fgoHdrOf(token)
	mp := acquirem()
	if fgoRCClass(fgoHdr+h.typ.Size_) != fgoRCClass(fgoHdr+typ.Size_) {
		fgoRCFree(mp, token)
		releasem(mp)
		return fgoRCAllocZ(typ, false)
	}
	fgoRCCacheOf(mp).reuses++
	releasem(mp)
	h.rc = 1
	h.typ = typ
	return token
}

// Buffers hold the bytes of counted objects' string fields. Each is owned
// by exactly one field (every store copies), so it needs no count: its
// header holds only its size, and it is freed when the field is
// overwritten or its object is released. Buffers share the objects' size
// classes and free lists; larger ones get memory of their own.
type fgoBufHeader struct {
	link uintptr // free-list link while free, like fgoRCHeader.rc
	size uintptr // header included
}

func fgoBufAlloc(n uintptr) unsafe.Pointer {
	size := n + unsafe.Sizeof(fgoBufHeader{})
	var p unsafe.Pointer
	if size > fgoRCMaxSize {
		p = sysAlloc(alignUp(size, physPageSize), &memstats.other_sys, "nogc")
		if p == nil {
			throw("nogc: out of memory")
		}
		lock(&fgoRC.lock)
		fgoRC.live++
		unlock(&fgoRC.lock)
	} else if c := fgoRCClass(size); c <= fgoRCCached {
		mp := acquirem()
		rc := fgoRCCacheOf(mp)
		q, _ := fgoRCTakeCached(rc, c)
		rc.live++
		releasem(mp)
		p = unsafe.Pointer(q)
	} else {
		lock(&fgoRC.lock)
		q, _ := fgoRCTake(fgoRC.free[:], &fgoRC.chunk, &fgoRC.end, c)
		fgoRC.live++
		unlock(&fgoRC.lock)
		p = unsafe.Pointer(q)
	}
	(*fgoBufHeader)(p).size = size
	(*fgoBufHeader)(p).link = 0
	return add(p, unsafe.Sizeof(fgoBufHeader{}))
}

// fgoBufFree frees the buffer whose bytes start at data.
func fgoBufFree(mp *m, data unsafe.Pointer) {
	if data == unsafe.Pointer(&zerobase) {
		return // an empty, non-nil slice
	}
	p := unsafe.Pointer(uintptr(data) - unsafe.Sizeof(fgoBufHeader{}))
	size := (*fgoBufHeader)(p).size
	if size > fgoRCMaxSize {
		sysFree(p, alignUp(size, physPageSize), &memstats.other_sys)
		lock(&fgoRC.lock)
		fgoRC.live--
		unlock(&fgoRC.lock)
		return
	}
	fgoRCFreeClass(mp, p, fgoRCClass(size))
}

// fgoStrSet stores a copy of s into the string field at dst of the
// object at base, freeing the buffer the field held. Only counted
// objects own buffers; others get a copy on the GC heap.
func fgoStrSet(dst *string, s string, base unsafe.Pointer) {
	if fgoState(base) != fgoCounted {
		*dst = fgoStrCopy(s)
		return
	}
	fgoStrPut(dst, fgoStrDup(s), base)
}

// fgoStrDup copies s into a new buffer.
func fgoStrDup(s string) string {
	if len(s) == 0 {
		return ""
	}
	p := fgoBufAlloc(uintptr(len(s)))
	memmove(p, unsafe.Pointer(unsafe.StringData(s)), uintptr(len(s)))
	return unsafe.String((*byte)(p), len(s))
}

// fgoStrPut stores ns, a buffer from fgoStrDup, into the string field at
// dst of the object at base, freeing the buffer the field held.
func fgoStrPut(dst *string, ns string, base unsafe.Pointer) {
	if fgoState(base) != fgoCounted {
		*dst = fgoStrCopy(ns)
		if len(ns) > 0 {
			mp := acquirem()
			fgoBufFree(mp, unsafe.Pointer(unsafe.StringData(ns)))
			releasem(mp)
		}
		return
	}
	old := (*stringStruct)(unsafe.Pointer(dst))
	o, olen := old.str, old.len
	old.str, old.len = unsafe.Pointer(unsafe.StringData(ns)), len(ns)
	if olen > 0 {
		mp := acquirem()
		fgoBufFree(mp, o)
		releasem(mp)
	}
}

// fgoStrClone copies a string read from a counted object onto the GC
// heap, for a use that outlives the frame. Other strings need no copy.
func fgoStrClone(s string) string {
	if uintptr(unsafe.Pointer(unsafe.StringData(s)))-fgoArena.lo >= fgoArena.size {
		return s
	}
	return fgoStrCopy(s)
}

// fgoStrCopy copies s onto the GC heap.
func fgoStrCopy(s string) string {
	if len(s) == 0 {
		return ""
	}
	t, b := rawstring(len(s))
	copy(b, s)
	return t
}

// fgoSliceSet stores a copy of s, whose elements are esize bytes and
// hold no pointers, into the slice field at dst of the object at base,
// freeing the buffer the field held. The copy's capacity is its length,
// so an append to the field always moves it.
func fgoSliceSet(dst *slice, s slice, esize uintptr, base unsafe.Pointer) {
	if fgoState(base) != fgoCounted {
		*dst = fgoSliceCopy(s, esize)
		return
	}
	fgoSlicePut(dst, fgoSliceDup(s, esize), esize, base)
}

// fgoSliceDup copies s into a new buffer.
func fgoSliceDup(s slice, esize uintptr) slice {
	switch {
	case s.array == nil:
		return slice{}
	case s.len == 0:
		return slice{array: unsafe.Pointer(&zerobase)}
	}
	n := uintptr(s.len) * esize
	p := fgoBufAlloc(n)
	memmove(p, s.array, n)
	return slice{array: p, len: s.len, cap: s.len}
}

// fgoSliceCopy copies s onto the GC heap.
func fgoSliceCopy(s slice, esize uintptr) slice {
	if s.len == 0 {
		return slice{array: s.array}
	}
	n := uintptr(s.len) * esize
	p := mallocgc(n, nil, false)
	memmove(p, s.array, n)
	return slice{array: p, len: s.len, cap: s.len}
}

// fgoSlicePut stores ns, a buffer from fgoSliceDup, into the slice field
// at dst of the object at base, freeing the buffer the field held.
func fgoSlicePut(dst *slice, ns slice, esize uintptr, base unsafe.Pointer) {
	if fgoState(base) != fgoCounted {
		*dst = fgoSliceCopy(ns, esize)
		if ns.len > 0 {
			mp := acquirem()
			fgoBufFree(mp, ns.array)
			releasem(mp)
		}
		return
	}
	o, olen := dst.array, dst.len
	*(*uintptr)(unsafe.Pointer(&dst.array)) = uintptr(ns.array)
	dst.len, dst.cap = ns.len, ns.cap
	if olen > 0 {
		mp := acquirem()
		fgoBufFree(mp, o)
		releasem(mp)
	}
}

// fgoSliceClone copies a slice read from a counted object onto the GC
// heap, for a use that outlives the frame. Other slices need no copy.
func fgoSliceClone(s slice, esize uintptr) slice {
	if s.len == 0 || uintptr(s.array)-fgoArena.lo >= fgoArena.size {
		return s
	}
	return fgoSliceCopy(s, esize)
}

// fgoPtrsMake allocates the buffer for a slice of n nogc pointers that
// will be stored into a struct field. Its elements start nil and are
// filled by fgoPtrsAppend1 and fgoPtrsAppendN, each holding a reference.
func fgoPtrsMake(n int) []unsafe.Pointer {
	if n == 0 {
		return unsafe.Slice((*unsafe.Pointer)(unsafe.Pointer(&zerobase)), 0)
	}
	p := fgoBufAlloc(uintptr(n) * goarch.PtrSize)
	memclrNoHeapPointers(p, uintptr(n)*goarch.PtrSize)
	return unsafe.Slice((*unsafe.Pointer)(p), n)
}

// fgoPtrsAppend1 stores a new reference to p at s[i].
func fgoPtrsAppend1(s []unsafe.Pointer, i int, p unsafe.Pointer) {
	switch fgoQuick(p) {
	case fgoCounted:
		fgoHdrOf(p).rc++
	case fgoNone:
		if p != nil {
			fgoPtrsTaint(s)
		}
	default:
		if fgoGCPtr(p) {
			fgoPtrsTaint(s)
		}
		fgoRCInc(p)
	}
	*(*uintptr)(unsafe.Pointer(&s[i])) = uintptr(p)
}

// fgoPtrsAppendN stores new references to src's elements from s[i].
// Elements of a buffer from the arena are nogc objects already.
func fgoPtrsAppendN(s []unsafe.Pointer, i int, src []unsafe.Pointer) {
	if uintptr(unsafe.Pointer(unsafe.SliceData(src)))-fgoArena.lo < fgoArenaSize {
		for j, p := range src {
			fgoIncObj(p)
			*(*uintptr)(unsafe.Pointer(&s[i+j])) = uintptr(p)
		}
		return
	}
	for j, p := range src {
		fgoPtrsAppend1(s, i+j, p)
	}
}

// fgoPtrsTaint records, in the header of buffer s from fgoPtrsMake (its
// link word, unused while it is allocated), that s holds a pointer no
// counted object can count.
func fgoPtrsTaint(s []unsafe.Pointer) {
	if len(s) > 0 && unsafe.Pointer(unsafe.SliceData(s)) != unsafe.Pointer(&zerobase) {
		(*fgoBufHeader)(unsafe.Pointer(uintptr(unsafe.Pointer(unsafe.SliceData(s))) - unsafe.Sizeof(fgoBufHeader{}))).link = 1
	}
}

// fgoPtrsTainted reports whether fgoPtrsTaint marked s.
func fgoPtrsTainted(s []unsafe.Pointer) bool {
	return len(s) > 0 && unsafe.Pointer(unsafe.SliceData(s)) != unsafe.Pointer(&zerobase) &&
		(*fgoBufHeader)(unsafe.Pointer(uintptr(unsafe.Pointer(unsafe.SliceData(s)))-unsafe.Sizeof(fgoBufHeader{}))).link != 0
}

// fgoPtrsTake returns a new reference to the element at slot of a slice
// field of holder. If holder's reference is the only one, the slice's
// reference is moved out instead: the slot is cleared and the element
// keeps its count, so it can still be unique (and its memory reused)
// where the caller passes it. The compiler calls this only where nothing
// reads the slot again.
func fgoPtrsTake(holder, slot unsafe.Pointer) unsafe.Pointer {
	p := *(*unsafe.Pointer)(slot)
	if p == nil {
		return nil
	}
	if s := fgoQuick(holder); s == fgoCounted && fgoHdrOf(holder).rc == 1 || s == fgoMaybe && fgoUniqueSlow(holder) {
		*(*uintptr)(slot) = 0
	} else if fgoState(holder) == fgoCounted {
		fgoIncObj(p)
	} else {
		fgoRCInc(p)
	}
	return p
}

// fgoPtrsClone copies a slice of nogc pointers read from a field onto the
// GC heap, for a use that outlives the frame. Its elements are handed to
// the GC, like any nogc pointer stored in GC memory.
func fgoPtrsClone(s []unsafe.Pointer) []unsafe.Pointer {
	if len(s) == 0 {
		return s[:0:0]
	}
	if uintptr(unsafe.Pointer(unsafe.SliceData(s)))-fgoArena.lo >= fgoArena.size {
		return s
	}
	c := make([]unsafe.Pointer, len(s))
	for i, p := range s {
		fgoRCHandoff(p)
		c[i] = p
	}
	return c
}

// fgoPtrsPut stores ns, from fgoPtrsMake (or nil), into the slice field
// at dst of the object at base, dropping the slice it replaces. Objects
// other than counted ones get the slice on the GC heap.
func fgoPtrsPut(dst *[]unsafe.Pointer, ns []unsafe.Pointer, base unsafe.Pointer) {
	if fgoState(base) != fgoCounted {
		if len(ns) == 0 {
			*dst = ns
			return
		}
		c := make([]unsafe.Pointer, len(ns))
		for i, p := range ns {
			fgoRCHandoff(p)
			c[i] = p
		}
		*dst = c
		for _, p := range ns {
			fgoRCDec(p)
		}
		mp := acquirem()
		fgoBufFree(mp, unsafe.Pointer(unsafe.SliceData(ns)))
		releasem(mp)
		return
	}
	old := *dst
	*(*slice)(unsafe.Pointer(dst)) = *(*slice)(unsafe.Pointer(&ns))
	if fgoPtrsTainted(ns) {
		fgoRCHandoff(base) // it cannot count some element
	}
	if len(old) == 0 {
		return
	}
	for _, p := range old {
		fgoRCDec(p)
	}
	mp := acquirem()
	fgoBufFree(mp, unsafe.Pointer(unsafe.SliceData(old)))
	releasem(mp)
}

// fgoRCStats sums the per-M counters. It is a diagnostic: counts kept
// by an M that has exited are lost.
func fgoRCStats() (live, reuses int64) {
	lock(&sched.lock)
	for mp := allm; mp != nil; mp = mp.alllink {
		if rc := mp.fgoRC; rc != nil {
			live += rc.live
			reuses += rc.reuses
		}
	}
	unlock(&sched.lock)
	lock(&fgoRC.lock)
	live += fgoRC.live
	unlock(&fgoRC.lock)
	return
}

//go:linkname fgo_nogc_Live fgo/nogc.Live
func fgo_nogc_Live() int {
	live, _ := fgoRCStats()
	return int(live)
}

//go:linkname fgo_nogc_Reuses fgo/nogc.Reuses
func fgo_nogc_Reuses() int {
	_, reuses := fgoRCStats()
	return int(reuses)
}

//go:linkname fgo_nogc_count fgo/nogc.count
func fgo_nogc_count(p unsafe.Pointer) int {
	if fgoState(p) == fgoNone {
		return 0
	}
	return int(atomic.Loaduintptr(&fgoHdrOf(p).rc) &^ fgoHeld)
}

//go:linkname fgo_nogc_handedOff fgo/nogc.handedOff
func fgo_nogc_handedOff(p unsafe.Pointer) bool {
	return fgoState(p) == fgoHanded
}

// Frames that hold owned nogc pointers keep each one in a dedicated stack
// slot while they own it (cleared when it is released or handed on), and
// list those slots in FUNCDATA_FgoShadow: a uvarint count, then each
// slot's offset below the frame's varp as a uvarint; then the same for
// slots holding reuse tokens (memory of a dead object kept for an
// allocation that has not happened yet), and for slots holding string
// buffers on their way to a store. When frames are
// discarded without running their own releases — recover() jumping past
// them, or Goexit ending the goroutine — the runtime releases whatever
// their slots still hold.

// fgoRCReleaseFrames releases the owned nogc pointers of gp's frames with
// sp at most limit. gp must be stopped (its sched saved), as in recovery.
func fgoRCReleaseFrames(gp *g, limit uintptr) {
	var u unwinder
	for u.init(gp, 0); u.valid(); u.next() {
		if u.frame.sp > limit {
			break
		}
		fgoRCReleaseFrame(&u.frame)
	}
}

// fgoRCReleaseOwnFrames releases the owned nogc pointers of every frame of
// the calling goroutine above its caller.
//
//go:noinline
func fgoRCReleaseOwnFrames() {
	pc, sp, gp := sys.GetCallerPC(), sys.GetCallerSP(), getg()
	systemstack(func() {
		var u unwinder
		for u.initAt(pc, sp, 0, gp, 0); u.valid(); u.next() {
			fgoRCReleaseFrame(&u.frame)
		}
	})
}

func fgoRCReleaseFrame(frame *stkframe) {
	info := (*byte)(funcdata(frame.fn, abi.FUNCDATA_FgoShadow))
	if info == nil {
		return
	}
	for list := range 3 { // owned pointers, reuse tokens, string buffers
		var n uintptr
		n, info = fgoUvarint(info)
		for range n {
			var off uintptr
			off, info = fgoUvarint(info)
			slot := (*unsafe.Pointer)(unsafe.Pointer(frame.varp - off))
			p := *slot
			if p == nil {
				continue
			}
			*(*uintptr)(unsafe.Pointer(slot)) = 0
			switch list {
			case 0:
				fgoRCDec(p)
			case 1:
				mp := acquirem()
				fgoRCFree(mp, p)
				releasem(mp)
			case 2:
				mp := acquirem()
				fgoBufFree(mp, p)
				releasem(mp)
			}
		}
	}
}

func fgoUvarint(p *byte) (uintptr, *byte) {
	var v uintptr
	for shift := 0; ; shift += 7 {
		b := *p
		p = addb(p, 1)
		v |= uintptr(b&0x7f) << shift
		if b < 0x80 {
			return v, p
		}
	}
}

// fgoWeakHandle returns the weak handle for p if p points to a nogc
// object, handing the object to the GC, which decides when it dies.
func fgoWeakHandle(p unsafe.Pointer) *atomic.Uintptr {
	if uintptr(p)-fgoArena.lo >= fgoArena.size {
		return nil
	}
	fgoRCHandoff(p)
	obj := fgoHandedObj(uintptr(p))
	if obj == 0 {
		throw("nogc: weak pointer into a counted object")
	}
	lock(&fgoEsc.lock)
	r := fgoEscFind((*fgoEscTab)(fgoEsc.tab), obj)
	if r.weak == nil {
		r.weak = (*atomic.Uintptr)(persistentalloc(unsafe.Sizeof(atomic.Uintptr{}), goarch.PtrSize, &memstats.gcMiscSys))
		r.weak.Store(uintptr(p))
	}
	h := r.weak
	unlock(&fgoEsc.lock)
	return h
}

// fgoMakeStrong returns the pointer weak handle h holds to a nogc object,
// or nil if the GC has condemned the object.
func fgoMakeStrong(h *atomic.Uintptr) unsafe.Pointer {
	lock(&fgoEsc.lock)
	p := h.Load()
	var r *fgoEscRec
	if p != 0 {
		r = fgoEscFind((*fgoEscTab)(fgoEsc.tab), fgoHandedObj(p))
	}
	if r == nil {
		unlock(&fgoEsc.lock)
		return nil
	}
	last := work.cycles.Load()
	marking := gcphase != _GCoff
	if marking {
		last--
	}
	if int32(last-atomic.Load(&r.mark)) > 0 && atomic.Loaduintptr(&fgoHdrOf(unsafe.Pointer(r.obj)).rc) == fgoHeld {
		unlock(&fgoEsc.lock)
		return nil // the sweep frees it
	}
	unlock(&fgoEsc.lock)
	if marking {
		shade(p)
	}
	return unsafe.Pointer(p)
}
