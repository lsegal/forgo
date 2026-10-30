// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

// FgoOwnedTag marks (as its Note) a nogc pointer parameter the callee
// takes ownership of. Other nogc pointer parameters are borrowed.
const FgoOwnedTag = "fgo:own"

// FgoParamOwned reports whether parameter f is an owned nogc pointer.
func FgoParamOwned(f *Field) bool { return f.Note == FgoOwnedTag }

// FgoIsRC reports whether t is a struct type that nogc code (see
// cmd/compile/internal/noder's forgo_nogc.go) allocates as a reference
// counted object. Any struct type can be, except the compiler's own
// (closures, method values), which are noalg.
func (t *Type) FgoIsRC() bool {
	return t != nil && t.IsStruct() && !t.IsFuncArgStruct() && !t.IsShape() && !t.NotInHeap() && !t.Noalg()
}

// FgoIsRCPtr reports whether t is a nogc pointer type: a pointer to a
// struct, which nogc code counts. The runtime counts the same types
// (runtime.fgoIsRCPtrType).
func (t *Type) FgoIsRCPtr() bool {
	return t != nil && t.IsPtr() && t.Elem().FgoIsRC()
}

// FgoHasRCFields reports whether struct type t has fields a counted object
// owns: nogc pointers, slices of them, strings and scalar slices.
func FgoHasRCFields(t *Type) bool {
	for _, f := range t.Fields() {
		if f.Type.FgoIsRCPtr() || FgoPtrSliceType(f.Type) || FgoBufType(f.Type) {
			return true
		}
	}
	return false
}

// FgoMutableFields records the fields of local types that nogc code
// assigns anywhere other than in a composite literal. The nogc checker
// fills it in before any function is compiled.
var FgoMutableFields = map[*Field]bool{}

// FgoFieldImmutable reports whether field f of struct type t never changes
// in a counted object after construction: recursive fields never do
// (assigning one hands the object to the GC first), and other fields of
// local types do not if no nogc code assigns them. (Only nogc code
// changes counted objects; a pointer read from an object that has been
// handed off is on the stack, where the GC sees it.)
func FgoFieldImmutable(t *Type, f *Field) bool {
	if FgoRecursiveField(t, f) {
		return true
	}
	s := t.Sym()
	return s != nil && s.Pkg == LocalPkg && !FgoMutableFields[f]
}

// FgoRecursiveField reports whether field f of struct type t is a nogc
// pointer (or slice of them) through which t can be reached again. Nogc
// code sets such a field of a counted object only when the object is
// constructed, which makes every counted object's children older than it
// and so rules out reference cycles; it hands the object to the GC
// before assigning one anywhere else.
func FgoRecursiveField(t *Type, f *Field) bool {
	ft := f.Type
	if FgoPtrSliceType(ft) {
		ft = ft.Elem()
	}
	if !ft.FgoIsRCPtr() {
		return false
	}
	seen := map[*Type]bool{}
	var reaches func(u *Type) bool
	reaches = func(u *Type) bool {
		if Identical(u, t) {
			return true
		}
		if seen[u] {
			return false
		}
		seen[u] = true
		for _, g := range u.Fields() {
			gt := g.Type
			if FgoPtrSliceType(gt) {
				gt = gt.Elem()
			}
			if gt.FgoIsRCPtr() && reaches(gt.Elem()) {
				return true
			}
		}
		return false
	}
	return reaches(ft.Elem())
}

// FgoScalar reports whether t holds no pointers at all: numbers, bools,
// and arrays and structs of them.
func FgoScalar(t *Type) bool {
	switch {
	case t.IsBoolean(), t.IsInteger(), t.IsFloat(), t.IsComplex():
		return true
	case t.IsArray():
		return FgoScalar(t.Elem())
	case t.IsStruct():
		for _, f := range t.Fields() {
			if !FgoScalar(f.Type) {
				return false
			}
		}
		return true
	}
	return false
}

// FgoBufType reports whether a nogc object field of type t owns a buffer
// outside the GC heap: a string, or a slice of scalars.
func FgoBufType(t *Type) bool {
	return t.IsString() || t.IsSlice() && FgoScalar(t.Elem())
}

// FgoPtrSliceType reports whether t is a slice of nogc pointers. As a
// nogc object field, such a slice owns its elements' references, in a
// buffer outside the GC heap. Its elements are never assigned in place:
// the field is rebuilt (from a slice literal, append, or slicing of
// another such field) instead.
func FgoPtrSliceType(t *Type) bool {
	return t != nil && t.IsSlice() && t.Elem().FgoIsRCPtr()
}

// FgoMixed reports whether struct type t has fields holding GC managed
// values (maps, interfaces, funcs, channels, pointers to anything but
// structs, slices of anything but scalars and struct pointers, arrays
// and structs holding pointers). The runtime makes live counted objects
// of such types roots for the garbage collector.
func FgoMixed(t *Type) bool {
	if !t.FgoIsRC() {
		return false
	}
	for _, f := range t.Fields() {
		ft := f.Type
		if !FgoScalar(ft) && !ft.FgoIsRCPtr() && !FgoBufType(ft) && !FgoPtrSliceType(ft) {
			return true
		}
	}
	return false
}
