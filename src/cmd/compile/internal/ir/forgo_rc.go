// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ir

import (
	"strings"

	"cmd/compile/internal/types"
)

// FgoNogc reports whether fn is nogc code: a //fgo:nogc root, a nogc copy
// of a function a root reaches, or a closure in either. Only nogc code
// allocates and counts nogc objects.
func (fn *Func) FgoNogc() bool { return fn != nil && fn.Pragma&FgoNogc != 0 }

// FgoNogcPrefix starts the name of the nogc copy of a function.
const FgoNogcPrefix = "_fgonogc_"

// FgoNogcABI reports whether fn is the nogc copy of a function, called
// only from nogc code and following the nogc calling convention.
func (fn *Func) FgoNogcABI() bool {
	return fn.FgoNogc() && fn.Nname != nil && strings.HasPrefix(fn.Sym().Name, FgoNogcPrefix)
}

// FgoRCField returns the field selected by n, and the struct type it is
// selected from, if n selects a field through a nogc pointer (p.f or
// (*p).f with p a pointer to a struct), which in nogc code may be a
// counted object's.
func FgoRCField(n Node) (*types.Type, *types.Field) {
	switch n.Op() {
	case ODOTPTR:
		sel := n.(*SelectorExpr)
		if t := sel.X.Type(); t.FgoIsRCPtr() {
			return t.Elem(), sel.Selection
		}
	case ODOT:
		sel := n.(*SelectorExpr)
		if sel.X.Op() == ODEREF {
			if t := sel.X.(*StarExpr).X.Type(); t.FgoIsRCPtr() {
				return t.Elem(), sel.Selection
			}
		}
	}
	return nil, nil
}

// FgoBufField reports whether n selects a string or scalar slice field
// of a nogc object. Such a field owns its contents, in a buffer outside
// the GC heap: storing into it copies, and reading it gives a view into
// the buffer (or, where the value escapes, a copy on the GC heap).
func FgoBufField(n Node) bool {
	_, f := FgoRCField(n)
	return f != nil && types.FgoBufType(f.Type)
}

// FgoPtrSliceField reports whether n selects a field of a nogc object
// that is a slice of nogc pointers.
func FgoPtrSliceField(n Node) bool {
	_, f := FgoRCField(n)
	return f != nil && types.FgoPtrSliceType(f.Type)
}
