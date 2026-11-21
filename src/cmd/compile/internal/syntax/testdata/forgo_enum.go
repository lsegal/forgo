// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// forgo: enum types.

package enum

type Shape enum {
	Circle(radius float64)
	Rect(w, h float64)
	Empty
}

type Option[T any] enum { Some(T); None }

type Result[T, E any] enum {
	Ok(T)
	Err(E)
}

type Pair enum { P(int, string); Q(a, b int, c string); R() }

type Empty enum {}

// enum is only a keyword before "{": these are types named enum.
type (
	A enum
	B enum.T
	C []enum
	D = enum
)

func _() {
	enum := 1
	_ = []enum{enum}
}

type Bad enum {
	X(int /* ERROR unexpected newline in parameter list */
}
