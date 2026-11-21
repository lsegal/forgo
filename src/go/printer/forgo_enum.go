// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package printer

import (
	"go/ast"
	"go/token"
)

// enumType prints a forgo enum type. Like an interface, a one-line enum
// in the source stays on one line; otherwise each variant gets its own
// line, keeping blank lines between variants.
func (p *printer) enumType(x *ast.EnumType) {
	p.print(&ast.Ident{NamePos: x.Enum, Name: "enum"})
	hasComments := p.commentBefore(p.posFor(x.Rbrace))
	srcIsOneLine := x.Lbrace.IsValid() && x.Rbrace.IsValid() && p.lineFor(x.Lbrace) == p.lineFor(x.Rbrace)

	if !hasComments && srcIsOneLine {
		p.print(blank)
		p.setPos(x.Lbrace)
		if len(x.Variants) == 0 {
			p.print(token.LBRACE)
		} else {
			p.print(token.LBRACE, blank)
			for i, v := range x.Variants {
				if i > 0 {
					p.print(token.SEMICOLON, blank)
				}
				p.enumVariant(v)
			}
			p.print(blank)
		}
		p.setPos(x.Rbrace)
		p.print(token.RBRACE)
		return
	}

	p.print(blank)
	p.setPos(x.Lbrace)
	p.print(token.LBRACE, indent)
	if hasComments || len(x.Variants) > 0 {
		p.print(formfeed)
	}
	var line int
	for i, v := range x.Variants {
		if i > 0 {
			p.linebreak(p.lineFor(v.Pos()), 1, ignore, p.linesFrom(line) > 0)
		}
		p.recordLine(&line)
		p.enumVariant(v)
	}
	p.print(unindent, formfeed)
	p.setPos(x.Rbrace)
	p.print(token.RBRACE)
}

func (p *printer) enumVariant(v *ast.EnumVariant) {
	p.expr(v.Name)
	if v.Params != nil && len(v.Params.List) > 0 {
		p.parameters(v.Params, funcParam)
	}
}
