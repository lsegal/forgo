// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements forgo's untyped composite literal call arguments.
// When a call argument is {...} or &{...} and the parameter it is passed
// to has a struct type S or a pointer type *S, the literal is checked as
// if S had been written: f({A: 1}) is f(S{A: 1}) (or f(&S{A: 1}) for a
// *S parameter), and f(&{A: 1}) is f(&S{A: 1}). The parameter type is
// passed down as the literal's hint, the same way a slice literal passes
// its element type to an elided element literal, so the type is recorded
// for the literal and the rest of the compiler sees an ordinary one.
package types

import (
	"go/ast"
	"go/token"
)

// forgoArgHints returns the composite literal hints for the arguments of
// call, which calls a function with signature sig: for each argument that
// is an untyped composite literal {...} or &{...} whose parameter type
// allows it, the parameter type; nil otherwise. The result is nil when no
// argument needs a hint.
func forgoArgHints(sig *Signature, call *ast.CallExpr) []Type {
	// The parameter types of a generic function are only known once its
	// type arguments are inferred, which needs the argument types first.
	if sig.TypeParams().Len() > 0 {
		return nil
	}
	var hints []Type
	for i, arg := range call.Args {
		addr := false
		if op, _ := arg.(*ast.UnaryExpr); op != nil && op.Op == token.AND {
			arg, addr = op.X, true
		}
		if lit, _ := arg.(*ast.CompositeLit); lit == nil || lit.Type != nil {
			continue
		}
		typ := forgoParamType(sig, i, call.Ellipsis.IsValid())
		if typ == nil {
			continue
		}
		var base Type
		if p, _ := typ.Underlying().(*Pointer); p != nil {
			base = p.base
		} else if !addr {
			base = typ
		}
		if base == nil {
			continue
		}
		if _, ok := base.Underlying().(*Struct); !ok {
			continue
		}
		if hints == nil {
			hints = make([]Type, len(call.Args))
		}
		hints[i] = typ
	}
	return hints
}

// forgoParamType returns the type of the parameter of sig that argument i
// is passed to, or nil if there is none.
func forgoParamType(sig *Signature, i int, hasDots bool) Type {
	n := sig.params.Len()
	if sig.variadic && !hasDots && i >= n-1 {
		if s, _ := sig.params.At(n - 1).typ.(*Slice); s != nil {
			return s.elem
		}
		return nil
	}
	if i < n {
		return sig.params.At(i).typ
	}
	return nil
}

// forgoAddrLitHint returns the hint for the operand of the unary
// expression e, given the hint for e itself: the base type of a pointer
// hint when e is &{...}, nil otherwise.
func forgoAddrLitHint(e *ast.UnaryExpr, hint Type) Type {
	if hint == nil || e.Op != token.AND {
		return nil
	}
	if lit, _ := ast.Unparen(e.X).(*ast.CompositeLit); lit == nil || lit.Type != nil {
		return nil
	}
	if p, _ := hint.Underlying().(*Pointer); p != nil {
		return p.base
	}
	return nil
}

// forgoHint returns hints[i], or nil if hints has no entry for i.
func forgoHint(hints []Type, i int) Type {
	if i < len(hints) {
		return hints[i]
	}
	return nil
}
