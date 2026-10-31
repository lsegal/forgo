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

// Fields tagged `fgo:"nogc"`.
//
// A struct field of pointer-to-struct type tagged `fgo:"nogc"` holds a
// counted reference wherever the struct lives, including in an object of
// the GC heap, which nogc code otherwise cannot count references from.
// Every store into such a field, from any code, goes through fgoTagStore
// (or fgoRCStoreTag, from nogc code), and a read of it by code that is
// not nogc hands the object it gets to the GC (fgoTagLoad), since that
// code does not count the reference it keeps.
//
// A GC object can be reached from any goroutine, so an object stored in
// a tagged field, and every counted object it reaches, becomes shared
// (fgoShare): fgoShared in fgoArena.state, and fgoSharedBit in its count,
// which then changes atomically. A shared object is still freed when its
// last reference dies; it just takes the slow paths of counting, and is
// never reused in place.
//
// When the GC frees an object holding tagged fields, their references
// must be released: each store of an object into a tagged field of a GC
// heap object adds a special record for that field (_KindSpecialFgoTag),
// and freeSpecial calls fgoTagDead for it while the sweeper frees the
// object. That drops the reference, and an object whose count reaches
// zero there is queued on fgoTag.pending (the sweeper must not run the
// release), to be released by the next fgoTagDrain.

// fgoSharedBit marks the count of a shared object.
const fgoSharedBit = uintptr(1) << (goarch.PtrSize*8 - 2)

var fgoTag struct {
	pending uintptr // objects to release, linked through their count words
}

// fgoSharedDec drops a reference to shared object p.
func fgoSharedDec(p unsafe.Pointer) {
	if atomic.Xadduintptr(&fgoHdrOf(p).rc, ^uintptr(0)) == fgoSharedBit {
		fgoRCRelease(p, true)
	}
}

// fgoShare makes p's object, if it is a counted one, and every counted
// object it reaches shared. The caller holds a reference to p, so that
// those objects are reachable only from the caller's goroutine until
// then.
func fgoShare(p unsafe.Pointer) {
	if fgoState(p) != fgoCounted {
		return
	}
	work := []unsafe.Pointer{p}
	for len(work) > 0 {
		x := work[len(work)-1]
		work = work[:len(work)-1]
		if fgoState(x) != fgoCounted {
			continue // reached twice
		}
		h := fgoHdrOf(x)
		h.rc |= fgoSharedBit
		fgoMarkHanded(x, fgoShared)
		typ := h.typ
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
				if c := *(*unsafe.Pointer)(f); c != nil && fgoState(c) == fgoCounted {
					work = append(work, c)
				}
			case off&2 != 0:
				for _, c := range *(*[]unsafe.Pointer)(f) {
					if c != nil && fgoState(c) == fgoCounted {
						work = append(work, c)
					}
				}
			}
		}
	}
}

// fgoTagStore stores v into the tagged field at slot, for code that is
// not nogc: the field takes a reference of its own to v.
func fgoTagStore(slot *unsafe.Pointer, v unsafe.Pointer) {
	fgoTagDrain()
	fgoTagHold(v)
	fgoTagPut(slot, v)
}

// fgoRCStoreTag is fgoRCStore for a tagged field: v is a reference the
// caller hands over, and a field of an object that is not counted (a GC
// object, a global) takes it instead of handing v to the GC.
func fgoRCStoreTag(base unsafe.Pointer, slot *unsafe.Pointer, v unsafe.Pointer) {
	switch fgoState(base) {
	case fgoCounted, fgoShared, fgoHanded:
		fgoRCStore(base, slot, v)
		return
	}
	fgoTagDrain()
	if fgoGCPtr(v) {
		*slot = v // a GC object: the GC traces it from here
		return
	}
	fgoShare(v)
	fgoTagPut(slot, v)
}

// fgoRCStoreTagGC is fgoRCStoreTag for a tagged field through which
// base's object can reach itself again: a counted object is handed off
// first, as by fgoRCStoreGC; a GC object cannot be reached from the
// counted objects it holds, so it holds v counted.
func fgoRCStoreTagGC(base unsafe.Pointer, slot *unsafe.Pointer, v unsafe.Pointer) {
	switch fgoState(base) {
	case fgoCounted, fgoShared, fgoHanded:
		fgoRCStoreGC(base, slot, v)
		return
	}
	fgoRCStoreTag(base, slot, v)
}

// fgoTagHold takes a reference to v for a tagged field, sharing it first.
func fgoTagHold(v unsafe.Pointer) {
	switch fgoState(v) {
	case fgoCounted:
		fgoShare(v)
		fallthrough
	case fgoShared, fgoHanded:
		atomic.Xadduintptr(&fgoHdrOf(v).rc, 1)
	}
}

