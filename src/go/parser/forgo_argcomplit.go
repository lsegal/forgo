// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements forgo's untyped composite literal call arguments:
// a call argument may be written {...} or &{...}, with the literal's type
// inferred from the parameter it is passed to (see go/types/forgo_argcomplit.go).

package parser

import (
	"go/ast"
	"go/token"
)

// parseArg parses a single call argument.
//
//	arg = LiteralValue | "&" LiteralValue | Expression .
//
// Only a whole argument may be an untyped literal: {...} and &{...} are
// not operands, so they cannot appear inside a larger expression.
func (p *parser) parseArg() ast.Expr {
	defer decNestLev(incNestLev(p))

	if p.trace {
		defer un(trace(p, "Arg"))
	}

	switch p.tok {
	case token.LBRACE:
		return p.parseLiteralValue(nil)

	case token.AND:
		pos := p.pos
		p.next()
		if p.tok == token.LBRACE {
			return &ast.UnaryExpr{OpPos: pos, Op: token.AND, X: p.parseLiteralValue(nil)}
		}
		// not a literal: finish parsing the expression as parseRhs would
		old := p.inRhs
		p.inRhs = true
		x := &ast.UnaryExpr{OpPos: pos, Op: token.AND, X: p.parseUnaryExpr()}
		y := p.parseBinaryExpr(x, token.LowestPrec+1)
		p.inRhs = old
		return y
	}

	return p.parseRhs()
}
