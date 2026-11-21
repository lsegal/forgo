// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains tests for the enumdefault checker.

package enumdefault

import "cmd/vet/testdata/enumdefault/shapes"

type Shape enum {
	Circle(radius float64)
	Rect(w, h float64)
	Empty
}

type Color enum {
	Red
	Green
	Blue
}

type Option[T any] enum {
	Some(T)
	None
}

func TypeSwitch(s Shape) float64 {
	switch v := s.(type) {
	case Shape.Circle:
		return v.radius
	default: // ERROR "default clause hides variants Shape.Rect, Shape.Empty and any added later"
		return 0
	}
}

func TypeSwitchOneHidden(s Shape) float64 {
	switch v := s.(type) {
	case Shape.Circle:
		return v.radius
	case Shape.Rect:
		return 1
	default: // ERROR "default clause hides variant Shape.Empty and any added later"
		return 0
	}
}

func TypeSwitchNoBinding(s Shape) bool {
	switch (s).(type) {
	case Shape.Empty:
		return true
	default: // ERROR "default clause hides variants Shape.Circle, Shape.Rect and any added later"
	}
	return false
}

func Unreachable(s Shape) int {
	switch s.(type) {
	case Shape.Circle, Shape.Rect:
		return 1
	case Shape.Empty:
		return 2
	default: // ERROR "default clause is unreachable, but would hide variants added to Shape later"
		return 3
	}
}

func NoDefault(s Shape) int {
	switch s.(type) {
	case Shape.Circle:
		return 1
	case Shape.Rect, Shape.Empty:
		return 2
	}
	return 0
}

func ExprSwitch(c Color) string {
	switch c {
	case Color.Red:
		return "red"
	default: // ERROR "default clause hides variants Color.Green, Color.Blue and any added later"
		return "other"
	}
}

func ExprSwitchValues(s, t Shape) int {
	// A constructor call or variable only matches particular values.
	switch s {
	case Shape.Circle(1), t:
		return 1
	case Shape.Empty:
		return 2
	default: // ERROR "default clause hides variants Shape.Circle, Shape.Rect and any added later"
		return 3
	}
}

func ExprSwitchNoDefault(c Color) string {
	switch c {
	case Color.Red, Color.Green, Color.Blue:
		return "rgb"
	}
	return ""
}

func NotEnum(x any, n int) int {
	switch x.(type) {
	case int:
		return 1
	default:
	}
	switch n {
	case 1:
		return 1
	default:
	}
	return 0
}

func Generic[T any](o Option[T]) bool {
	switch o.(type) {
	case Option.None:
		return false
	default: // ERROR "default clause hides variant Option.T..Some and any added later"
		return true
	}
}

func Instantiated(o Option[int]) int {
	switch v := o.(type) {
	case Option[int].Some:
		return v._0
	default: // ERROR "default clause hides variant Option.int..None and any added later"
		return 0
	}
}

func OtherPackage(s shapes.Shape) float64 {
	switch s.(type) {
	case shapes.Shape.Circle:
		return 2
	case shapes.Shape.Empty:
		return 0
	default: // ERROR "default clause hides variants shapes.Shape.Rect, shapes.Shape.hidden and any added later"
		return 1
	}
}

func Nested(s Shape, c Color) int {
	switch s.(type) {
	case Shape.Circle, Shape.Rect, Shape.Empty:
		func() {
			switch c {
			case Color.Blue:
			default: // ERROR "default clause hides variants Color.Red, Color.Green and any added later"
			}
		}()
	}
	return 0
}
