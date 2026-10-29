// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// forgo: an untyped composite literal call argument {...} or &{...} takes
// its type from the parameter it is passed to, when that parameter is a
// struct type S or a pointer type *S.

package argcomplit

type S struct{ A int }

type P *S

type I []int

func val(S)                   {}
func ptr(*S)                  {}
func named(P)                 {}
func two(int, S)              {}
func variadic(...*S)          {}
func slice(I)                 {}
func generic[T any](T)        {}
func anon(struct{ B string }) {}

type R struct{}

func (R) m(S, *S) {}

func _() {
	val({A: 1})
	val({1})
	val({})
	ptr({A: 1})
	ptr(&{A: 1})
	named({A: 1})
	named(&{A: 1})
	two(1, {A: 2})
	variadic()
	variadic({A: 1}, &{A: 2}, {})
	variadic([]*S{{A: 1}}...)
	anon({B: "b"})
	R{}.m({A: 1}, &{A: 2})
	R.m(R{}, {A: 1}, {A: 2})
	func(s S) {}({A: 1})
	generic[S]({A: 1})

	// explicitly typed literals are unaffected
	val(S{A: 1})
	ptr(&S{A: 1})

	// the inferred type is checked like an explicit one
	val({B /* ERROR "unknown field B in struct literal of type S" */ : 1})
	ptr(&{A: "a" /* ERROR "cannot use \"a\"" */})
	val({1, 2 /* ERROR "too many values" */})

	// only struct types (and pointers to them) are inferred
	val(&{ /* ERROR "missing type in composite literal" */ A: 1})
	slice({ /* ERROR "missing type in composite literal" */ 1, 2})
	generic({ /* ERROR "missing type in composite literal" */ A: 1})
	two({ /* ERROR "missing type in composite literal" */ }, {})
}
