// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// forgo: a call argument may be an untyped composite literal {...} or &{...}.

package argcomplit

func _() {
	f({A: 1})
	f(&{A: 1})
	f({}, &{}, {1, 2}...)
	f(&x, &x.y + 1, &T{})
	if f({A: 1}) {
	}
	g(func() { f(&{A: 1}) })
}

// {...} and &{...} are not operands: they cannot be used outside an
// argument list.
func _() {
	_ = & /* ERROR unexpected {, expected expression */ {}
/* ERROR non-declaration statement outside function body */ }
