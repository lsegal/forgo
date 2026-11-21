// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements parsing of forgo's enum types (closed sum types
// whose variants carry their own fields; see types2/forgo_enum.go).

package syntax

// typeDeclType parses the type of a type declaration. It is typeOrNil,
// except that a declaration's type may also be an enum type:
//
//	type Shape enum { Circle(radius float64); Rect(w, h float64); Empty }
//
// "enum" is a contextual keyword, recognized only directly after the
// declared name (and its type parameters) and only when it is followed
// by "{". Anywhere else, and as a type named enum, it is an identifier.
func (p *parser) typeDeclType() Expr {
	if p.tok != _Name || p.lit != "enum" {
		return p.typeOrNil()
	}
	name := p.name()
	if p.tok != _Lbrace {
		// Not an enum type: just a type (or package) named enum.
		return p.qualifiedName(name)
	}
	return p.enumType(name.pos)
}

// EnumType    = "enum" "{" { EnumVariant ";" } "}" .
// EnumVariant = identifier [ Parameters ] .
func (p *parser) enumType(pos Pos) *EnumType {
	if trace {
		defer p.trace("enumType")()
	}

	typ := new(EnumType)
	typ.pos = pos

	p.want(_Lbrace)
	p.list("enum type", _Semi, _Rbrace, func() bool {
		v := new(EnumVariant)
		v.pos = p.pos()
		v.Name = p.name()
		if p.tok == _Lparen {
			p.next()
			v.ParamList = p.paramList(nil, nil, _Rparen, false, false)
		}
		typ.VariantList = append(typ.VariantList, v)
		return false
	})

	return typ
}
