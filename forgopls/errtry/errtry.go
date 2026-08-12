// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package errtry defines an analyzer that suggests replacing a manual
// "if err != nil { return zero..., err }" check with forgo's "?"
// error-propagation operator.
package errtry

import (
	"bytes"
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "errtry",
	Doc:      `suggests replacing "if err != nil { return zero..., err }" with the "?" operator`,
	URL:      "https://pkg.go.dev/forgopls/errtry",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	insp.Preorder([]ast.Node{(*ast.FuncDecl)(nil), (*ast.FuncLit)(nil)}, func(n ast.Node) {
		var body *ast.BlockStmt
		var results *ast.FieldList
		switch f := n.(type) {
		case *ast.FuncDecl:
			body, results = f.Body, f.Type.Results
		case *ast.FuncLit:
			body, results = f.Body, f.Type.Results
		}
		if body == nil || !hasNamedErrorResult(results) {
			return
		}
		checkBlock(pass, body, results)
	})

	return nil, nil
}

// hasNamedErrorResult reports whether results ends in a named result of
// type error, the precondition forgo's "?" imposes on its enclosing
// function (see AGENTS.md, rule 1).
func hasNamedErrorResult(results *ast.FieldList) bool {
	if results == nil || len(results.List) == 0 {
		return false
	}
	last := results.List[len(results.List)-1]
	if len(last.Names) != 1 || last.Names[0].Name == "_" {
		return false
	}
	id, ok := last.Type.(*ast.Ident)
	return ok && id.Name == "error" && id.Obj == nil
}

// checkBlock scans a block's own statement list (not nested blocks, which
// are handled when the inspector visits their own enclosing FuncDecl/
// FuncLit, or recursively below for if/for/etc bodies) for the
// assign-then-check-err pattern.
func checkBlock(pass *analysis.Pass, block *ast.BlockStmt, results *ast.FieldList) {
	list := block.List
	for i := 0; i+1 < len(list); i++ {
		assign, ok := list[i].(*ast.AssignStmt)
		if !ok {
			continue
		}
		ifStmt, ok := list[i+1].(*ast.IfStmt)
		if !ok {
			continue
		}
		checkPair(pass, assign, ifStmt, results)
	}
	// Recurse into nested control-flow bodies so the pattern is found
	// wherever it appears, not just at the top level of the function.
	for _, s := range list {
		walkNested(pass, s, results)
	}
}

func walkNested(pass *analysis.Pass, s ast.Stmt, results *ast.FieldList) {
	switch x := s.(type) {
	case *ast.BlockStmt:
		checkBlock(pass, x, results)
	case *ast.IfStmt:
		checkBlock(pass, x.Body, results)
		switch e := x.Else.(type) {
		case *ast.BlockStmt:
			checkBlock(pass, e, results)
		case *ast.IfStmt:
			walkNested(pass, e, results)
		}
	case *ast.ForStmt:
		checkBlock(pass, x.Body, results)
	case *ast.RangeStmt:
		checkBlock(pass, x.Body, results)
	case *ast.SwitchStmt:
		for _, c := range x.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok {
				for _, cs := range cc.Body {
					walkNested(pass, cs, results)
				}
			}
		}
	case *ast.LabeledStmt:
		walkNested(pass, x.Stmt, results)
	}
}

// checkPair looks for:
//
//	lhs..., err := call(...)
//	if err != nil {
//		return zero..., err
//	}
//
// immediately followed by each other, and reports a diagnostic with a
// suggested fix that rewrites both statements to use "?" instead.
func checkPair(pass *analysis.Pass, assign *ast.AssignStmt, ifStmt *ast.IfStmt, results *ast.FieldList) {
	if ifStmt.Init != nil || ifStmt.Else != nil {
		return
	}
	if len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
		return
	}
	if _, ok := assign.Rhs[0].(*ast.CallExpr); !ok {
		return
	}

	errIdent, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
	if !ok || errIdent.Name == "_" {
		return
	}
	errObj := pass.TypesInfo.ObjectOf(errIdent)
	if errObj == nil || errObj.Type() == nil || errObj.Type().String() != "error" {
		return
	}

	if !isErrNeqNil(ifStmt.Cond, errObj, pass) {
		return
	}
	if len(ifStmt.Body.List) != 1 {
		return
	}
	ret, ok := ifStmt.Body.List[0].(*ast.ReturnStmt)
	if !ok {
		return
	}

	if len(ret.Results) != resultCount(results) || len(ret.Results) == 0 {
		return
	}
	// The final returned value must be the propagated error itself...
	lastIdent, ok := ret.Results[len(ret.Results)-1].(*ast.Ident)
	if !ok || pass.TypesInfo.ObjectOf(lastIdent) != errObj {
		return
	}
	// ...and every earlier one must be the obvious zero value of its
	// result, since that's what naked-return zero initialization (which
	// "?" relies on) would produce.
	for _, r := range ret.Results[:len(ret.Results)-1] {
		if !isObviousZero(r) {
			return
		}
	}

	// err must not be read anywhere else: once we rewrite, it stops
	// existing as a variable.
	if countUses(pass, assign, ifStmt, errObj) > 0 {
		return
	}

	reportFix(pass, assign, ifStmt, errIdent)
}

