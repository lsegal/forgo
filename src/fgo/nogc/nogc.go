// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package nogc reports on reference counted ("nogc") memory.
//
// Code opts into nogc with a //fgo:nogc directive on a function, its root:
//
//	//fgo:nogc
//	func build(n int) *Node {
//		...
//		return &Node{Val: n, Left: build(n - 1), Right: build(n - 1)}
//	}
//
// &T{...} and new(T) of a struct type in a root, and in every function
// of the same package that a root calls directly (or calls one that
// does, and so on), allocate outside the GC heap. The compiler counts the
// references that code holds, frees each object as soon as its last
// reference dies, and reuses a dying object's memory for a following
// allocation of the same size where it can. No free call exists. A
// //fgo:nogc directive on a type makes its methods strict roots (see
// below), and every //fgo:comptime function is a root.
//
// Being nogc is a property of where an object was allocated, not of its
// type: any struct type works, including types from other packages, and
// the same type is garbage collected when other code allocates it. The
// functions a root reaches still exist unchanged for other callers; the
// compiler builds a second, nogc copy of each for nogc code to call.
//
// Nogc code can hand its objects to any other code. When a nogc object
// is stored where nogc code does not count it (an interface, a map, a
// slice, a global, a closure, a channel, a field of a garbage collected
// object), or passed to code that is not nogc and may keep or change it
// (another package's function that the compiler did not inline, a func
// value, an interface method, a goroutine), it is handed to the garbage
// collector first, with every nogc object it reaches. From then on the
// garbage collector frees it once nothing points to it, and its fields
// hold ordinary garbage collected values. Taking the address of a field
// of a nogc object, and assigning a field through which the object can
// reach itself again (outside a composite literal), hand the object off
// the same way, so that reference cycles are the collector's to free.
//
// A string field of a nogc object owns its bytes, outside the GC heap:
// storing into it copies the string, and the bytes are freed with the
// object. Reading it gives a string pointing into those bytes, and the
// object is kept alive for as long as that string is used; where the
// string outlives the function (stored elsewhere, returned, or passed
// somewhere that keeps it), the read copies it onto the GC heap instead.
// A slice field of numbers and bools works the same way: storing into it
// copies the elements (with capacity equal to length, so an append to
// the field always copies again), and reading it gives a view whose
// element writes change the object. In nogc code, these stores copy into
// garbage collected objects too.
//
// A slice of struct pointers in a field of a nogc object holds a
// reference to each element. Nogc code does not assign its elements:
// it stores a new slice, built from a slice literal, append, or slicing
// of another such field.
//
// When recover() discards frames, or Goexit ends a goroutine, the objects
// those frames owned are released too.
//
// A //fgo:nogc strict root may not hand anything to the garbage
// collector: each hand-off in it, or in a function it calls, is a
// compile error saying what escapes and where.
//
// A struct field of pointer-to-struct type tagged `fgo:"nogc"` holds a
// counted reference wherever the struct lives, including in a garbage
// collected object. What it holds, and everything that reaches, is
// shared: counted atomically, as other goroutines may reach it, and
// still freed at its last reference. When the garbage collector frees an
// object holding a tagged field, the field's reference is released. Code
// that is not nogc may store into a tagged field; reading one hands what
// it reads to the garbage collector. A struct holding a tagged field
// cannot be copied: the compiler rejects every copy of one.
package nogc

import "unsafe"

// Count reports the reference count of the nogc object p points to, or 0
// if p does not point to one. Counts of objects handed to the garbage
// collector include only the references nogc code holds.
func Count[T any](p *T) int { return count(unsafe.Pointer(p)) }

// HandedOff reports whether p points to a nogc object that was handed to
// the garbage collector.
func HandedOff[T any](p *T) bool { return handedOff(unsafe.Pointer(p)) }

// Live reports how many nogc objects and string buffers are allocated and
// not yet freed.
func Live() int

// Reuses reports how many allocations reused a dying object's memory.
func Reuses() int

func count(p unsafe.Pointer) int

func handedOff(p unsafe.Pointer) bool
