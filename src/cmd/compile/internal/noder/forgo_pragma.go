// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

// forgoPragma recognizes forgo's own directive comments ("//forgo:comptime",
// "//forgo:macro") and records them on pragma. It reports whether text was
// a forgo directive, so noder.pragma can skip its normal //go:... switch
// for it.
func forgoPragma(pragma *pragmas, text string) bool {
	switch text {
	case "forgo:comptime":
		pragma.IsComptime = true
	case "forgo:macro":
		pragma.IsMacro = true
	default:
		return false
	}
	return true
}

// ForgoComptime reports whether the declaration carries //forgo:comptime.
// Implements syntax.ForgoPragma.
func (p *pragmas) ForgoComptime() bool { return p != nil && p.IsComptime }

// ForgoMacro reports whether the declaration carries //forgo:macro.
// Implements syntax.ForgoPragma.
func (p *pragmas) ForgoMacro() bool { return p != nil && p.IsMacro }

// forgoTransform rewrites every forgo-specific construct (macro calls, the
// postfix-if statement modifier, the throw statement, the ? operator)
// found across noders' parsed files into ordinary syntax the rest of the
// compiler already understands. It must run after parsing and before type
// checking.
//
// forgoLowerPostfixIf runs before forgoLowerThrow/forgoLowerTry: it
// unwraps `STMT if COND` into `if COND { STMT }` first, so those two
// passes see the wrapped STMT as an ordinary statement inside a regular
// *syntax.IfStmt, which they already know how to walk into.
func forgoTransform(m posMap, noders []*noder) {
	forgoExpandMacros(noders)
	forgoLowerPostfixIf(noders)
	forgoLowerThrow(m, noders)
	forgoLowerTry(m, noders)
}