func resultCount(results *ast.FieldList) int {
	n := 0
	for _, f := range results.List {
		if len(f.Names) == 0 {
			n++
		} else {
			n += len(f.Names)
		}
	}
	return n
}

func isErrNeqNil(cond ast.Expr, errObj types.Object, pass *analysis.Pass) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	x, xok := bin.X.(*ast.Ident)
	y, yok := bin.Y.(*ast.Ident)
	switch {
	case xok && pass.TypesInfo.ObjectOf(x) == errObj:
		return yok && y.Name == "nil"
	case yok && pass.TypesInfo.ObjectOf(y) == errObj:
		return xok && x.Name == "nil"
	}
	return false
}

// isObviousZero reports whether e is a simple, syntactic zero value: nil,
// 0, 0.0, "", false, or an empty composite literal T{}. It does not use
// type information, so it only recognizes zero values spelled the way a
// human naturally would; anything else is treated conservatively as "not
// provably zero" and the suggestion is skipped.
func isObviousZero(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "nil" || x.Name == "false"
	case *ast.BasicLit:
		switch x.Kind {
		case token.INT, token.FLOAT:
			return isZeroNumber(x.Value)
		case token.STRING:
			return x.Value == `""` || x.Value == "``"
		}
		return false
	case *ast.CompositeLit:
		return len(x.Elts) == 0
	case *ast.UnaryExpr:
		if x.Op == token.SUB {
			return isObviousZero(x.X)
		}
	}
	return false
}

func isZeroNumber(lit string) bool {
	for _, c := range lit {
		if c != '0' && c != '.' {
			return false
		}
	}
	return true
}

// countUses counts identifier references to obj outside of [assign, ifStmt)
// itself, i.e. the assignment and its immediately following err check
// (which are consumed by the rewrite and don't count as "elsewhere").
func countUses(pass *analysis.Pass, assign *ast.AssignStmt, ifStmt *ast.IfStmt, obj types.Object) int {
	n := 0
	for id, o := range pass.TypesInfo.Uses {
		if o == obj && (id.Pos() < assign.Pos() || id.Pos() >= ifStmt.End()) {
			n++
		}
	}
	return n
}

func reportFix(pass *analysis.Pass, assign *ast.AssignStmt, ifStmt *ast.IfStmt, errIdent *ast.Ident) {
	call := assign.Rhs[0].(*ast.CallExpr)
	callSrc, err := sourceText(pass, call.Pos(), call.End())
	if err != nil {
		return
	}

	var newText []byte
	if len(assign.Lhs) == 1 {
		// err was the only result: "call(...)?" as a whole statement.
		newText = append([]byte(nil), callSrc...)
		newText = append(newText, '?')
	} else {
		var lhs bytes.Buffer
		for i, l := range assign.Lhs[:len(assign.Lhs)-1] {
			if i > 0 {
				lhs.WriteString(", ")
			}
			src, err := sourceText(pass, l.Pos(), l.End())
			if err != nil {
				return
			}
			lhs.Write(src)
		}
		lhs.WriteString(" ")
		lhs.WriteString(assign.Tok.String())
		lhs.WriteString(" ")
		lhs.Write(callSrc)
		lhs.WriteByte('?')
		newText = lhs.Bytes()
	}

	pass.Report(analysis.Diagnostic{
		Pos:     assign.Pos(),
		End:     ifStmt.End(),
		Message: "can use ? instead of an explicit if err != nil check",
		SuggestedFixes: []analysis.SuggestedFix{{
			Message: "Replace with ?",
			TextEdits: []analysis.TextEdit{{
				Pos:     assign.Pos(),
				End:     ifStmt.End(),
				NewText: newText,
			}},
		}},
	})
}

func sourceText(pass *analysis.Pass, start, end token.Pos) ([]byte, error) {
	file := pass.Fset.File(start)
	content, err := pass.ReadFile(file.Name())
	if err != nil {
		return nil, err
	}
	return content[file.Offset(start):file.Offset(end)], nil
}
