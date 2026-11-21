// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package enumdefault defines an Analyzer that reports default clauses
// in switches over forgo enum values.
//
// # Analyzer enumdefault
//
// enumdefault: report default clauses that hide enum variants
//
// A switch over a forgo enum value without a default clause must name
// every variant, so adding a variant later is a compile error at every
// switch that does not handle it yet. A default clause turns that check
// off: today's unnamed variants, and every variant added later, go to
// the default clause silently. This analyzer reports such a clause,
// naming the variants that reach it:
//
//	switch v := s.(type) {
//	case Shape.Circle:
//		return v.radius
//	default: // default clause hides variants Shape.Rect, Shape.Empty and any added later
//		return 0
//	}
//
// It checks type switches (switch v := x.(type)) and expression switches
// (switch x) over enum values. A default clause in a switch whose cases
// already name every variant is unreachable today but would still catch
// variants added later, so it is reported too, with its own message.
//
// The check is off by default, since a default clause is sometimes the
// right choice: when the variants it covers genuinely share one meaning.
// Enable it with -enumdefault, which adds it to the checks vet runs
// rather than replacing them.
package enumdefault
