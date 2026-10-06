// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// An impl is the Go side of an object handed to the host.
type impl interface {
	// destroy runs when the last reference is released.
	destroy()
}

// An iface is one interface an object implements: its table and the
// IDs that queryInterface answers with it.
type iface struct {
	vtbl unsafe.Pointer
	iids []*TUID
}

// An object is reference counted across all of its interfaces. Each
// interface is a separate view; FUnknown is the first one, so every
// view queried for FUnknown yields the same pointer, as COM requires.
type object struct {
	refs   atomic.Int32
	impl   impl
	ifaces []iface
	views  []unsafe.Pointer
}

var (
	objectsMu sync.RWMutex
	objects   = map[uint64]*object{}
	nextObj   uint64
)

// newObject registers impl with one reference and returns it.
func newObject(im impl, ifaces ...iface) *object {
	o := &object{impl: im, ifaces: ifaces}
	o.refs.Store(1)
	objectsMu.Lock()
	nextObj++
	h := nextObj
	objects[h] = o
	objectsMu.Unlock()
	for _, f := range ifaces {
		o.views = append(o.views, newView(f.vtbl, h))
	}
	return o
}

// lookup returns the object that a view belongs to, or nil.
func lookup(self unsafe.Pointer) *object {
	if self == nil {
		return nil
	}
	objectsMu.RLock()
	defer objectsMu.RUnlock()
	return objects[handleOf(self)]
}

// implOf returns the impl behind a view, or nil.
func implOf(self unsafe.Pointer) impl {
	if o := lookup(self); o != nil {
		return o.impl
	}
	return nil
}

func (o *object) query(iid *TUID) unsafe.Pointer {
	if *iid == iidFUnknown {
		return o.views[0]
	}
	for i, f := range o.ifaces {
		for _, id := range f.iids {
			if *id == *iid {
				return o.views[i]
			}
		}
	}
	return nil
}

func (o *object) addRef() uint32 { return uint32(o.refs.Add(1)) }

func (o *object) release() uint32 {
	n := o.refs.Add(-1)
	if n > 0 {
		return uint32(n)
	}
	objectsMu.Lock()
	delete(objects, handleOf(o.views[0]))
	objectsMu.Unlock()
	o.impl.destroy()
	for _, v := range o.views {
		freeView(v)
	}
	return 0
}

// queryInterface answers for o through out, adding a reference.
func (o *object) queryInterface(iid unsafe.Pointer, out *unsafe.Pointer) Result {
	if iid == nil || out == nil {
		return InvalidArgument
	}
	v := o.query((*TUID)(iid))
	*out = v
	if v == nil {
		return NoInterface
	}
	o.addRef()
	return ResultOK
}
