// Copyright 2026 The Fore Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package comptimeconst defines an analyzer that suggests marking a
// package-level function as "//fgo:comptime" and promoting a "var"
// computed purely from its call (with constant arguments) to a "const",
// per AGENTS.md rule 2. It recognizes two shapes: a direct initializer
// ("var x = f(...)") and the common package-initialization idiom of
// assigning to an uninitialized package var from a dedicated "func init()"
// ("var x T" + "func init() { x = f(...) }").
package comptimeconst

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

var Analyzer = &analysis.Analyzer{
	Name: "comptimeconst",
	Doc:  `suggests "//fgo:comptime" for a function used to compute a const from constant arguments`,
	URL:  "https://pkg.go.dev/forgopls/comptimeconst",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	funcDecls := make(map[*types.Func]*ast.FuncDecl)
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			if obj, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func); ok {
				funcDecls[obj] = fd
			}
		}
	}

	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR || len(gd.Specs) != 1 {
				continue
			}
			vs, ok := gd.Specs[0].(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 || vs.Names[0].Name == "_" {
				continue
			}
			call, ok := vs.Values[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			fnDecl := resolveComptimeCallee(pass, funcDecls, call)
			if fnDecl == nil {
				continue
			}
			reportComptimeFix(pass, gd, vs, fnDecl)
		}
	}

	zeroVars := findZeroVars(pass)
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			initDecl, ok := decl.(*ast.FuncDecl)
			if !ok || initDecl.Recv != nil || initDecl.Name.Name != "init" {
				continue
			}
			if initDecl.Body == nil || len(initDecl.Body.List) != 1 {
				continue
			}
			assign, ok := initDecl.Body.List[0].(*ast.AssignStmt)
			if !ok || assign.Tok != token.ASSIGN || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				continue
			}
			lhsIdent, ok := assign.Lhs[0].(*ast.Ident)
			if !ok {
				continue
			}
			varObj, ok := pass.TypesInfo.Uses[lhsIdent].(*types.Var)
			if !ok {
				continue
			}
			vd, ok := zeroVars[varObj]
			if !ok {
				continue
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			fnDecl := resolveComptimeCallee(pass, funcDecls, call)
			if fnDecl == nil {
				continue
			}
			reportComptimeInitFix(pass, vd.gd, vd.vs, fnDecl, initDecl, call)
		}
	}
	return nil, nil
}

// resolveComptimeCallee reports the declaration of call's callee if it's
// eligible for comptime folding (a package-local, not-yet-annotated
// function within the interpreter's supported subset) and every argument
// is a constant expression; otherwise nil.
func resolveComptimeCallee(pass *analysis.Pass, funcDecls map[*types.Func]*ast.FuncDecl, call *ast.CallExpr) *ast.FuncDecl {
	fnIdent, ok := call.Fun.(*ast.Ident)
	if !ok {
		return nil
	}
	fnObj, ok := pass.TypesInfo.Uses[fnIdent].(*types.Func)
	if !ok {
		return nil
	}
	fnDecl, ok := funcDecls[fnObj]
	if !ok || hasComptimePragma(fnDecl) {
		return nil
	}
	if !allConstArgs(pass, call.Args) || !comptimeEligible(pass, fnDecl) {
		return nil
	}
	return fnDecl
}

// varDecl is a package-level "var name [type]" with no initializer.
type varDecl struct {
	gd *ast.GenDecl
	vs *ast.ValueSpec
}

func findZeroVars(pass *analysis.Pass) map[*types.Var]varDecl {
	out := make(map[*types.Var]varDecl)
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR || len(gd.Specs) != 1 {
				continue
			}
			vs, ok := gd.Specs[0].(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 0 || vs.Names[0].Name == "_" {
				continue
			}
			if obj, ok := pass.TypesInfo.Defs[vs.Names[0]].(*types.Var); ok {
				out[obj] = varDecl{gd, vs}
			}
		}
	}
	return out
}

func hasComptimePragma(fd *ast.FuncDecl) bool {
	if fd.Doc == nil {
		return false
	}
	for _, c := range fd.Doc.List {
		if strings.TrimSpace(strings.TrimPrefix(c.Text, "//")) == "fgo:comptime" {
			return true
		}
	}
	return false
}

func allConstArgs(pass *analysis.Pass, args []ast.Expr) bool {
	for _, a := range args {
		if pass.TypesInfo.Types[a].Value == nil {
			return false
		}
	}
	return true
}

