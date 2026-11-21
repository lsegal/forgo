// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// forgo: enum types, variant constructors, and exhaustive type switches.

package enum

import "math"

type Shape enum {
	Circle(radius float64)
	Rect(w, h float64)
	Empty
}

func (s Shape) Area() (area float64) {
	switch v := s.(type) {
	case Shape.Circle:
		area = math.Pi * v.radius * v.radius
	case Shape.Rect:
		area = v.w * v.h
	case Shape.Empty:
		area = 0
	}
	return
}

type Option[T any] enum {
	Some(T)
	None
}

type Result[T, E any] enum {
	Ok(T)
	Err(E)
}

// Construction.
func _() {
	var s Shape = Shape.Circle(2)
	s = Shape.Rect(1, 2)
	s = Shape.Empty
	_ = s.Area()

	var o Option[int] = Option.Some(1)
	o = Option[int].None
	o = Option[int].Some(2)
	_ = o
	var r Result[string, error] = Result[string, error].Ok("x")
	_ = r
	r2 := Result.Ok[string, error]("y")
	_ = r2

	_ = Shape.Circle("x" /* ERRORx `cannot use "x"` */)
	_ = Shape.Circle /* ERROR "variant constructor Shape.Circle must be called" */
	_ = Option.None /* ERROR "cannot use generic enum Option without instantiation" */
	_ = Shape.Square /* ERROR "Shape.Square undefined" */ (1)
	_ = Shape /* ERROR "invalid composite literal of enum type Shape" */ {}
	_ = s.Circle /* ERROR "the variants of enum Shape are reached through a type switch" */
	_ = s._tag /* ERROR "the variants of enum Shape are reached through a type switch" */
	_ = Shape(struct /* ERROR "enum types only convert to themselves" */ {}{})
}

// Matching and binding.
func _(o Option[string], s Shape) {
	switch v := o.(type) {
	case Option.Some:
		var _ string = v._0
	case Option[string].None:
		var _ Option[string] = v
	}

	switch v := s.(type) {
	case Shape.Circle, Shape.Empty:
		var _ Shape = v
	case Shape.Rect:
		_ = v.w + v.h
	}

	switch s.(type) {
	case Shape.Circle, Shape.Rect:
	default:
	}

	switch s.(type) {
	case Shape.Circle:
	case Shape.Circle /* ERROR "duplicate case Shape.Circle in enum switch" */ :
	case Shape.Rect, Shape.Empty:
	}

	switch s.(type) {
	case int /* ERROR "must name a variant" */ :
	default:
	}

	switch s.(type) {
	case Option /* ERROR "must name a variant of Shape" */ [int].None:
	default:
	}
}

// Exhaustiveness.
func _(s Shape, r Result[int, error]) {
	switch /* ERROR "switch on Shape is not exhaustive: missing case Shape.Empty" */ s.(type) {
	case Shape.Circle:
	case Shape.Rect:
	}

	switch /* ERROR "switch on Shape is not exhaustive: missing cases Shape.Circle, Shape.Rect, Shape.Empty" */ s.(type) {
	}

	switch /* ERROR "switch on Result[int, error] is not exhaustive: missing case Result[int, error].Err" */ r.(type) {
	case Result.Ok:
	}
}

// Adding a variant breaks a switch that was exhaustive before.
type Shape2 enum {
	Circle(radius float64)
	Rect(w, h float64)
	Empty
	Triangle(a, b, c float64)
}

func _(s Shape2) {
	switch /* ERROR "missing case Shape2.Triangle" */ s.(type) {
	case Shape2.Circle:
	case Shape2.Rect:
	case Shape2.Empty:
	}
}

// Expression switches over enums whose variants carry no fields.
type Color enum { Red; Green; Blue }

func _(c Color, s Shape) {
	switch c {
	case Color.Red, Color.Green, Color.Blue:
	}
	switch /* ERROR "missing case Color.Blue" */ c {
	case Color.Red, Color.Green:
	}
	switch c {
	case Color.Red:
	default:
	}
	switch /* ERROR "match variants with fields in a type switch" */ s {
	case Shape.Empty:
	}
	_ = c == Color.Red
}

// Declarations.
type Bad1 enum /* ERROR "enum type must have at least one variant" */ {}

type Bad2 enum {
	A
	A /* ERROR "A redeclared" */
	_ /* ERROR "enum variant name cannot be _" */
	_tag /* ERROR "reserved" */
	B(x int, x /* ERROR "x redeclared" */ string)
}

type Bad3 = enum /* ERROR "enum type cannot be declared by an alias" */ { A }

type WithMethod enum { M; N }

func (WithMethod) M /* ERROR "field and method with the same name M" */ () {}

func _() {
	type Local enum { X(int); Y }
	switch v := Local.X(1).(type) {
	case Local.X:
		_ = v._0
	case Local.Y:
	}
}

// Comparability follows the payloads.
type Cmp enum { A(int); B(string) }
type NoCmp enum { A([]int); B }

func _(a, b Cmp, x, y NoCmp) {
	_ = a == b
	_ = x /* ERROR "cannot be compared" */ == y
}

// Generic code.
func Map[T, U any](o Option[T], f func(T) U) Option[U] {
	switch v := o.(type) {
	case Option.Some:
		return Option.Some(f(v._0))
	case Option.None:
		return Option[U].None
	}
	panic("unreachable")
}
