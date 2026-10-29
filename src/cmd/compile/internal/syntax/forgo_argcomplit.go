// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements forgo's untyped composite literal call arguments:
// a call argument may be written {...} or &{...}, with the literal's type
// inferred from the parameter it is passed to (see types2/forgo_argcomplit.go).

package syntax

// argExpr parses a single call argument.
//
//	arg = LiteralValue | "&" LiteralValue | Expression .
//
// Only a whole argument may be an untyped literal: {...} and &{...} are
// not operands, so they cannot appear inside a larger expression.
func (p *parser) argExpr() Expr {
	if trace {
		defer p.trace("argExpr")()
	}

	switch {
	case p.tok == _Lbrace:
		return p.complitexpr()

	case p.tok == _Operator && p.op == And:
		x := new(Operation)
		x.pos = p.pos()
		x.Op = And
		p.next()
		if p.tok == _Lbrace {
			x.X = p.complitexpr()
			return x
		}
		// not a literal: finish parsing the expression as unaryExpr would
		x.X = Unparen(p.unaryExpr())
		return p.binaryExpr(x, 0)
	}

	return p.expr()
}