// comptimeEligible reports whether fnDecl's signature and body stay inside
// the small subset the forgo comptime interpreter supports (AGENTS.md rule
// 2): int/float/string/bool locals and arithmetic, if, 3-clause for,
// compound assignment, calls to other package-local functions (assumed
// themselves comptime-able) or fmt.Sprintf/strconv.Itoa, and string
// indexing/len. No closures, generics, maps, slices, method calls, or range.
func comptimeEligible(pass *analysis.Pass, fnDecl *ast.FuncDecl) bool {
	if fnDecl.Body == nil || fnDecl.Type.TypeParams != nil {
		return false
	}
	if !allBasicFields(pass, fnDecl.Type.Params) || !allBasicFields(pass, fnDecl.Type.Results) {
		return false
	}
	if resultCount(fnDecl.Type.Results) != 1 {
		return false
	}
	return checkStmt(pass, fnDecl.Body)
}

func allBasicFields(pass *analysis.Pass, fl *ast.FieldList) bool {
	if fl == nil {
		return true
	}
	for _, f := range fl.List {
		if !isBasicType(pass.TypesInfo.TypeOf(f.Type)) {
			return false
		}
	}
	return true
}

func resultCount(fl *ast.FieldList) int {
	if fl == nil {
		return 0
	}
	n := 0
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			n++
		} else {
			n += len(f.Names)
		}
	}
	return n
}

func isBasicType(t types.Type) bool {
	b, ok := t.(*types.Basic)
	if !ok {
		return false
	}
	switch b.Kind() {
	case types.Bool,
		types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr,
		types.Float32, types.Float64,
		types.String:
		return true
	}
	return false
}

func checkStmt(pass *analysis.Pass, s ast.Stmt) bool {
	switch x := s.(type) {
	case nil:
		return true
	case *ast.BlockStmt:
		for _, st := range x.List {
			if !checkStmt(pass, st) {
				return false
			}
		}
		return true
	case *ast.ExprStmt:
		return checkExpr(pass, x.X)
	case *ast.AssignStmt:
		for _, e := range x.Lhs {
			if !checkExpr(pass, e) {
				return false
			}
		}
		for _, e := range x.Rhs {
			if !checkExpr(pass, e) {
				return false
			}
		}
		return true
	case *ast.DeclStmt:
		gd, ok := x.Decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.VAR && gd.Tok != token.CONST) {
			return false
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				return false
			}
			if vs.Type != nil && !isBasicType(pass.TypesInfo.TypeOf(vs.Type)) {
				return false
			}
			for _, v := range vs.Values {
				if !checkExpr(pass, v) {
					return false
				}
			}
		}
		return true
	case *ast.IfStmt:
		if x.Init != nil && !checkStmt(pass, x.Init) {
			return false
		}
		if !checkExpr(pass, x.Cond) {
			return false
		}
		if !checkStmt(pass, x.Body) {
			return false
		}
		return checkStmt(pass, x.Else)
	case *ast.ForStmt:
		if x.Init != nil && !checkStmt(pass, x.Init) {
			return false
		}
		if x.Cond != nil && !checkExpr(pass, x.Cond) {
			return false
		}
		if x.Post != nil && !checkStmt(pass, x.Post) {
			return false
		}
		return checkStmt(pass, x.Body)
	case *ast.IncDecStmt:
		return checkExpr(pass, x.X)
	case *ast.ReturnStmt:
		for _, r := range x.Results {
			if !checkExpr(pass, r) {
				return false
			}
		}
		return true
	case *ast.BranchStmt:
		return x.Tok == token.BREAK || x.Tok == token.CONTINUE
	case *ast.EmptyStmt:
		return true
	}
	return false
}

func checkExpr(pass *analysis.Pass, e ast.Expr) bool {
	switch x := e.(type) {
	case nil:
		return true
	case *ast.Ident:
		return true
	case *ast.BasicLit:
		return true
	case *ast.ParenExpr:
		return checkExpr(pass, x.X)
	case *ast.BinaryExpr:
		return checkExpr(pass, x.X) && checkExpr(pass, x.Y)
	case *ast.UnaryExpr:
		return checkExpr(pass, x.X)
	case *ast.IndexExpr:
		b, ok := pass.TypesInfo.TypeOf(x.X).(*types.Basic)
		if !ok || b.Kind() != types.String {
			return false
		}
		return checkExpr(pass, x.X) && checkExpr(pass, x.Index)
	case *ast.CallExpr:
		if !allowedCall(pass, x) {
			return false
		}
		for _, a := range x.Args {
			if !checkExpr(pass, a) {
				return false
			}
		}
		return true
	}
	return false
}

