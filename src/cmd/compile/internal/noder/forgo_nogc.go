// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"reflect"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// forgoNogcPrefix starts the name of the nogc copy of a function (see
// ir.Func.FgoNogcABI).
const forgoNogcPrefix = ir.FgoNogcPrefix

// forgoNogc finds the functions marked //fgo:nogc or //fgo:comptime (and
// the methods of types marked //fgo:nogc): the nogc roots. Their allocations, and those
// of every function in the package they can call directly, are reference
// counted instead of garbage collected.
//
// The same code can also run garbage collected, from code that is not
// nogc, so every such callee keeps its ordinary body and gets a nogc
// copy, named forgoNogcPrefix+name, which nogc code calls instead. The
// copies follow the nogc calling convention (owned results, and
// possibly owned parameters); everything else, including the roots
// themselves and closures, follows Go's, and hands the objects it
// returns to the garbage collector. Generic functions are not copied:
// calls to them, like calls to other packages, func values and
// interface methods, leave nogc code.
//
// It reports whether it added any declarations, so that the package
// must be type checked again.
func forgoNogc(m posMap, info *types2.Info, files []*syntax.File) bool {
	nogcTypes := map[string]bool{}
	marked := false
	for _, file := range files {
		for _, d := range file.DeclList {
			switch d := d.(type) {
			case *syntax.TypeDecl:
				if forgoIsNogc(d.Pragma) {
					nogcTypes[d.Name.Value] = true
					marked = true
				}
			case *syntax.FuncDecl:
				marked = marked || forgoIsNogc(d.Pragma) || forgoIsComptime(d.Pragma)
			}
		}
	}
	if !marked {
		return false
	}

	type decl struct {
		file *syntax.File
		fd   *syntax.FuncDecl
	}
	decls := map[*types2.Func]decl{}
	var order []*types2.Func
	var roots []*types2.Func
	strict := map[*types2.Func]bool{}
	for _, file := range files {
		for _, d := range file.DeclList {
			fd, ok := d.(*syntax.FuncDecl)
			if !ok {
				continue
			}
			obj, _ := info.Defs[fd.Name].(*types2.Func)
			if obj == nil {
				continue
			}
			decls[obj] = decl{file, fd}
			order = append(order, obj)
			root := forgoIsNogc(fd.Pragma) || fd.Recv != nil && nogcTypes[forgoRecvTypeName(fd.Recv.Type)]
			if !root && forgoIsComptime(fd.Pragma) && fd.Body != nil && !forgoGeneric(fd) {
				root = true // comptime functions are nogc when called at run time
			}
			if !root {
				continue
			}
			if forgoGeneric(fd) {
				base.ErrorfAt(m.makeXPos(fd.Pos()), 0, "//fgo:nogc: generic function %s is not supported", fd.Name.Value)
				continue
			}
			roots = append(roots, obj)
			// A nogc type's methods are strict: they must not hand
			// what they hold to the GC.
			if forgoIsStrict(fd.Pragma) || fd.Recv != nil && nogcTypes[forgoRecvTypeName(fd.Recv.Type)] {
				strict[obj] = true
			}
		}
	}
	base.ExitIfErrors()

	// callee returns the function or method of this package that call
	// calls statically, if it can have a nogc copy.
	callee := func(call *syntax.CallExpr) *types2.Func {
		fun := syntax.Unparen(call.Fun)
		var obj types2.Object
		switch fun := fun.(type) {
		case *syntax.Name:
			obj = info.Uses[fun]
		case *syntax.SelectorExpr:
			if sel := info.Selections[fun]; sel != nil {
				if sel.Kind() != types2.MethodVal {
					return nil
				}
				if _, iface := sel.Recv().Underlying().(*types2.Interface); iface {
					return nil
				}
				obj = sel.Obj()
			} else {
				obj = info.Uses[fun.Sel] // pkg.F: another package
			}
		}
		fn, _ := obj.(*types2.Func)
		if fn == nil {
			return nil
		}
		d, ok := decls[fn]
		if !ok || d.fd.Body == nil || forgoGeneric(d.fd) {
			return nil
		}
		return fn
	}

	// Find everything the roots can call, and the call sites to point
	// at the copies.
	inSet := map[*types2.Func]bool{}
	renames := map[*syntax.Name]string{}
	work := append([]*types2.Func(nil), roots...)
	isRoot := map[*types2.Func]bool{}
	for _, r := range roots {
		isRoot[r] = true
	}
	visited := map[*types2.Func]bool{}
	callees := map[*types2.Func][]*types2.Func{}
	for len(work) > 0 {
		fn := work[len(work)-1]
		work = work[:len(work)-1]
		if visited[fn] {
			continue
		}
		visited[fn] = true
		body := decls[fn].fd.Body
		if body == nil {
			continue
		}
		var calls []*syntax.CallExpr
		skip := map[*syntax.CallExpr]bool{}
		syntax.Inspect(body, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.CallStmt:
				// go and defer run the call later, through a closure.
				if call, ok := n.Call.(*syntax.CallExpr); ok {
					skip[call] = true
				}
			case *syntax.CallExpr:
				if !skip[n] {
					calls = append(calls, n)
				}
			}
			return true
		})
		for _, call := range calls {
			c := callee(call)
			if c == nil {
				continue
			}
			inSet[c] = true
			callees[fn] = append(callees[fn], c)
			switch fun := syntax.Unparen(call.Fun).(type) {
			case *syntax.Name:
				renames[fun] = forgoNogcPrefix + fun.Value
			case *syntax.SelectorExpr:
				renames[fun.Sel] = forgoNogcPrefix + fun.Sel.Value
			}
			work = append(work, c)
		}
	}

	// Everything a strict root can reach is strict: the hand-offs it
	// would make are errors, whoever calls it.
	for work = append(work[:0], roots...); len(work) > 0; {
		fn := work[len(work)-1]
		work = work[:len(work)-1]
		if !strict[fn] {
			continue
		}
		for _, c := range callees[fn] {
			if !strict[c] {
				strict[c] = true
				work = append(work, c)
			}
		}
	}

	// Copy every callee, pointing its calls at the copies, then point
	// the roots' own calls there too.
	for _, fn := range order {
		if !inSet[fn] {
			continue
		}
		d := decls[fn]
		c := forgoCopyNode(d.fd, renames).(*syntax.FuncDecl)
		c.Name.Value = forgoNogcPrefix + d.fd.Name.Value
		c.Pragma = forgoWithFlag(d.fd.Pragma, ir.FgoNogc|forgoStrictFlag(strict[fn]))
		d.file.DeclList = append(d.file.DeclList, c)
	}
	for _, r := range roots {
		d := decls[r]
		d.fd.Pragma = forgoWithFlag(d.fd.Pragma, ir.FgoNogc|ir.Noinline|forgoStrictFlag(strict[r]))
		syntax.Inspect(d.fd.Body, func(n syntax.Node) bool {
			if n, ok := n.(*syntax.Name); ok {
				if v, ok := renames[n]; ok {
					n.Value = v
				}
			}
			return true
		})
	}
	return len(inSet) > 0 || len(roots) > 0
}

