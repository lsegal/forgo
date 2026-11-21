// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package shapes declares an enum for the enumdefault test to switch
// over from another package.
package shapes

type Shape enum {
	Circle(radius float64)
	Rect(w, h float64)
	Empty
	hidden
}
