// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package enumdefault

import (
	_ "embed"
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/internal/analysis/analyzerutil"
)

//go:embed doc.go
var doc string

var Analyzer = &analysis.Analyzer{
	Name:     "enumdefault",
	Doc:      analyzerutil.MustExtractDoc(doc, "enumdefault"),
	URL:      "https://github.com/lsegal/forgo#enum--sum-types-with-exhaustive-switch",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	nodeFilter := []ast.Node{
		(*ast.TypeSwitchStmt)(nil),
		(*ast.SwitchStmt)(nil),
	}
	inspect.Preorder(nodeFilter, func(n ast.Node) {
		switch s := n.(type) {
		case *ast.TypeSwitchStmt:
			if x := typeSwitchSubject(s); x != nil {
				check(pass, pass.TypesInfo.TypeOf(x), s.Body, false)
			}
		case *ast.SwitchStmt:
			if s.Tag != nil {
				check(pass, pass.TypesInfo.TypeOf(s.Tag), s.Body, true)
			}
		}
	})
	return nil, nil
}

// typeSwitchSubject returns the operand x of the x.(type) guard of s.
func typeSwitchSubject(s *ast.TypeSwitchStmt) ast.Expr {
	var e ast.Expr
	switch a := s.Assign.(type) {
	case *ast.ExprStmt:
		e = a.X
	case *ast.AssignStmt:
		if len(a.Rhs) == 1 {
			e = a.Rhs[0]
		}
	}
	if ta, ok := ast.Unparen(e).(*ast.TypeAssertExpr); ok {
		return ta.X
	}
	return nil
}

// check reports the default clause of a switch with the given body over
// a value of type enum, if enum is a forgo enum type. exprSwitch reports
// whether the switch is an expression switch, whose cases are values
// rather than variant names.
func check(pass *analysis.Pass, enum types.Type, body *ast.BlockStmt, exprSwitch bool) {
	es := types.ForgoEnum(enum)
	if es == nil {
		return
	}
	var def *ast.CaseClause
	covered := make(map[int]bool)
	for _, c := range body.List {
		clause, ok := c.(*ast.CaseClause)
		if !ok {
			continue
		}
		if clause.List == nil {
			def = clause
			continue
		}
		for _, e := range clause.List {
			if i := caseVariant(pass, es, e, exprSwitch); i > 0 {
				covered[i] = true
			}
		}
	}
	if def == nil {
		return
	}

	name := types.TypeString(enum, func(p *types.Package) string {
		if p == pass.Pkg {
			return ""
		}
		return p.Name()
	})
	var hidden []string
	for i := 1; i < es.NumFields(); i++ {
		if !covered[i] {
			hidden = append(hidden, name+"."+es.Field(i).Name())
		}
	}
	if len(hidden) == 0 {
		pass.Reportf(def.Pos(), "default clause is unreachable, but would hide variants added to %s later", name)
		return
	}
	variants := "variant " + hidden[0]
	if len(hidden) > 1 {
		variants = "variants " + strings.Join(hidden, ", ")
	}
	pass.Reportf(def.Pos(), "default clause hides %s and any added later", variants)
}

// caseVariant returns the index of the field of enum struct es holding
// the variant that case e names, or -1. In a type switch, a case names a
// variant as Shape.Circle, pkg.Shape.Circle or Option[int].Some. In an
// expression switch, only a unit variant's value (Color.Red) matches
// every value of a variant; other cases match particular values or are
// not known statically.
func caseVariant(pass *analysis.Pass, es *types.Struct, e ast.Expr, exprSwitch bool) int {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return -1
	}
	if tv, ok := pass.TypesInfo.Types[sel.X]; !ok || !tv.IsType() {
		return -1
	}
	if exprSwitch {
		if _, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Var); !ok {
			return -1
		}
	}
	return types.ForgoEnumVariant(es, sel.Sel.Name)
}