func forgoIsNogc(p syntax.Pragma) bool {
	pr, _ := p.(*pragmas)
	return pr != nil && pr.IsNogc
}

func forgoIsStrict(p syntax.Pragma) bool {
	pr, _ := p.(*pragmas)
	return pr != nil && pr.IsStrict
}

func forgoStrictFlag(strict bool) ir.PragmaFlag {
	if strict {
		return ir.FgoNogcStrict
	}
	return 0
}

func forgoIsComptime(p syntax.Pragma) bool {
	pr, _ := p.(*pragmas)
	return pr != nil && pr.IsComptime
}

// forgoWithFlag returns a copy of pragma p with flag set.
func forgoWithFlag(p syntax.Pragma, flag ir.PragmaFlag) syntax.Pragma {
	var c pragmas
	if pr, _ := p.(*pragmas); pr != nil {
		c = *pr
	}
	c.Flag |= flag
	return &c
}

func forgoGeneric(fd *syntax.FuncDecl) bool {
	if len(fd.TParamList) > 0 {
		return true
	}
	if fd.Recv == nil {
		return false
	}
	t := fd.Recv.Type
	if op, ok := t.(*syntax.Operation); ok && op.Op == syntax.Mul && op.Y == nil {
		t = op.X
	}
	_, ok := syntax.Unparen(t).(*syntax.IndexExpr)
	return ok
}

// forgoRecvTypeName returns the name of the type a method receiver
// expression t names (T or *T).
func forgoRecvTypeName(t syntax.Expr) string {
	t = syntax.Unparen(t)
	if op, ok := t.(*syntax.Operation); ok && op.Op == syntax.Mul && op.Y == nil {
		t = syntax.Unparen(op.X)
	}
	if ix, ok := t.(*syntax.IndexExpr); ok {
		t = ix.X
	}
	if n, ok := t.(*syntax.Name); ok {
		return n.Value
	}
	return ""
}

var forgoNodeType = reflect.TypeFor[syntax.Node]()

// forgoCopyNode deep copies syntax tree n, giving each name in renames
// its new value in the copy. Pointers between nodes of the tree (such as
// a branch statement's target) point between the copies.
func forgoCopyNode(n syntax.Node, renames map[*syntax.Name]string) syntax.Node {
	memo := map[any]reflect.Value{}
	var cp func(v reflect.Value) reflect.Value
	cp = func(v reflect.Value) reflect.Value {
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() || !v.Type().Implements(forgoNodeType) || v.Elem().Kind() != reflect.Struct {
				return v // positions' bases, groups, pragmas: shared
			}
			if c, ok := memo[v.Interface()]; ok {
				return c
			}
			c := reflect.New(v.Type().Elem())
			memo[v.Interface()] = c
			c.Elem().Set(v.Elem())
			for i := 0; i < c.Elem().NumField(); i++ {
				if f := c.Elem().Field(i); f.CanSet() {
					f.Set(cp(f))
				}
			}
			if name, ok := v.Interface().(*syntax.Name); ok {
				if nv, ok := renames[name]; ok {
					c.Interface().(*syntax.Name).Value = nv
				}
			}
			return c
		case reflect.Interface:
			if v.IsNil() {
				return v
			}
			c := reflect.New(v.Type()).Elem()
			c.Set(cp(v.Elem()))
			return c
		case reflect.Slice:
			if v.IsNil() {
				return v
			}
			c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			for i := 0; i < v.Len(); i++ {
				c.Index(i).Set(cp(v.Index(i)))
			}
			return c
		case reflect.Array: // SliceExpr's indexes
			c := reflect.New(v.Type()).Elem()
			for i := 0; i < v.Len(); i++ {
				c.Index(i).Set(cp(v.Index(i)))
			}
			return c
		}
		return v
	}
	return cp(reflect.ValueOf(n)).Interface().(syntax.Node)
}
