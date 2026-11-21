// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fmt_test

import (
	. "fmt"
	"testing"
	"unsafe"
)

type enumShape enum {
	Circle(radius float64)
	Rect(w, h float64)
	Empty
}

type enumPair enum { P(int, string) }

// notEnum looks like an enum's representation, but lacks the tag.
type notEnum struct {
	_tag uint8
	A    struct{ x int }
}

func TestForgoEnumPrint(t *testing.T) {
	circle, rect, empty := enumShape.Circle(2), enumShape.Rect(3, 4), enumShape.Empty
	pair := enumPair.P(7, "x")
	bad := enumShape.Empty
	*(*uint8)(unsafe.Pointer(&bad)) = 9 // no such variant: print the struct

	for _, tt := range []struct {
		format string
		arg    any
		want   string
	}{
		{"%v", circle, "Circle{2}"},
		{"%+v", circle, "Circle{radius:2}"},
		{"%#v", circle, "fmt_test.enumShape.Circle(2)"},
		{"%v", rect, "Rect{3 4}"},
		{"%+v", rect, "Rect{w:3 h:4}"},
		{"%#v", rect, "fmt_test.enumShape.Rect(3, 4)"},
		{"%v", empty, "Empty"},
		{"%+v", empty, "Empty"},
		{"%#v", empty, "fmt_test.enumShape.Empty"},
		{"%v", &rect, "&Rect{3 4}"},
		{"%v", []enumShape{circle, empty}, "[Circle{2} Empty]"},
		{"%v", pair, "P{7 x}"},
		{"%+v", pair, "P{7 x}"},
		{"%#v", pair, `fmt_test.enumPair.P(7, "x")`},
		{"%v", bad, "{9 {0} {0 0} {}}"},
		{"%v", notEnum{}, "{0 {0}}"},
	} {
		if got := Sprintf(tt.format, tt.arg); got != tt.want {
			t.Errorf("Sprintf(%q, %T) = %q, want %q", tt.format, tt.arg, got, tt.want)
		}
	}
}