// allowedCall reports whether call is one of: len(...), a basic-type
// conversion, a call to another function in the same package, or
// fmt.Sprintf/strconv.Itoa.
func allowedCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if fun.Name == "len" {
			return true
		}
		if tn, ok := pass.TypesInfo.Uses[fun].(*types.TypeName); ok {
			return isBasicType(tn.Type())
		}
		if fn, ok := pass.TypesInfo.Uses[fun].(*types.Func); ok {
			return fn.Pkg() == pass.Pkg
		}
		return false
	case *ast.SelectorExpr:
		pkgIdent, ok := fun.X.(*ast.Ident)
		if !ok {
			return false
		}
		pn, ok := pass.TypesInfo.Uses[pkgIdent].(*types.PkgName)
		if !ok {
			return false
		}
		path := pn.Imported().Path()
		return (path == "fmt" && fun.Sel.Name == "Sprintf") || (path == "strconv" && fun.Sel.Name == "Itoa")
	}
	return false
}

func reportComptimeFix(pass *analysis.Pass, gd *ast.GenDecl, vs *ast.ValueSpec, fnDecl *ast.FuncDecl) {
	pass.Report(analysis.Diagnostic{
		Pos:     gd.Pos(),
		End:     vs.End(),
		Message: fmt.Sprintf("%s is computed from constant arguments to %s; consider //fgo:comptime", vs.Names[0].Name, fnDecl.Name.Name),
		SuggestedFixes: []analysis.SuggestedFix{{
			Message: fmt.Sprintf("Mark %s as //fgo:comptime and %s as const", fnDecl.Name.Name, vs.Names[0].Name),
			TextEdits: []analysis.TextEdit{
				{
					Pos:     fnDecl.Pos(),
					End:     fnDecl.Pos(),
					NewText: []byte("//fgo:comptime\n"),
				},
				{
					Pos:     gd.TokPos,
					End:     gd.TokPos + token.Pos(len("var")),
					NewText: []byte("const"),
				},
			},
		}},
	})
}

// reportComptimeInitFix handles the "var x T" + "func init() { x =
// f(...) }" idiom: since the whole point of that idiom is to run f once at
// startup, and f now folds to a compile-time constant, the fix inlines f's
// call directly into the var (promoted to a const) and deletes init
// entirely, rather than just marking things and leaving two disjoint
// declarations for the user to reconcile by hand.
func reportComptimeInitFix(pass *analysis.Pass, gd *ast.GenDecl, vs *ast.ValueSpec, fnDecl, initDecl *ast.FuncDecl, call *ast.CallExpr) {
	callSrc, err := sourceText(pass, call.Pos(), call.End())
	if err != nil {
		return
	}

	var newDecl bytes.Buffer
	newDecl.WriteString("const ")
	newDecl.WriteString(vs.Names[0].Name)
	if vs.Type != nil {
		typeSrc, err := sourceText(pass, vs.Type.Pos(), vs.Type.End())
		if err != nil {
			return
		}
		newDecl.WriteByte(' ')
		newDecl.Write(typeSrc)
	}
	newDecl.WriteString(" = ")
	newDecl.Write(callSrc)

	pass.Report(analysis.Diagnostic{
		Pos:     gd.Pos(),
		End:     initDecl.End(),
		Message: fmt.Sprintf("%s is computed once in init from constant arguments to %s; consider //fgo:comptime", vs.Names[0].Name, fnDecl.Name.Name),
		SuggestedFixes: []analysis.SuggestedFix{{
			Message: fmt.Sprintf("Mark %s as //fgo:comptime, %s as const, and remove init", fnDecl.Name.Name, vs.Names[0].Name),
			TextEdits: []analysis.TextEdit{
				{
					Pos:     fnDecl.Pos(),
					End:     fnDecl.Pos(),
					NewText: []byte("//fgo:comptime\n"),
				},
				{
					Pos:     gd.Pos(),
					End:     gd.End(),
					NewText: newDecl.Bytes(),
				},
				{
					Pos:     initDecl.Pos(),
					End:     initDecl.End(),
					NewText: nil,
				},
			},
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
