// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !compiler_bootstrap

package main

import "go/ast"

// walkForgo walks the forgo-only node types (postfix if, throw and ?) for
// walk, reporting whether x was one of them. The bootstrap toolchain's
// go/ast doesn't define them, hence the separate file.
func (f *File) walkForgo(x any, visit func(*File, any, astContext)) bool {
	switch n := x.(type) {
	case *ast.TryExpr:
		f.walk(&n.X, ctxExpr, visit)
	case *ast.ThrowStmt:
		f.walk(&n.X, ctxExpr, visit)
	case *ast.PostfixIfStmt:
		f.walk(n.Stmt, ctxStmt, visit)
		f.walk(&n.Cond, ctxExpr, visit)
	default:
		return false
	}
	return true
}
