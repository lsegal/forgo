// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package postfixif defines an analyzer that suggests collapsing a
// one-statement guard clause ("if COND { STMT }") into forgo's postfix
// "if" statement modifier ("STMT if COND"), per AGENTS.md rule 1c.
package postfixif

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "postfixif",
	Doc:      `suggests "STMT if COND" instead of a one-statement "if COND { STMT }" guard clause`,
	URL:      "https://pkg.go.dev/forgopls/postfixif",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	insp.Preorder([]ast.Node{(*ast.IfStmt)(nil)}, func(n ast.Node) {
		ifStmt := n.(*ast.IfStmt)
		checkIfStmt(pass, ifStmt)
	})

	return nil, nil
}

// checkIfStmt reports a diagnostic for ifStmt if it's a plain "if COND {
// STMT }" (no init clause, no else branch) whose single body statement is
// one of the kinds forgo's postfix "if" accepts.
func checkIfStmt(pass *analysis.Pass, ifStmt *ast.IfStmt) {
	if ifStmt.Init != nil || ifStmt.Else != nil {
		return
	}
	if len(ifStmt.Body.List) != 1 {
		return
	}
	stmt := ifStmt.Body.List[0]
	if !eligible(stmt) {
		return
	}
	reportFix(pass, ifStmt, stmt)
}

// eligible mirrors the parser's own postfix-if eligibility rule (see
// cmd/compile/internal/syntax/parser.go's maybePostfixIf and
// go/parser/parser.go's analogous check): statement kinds that don't
// introduce a new binding into the surrounding scope, excluding a ":="
// short variable declaration (would silently shrink its scope to the
// guard) and "fallthrough" (must remain its switch case's last statement).
func eligible(s ast.Stmt) bool {
	switch x := s.(type) {
	case *ast.ExprStmt, *ast.SendStmt, *ast.IncDecStmt, *ast.ReturnStmt, *ast.ThrowStmt:
		return true
	case *ast.AssignStmt:
		return x.Tok != token.DEFINE
	case *ast.BranchStmt:
		return x.Tok != token.FALLTHROUGH
	}
	return false
}

func reportFix(pass *analysis.Pass, ifStmt *ast.IfStmt, stmt ast.Stmt) {
	stmtSrc, err := sourceText(pass, stmt.Pos(), stmt.End())
	if err != nil {
		return
	}
	condSrc, err := sourceText(pass, ifStmt.Cond.Pos(), ifStmt.Cond.End())
	if err != nil {
		return
	}

	newText := append([]byte(nil), stmtSrc...)
	newText = append(newText, " if "...)
	newText = append(newText, condSrc...)

	pass.Report(analysis.Diagnostic{
		Pos:     ifStmt.Pos(),
		End:     ifStmt.End(),
		Message: "can use postfix if instead of a one-statement guard clause",
		SuggestedFixes: []analysis.SuggestedFix{{
			Message: "Replace with postfix if",
			TextEdits: []analysis.TextEdit{{
				Pos:     ifStmt.Pos(),
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