// fgoTagPut stores v, a reference the field takes over, into the tagged
// field at slot, and releases the reference the field held.
func fgoTagPut(slot *unsafe.Pointer, v unsafe.Pointer) {
	old := *slot
	*slot = v // with a write barrier: v may be a GC object
	if old == nil && v != nil {
		fgoTagWatch(slot)
	}
	fgoRCDec(old)
}

// fgoTagLoad is what code that is not nogc reads from a tagged field: it
// keeps p without counting it, so p's object is handed to the GC.
func fgoTagLoad(p unsafe.Pointer) unsafe.Pointer {
	fgoRCHandoff(p)
	return p
}

// specialFgoTag records a tagged field of a GC heap object (at the
// special's offset) that may hold a reference.
type specialFgoTag struct {
	_       sys.NotInHeap
	special special
}

// fgoTagWatch adds a special record for the tagged field at slot, if it
// is in the GC heap and has none, so that the GC releases the reference
// it holds when it frees the object.
func fgoTagWatch(slot *unsafe.Pointer) {
	if spanOfHeap(uintptr(unsafe.Pointer(slot))) == nil {
		return // a global, which lives forever, or a nogc object
	}
	lock(&mheap_.speciallock)
	s := (*specialFgoTag)(mheap_.specialFgoTagAlloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialFgoTag
	if !addspecial(unsafe.Pointer(slot), &s.special, false) {
		lock(&mheap_.speciallock)
		mheap_.specialFgoTagAlloc.free(unsafe.Pointer(s))
		unlock(&mheap_.speciallock)
	}
}

// fgoTagDead releases the reference held by the tagged field at p, of an
// object the GC is freeing. It runs in the sweeper, so it only queues an
// object whose count reaches zero.
//
//go:nowritebarrierrec
func fgoTagDead(p unsafe.Pointer) {
	slot := (*uintptr)(p)
	v := unsafe.Pointer(*slot)
	*slot = 0
	if v == nil {
		return
	}
	switch fgoState(v) {
	case fgoHanded:
		atomic.Xadduintptr(&fgoHdrOf(v).rc, ^uintptr(0))
	case fgoShared:
		h := fgoHdrOf(v)
		if atomic.Xadduintptr(&h.rc, ^uintptr(0)) != fgoSharedBit {
			return
		}
		for {
			head := atomic.Loaduintptr(&fgoTag.pending)
			h.rc = head
			if atomic.Casuintptr(&fgoTag.pending, head, uintptr(v)) {
				return
			}
		}
	}
}

// fgoTagDrain releases the objects whose last reference died while the
// GC freed objects holding them.
func fgoTagDrain() {
	if atomic.Loaduintptr(&fgoTag.pending) == 0 {
		return
	}
	p := atomic.Xchguintptr(&fgoTag.pending, 0)
	for p != 0 {
		next := fgoHdrOf(unsafe.Pointer(p)).rc
		fgoRCRelease(unsafe.Pointer(p), true)
		p = next
	}
}

// fgoTagNogc reports whether struct tag tag has fgo:"nogc".
func fgoTagNogc(tag string) bool {
	const want = `fgo:"nogc"`
	for i := 0; i+len(want) <= len(tag); i++ {
		if (i == 0 || tag[i-1] == ' ') && tag[i:i+len(want)] == want {
			return true
		}
	}
	return false
}

// fgoTagCount counts the tagged fields nested in a value of type t, a
// field of a counted object.
func fgoTagCount(t *_type) uintptr {
	n := uintptr(0)
	switch t.Kind() {
	case abi.Struct:
		for _, f := range (*structtype)(unsafe.Pointer(t)).Fields {
			if f.Typ.Kind() == abi.Pointer && fgoIsRCPtrType(f.Typ) && fgoTagNogc(f.Name.Tag()) {
				n++
			} else {
				n += fgoTagCount(f.Typ)
			}
		}
	case abi.Array:
		if at := (*arraytype)(unsafe.Pointer(t)); at.Elem.PtrBytes != 0 {
			n = uintptr(at.Len) * fgoTagCount(at.Elem)
		}
	}
	return n
}

// fgoTagFill adds to d the offsets of the tagged fields nested in a value
// of type t at offset off of a counted object.
func fgoTagFill(t *_type, off uintptr, d *fgoRCDesc) {
	switch t.Kind() {
	case abi.Struct:
		for _, f := range (*structtype)(unsafe.Pointer(t)).Fields {
			if f.Typ.Kind() == abi.Pointer && fgoIsRCPtrType(f.Typ) && fgoTagNogc(f.Name.Tag()) {
				d.offs[d.n] = off + f.Offset
				d.n++
			} else {
				fgoTagFill(f.Typ, off+f.Offset, d)
			}
		}
	case abi.Array:
		if at := (*arraytype)(unsafe.Pointer(t)); at.Elem.PtrBytes != 0 && fgoTagCount(at.Elem) != 0 {
			for i := uintptr(0); i < uintptr(at.Len); i++ {
				fgoTagFill(at.Elem, off+i*at.Elem.Size_, d)
			}
		}
	}
}
