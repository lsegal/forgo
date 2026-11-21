// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements parsing of forgo's enum types (see
// cmd/compile/internal/syntax/forgo_enum.go).

package parser

import (
	"go/ast"
	"go/token"
)

// parseTypeSpecType parses the type of a type spec. It is parseType,
// except that the type may also be an enum type:
//
//	type Shape enum { Circle(radius float64); Rect(w, h float64); Empty }
//
// "enum" is a contextual keyword, recognized only directly after the
// declared name (and its type parameters) and only when it is followed
// by "{". Anywhere else, and as a type named enum, it is an identifier.
func (p *parser) parseTypeSpecType() ast.Expr {
	if p.tok != token.IDENT || p.lit != "enum" {
		return p.parseType()
	}
	name := p.parseIdent()
	if p.tok != token.LBRACE {
		// Not an enum type: just a type (or package) named enum.
		typ := p.parseTypeName(name)
		if p.tok == token.LBRACK {
			typ = p.parseTypeInstance(typ)
		}
		return typ
	}
	return p.parseEnumType(name.Pos())
}

// EnumType    = "enum" "{" { EnumVariant ";" } "}" .
// EnumVariant = identifier [ Parameters ] .
func (p *parser) parseEnumType(pos token.Pos) *ast.EnumType {
	if p.trace {
		defer un(trace(p, "EnumType"))
	}

	typ := &ast.EnumType{Enum: pos}
	typ.Lbrace = p.expect(token.LBRACE)
	for p.tok == token.IDENT {
		v := &ast.EnumVariant{Name: p.parseIdent()}
		if p.tok == token.LPAREN {
			lparen := p.pos
			p.next()
			list := p.parseParameterList(nil, nil, token.RPAREN, false)
			rparen := p.expect(token.RPAREN)
			v.Params = &ast.FieldList{Opening: lparen, List: list, Closing: rparen}
		}
		typ.Variants = append(typ.Variants, v)
		if p.tok == token.RBRACE {
			break
		}
		p.expectSemi()
	}
	typ.Rbrace = p.expect(token.RBRACE)
	return typ
}
