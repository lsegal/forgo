// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fgoeval holds what forgo's //fgo:comptime interpreters share,
// independent of the syntax trees they walk: the values they compute,
// Go's semantics for operations on them, the natively evaluated library
// calls (fmt.Sprintf, strconv.Itoa, comptime/embed, comptime/json), and
// the conversion of a result to a go/constant.Value. The compiler's
// interpreter (cmd/compile/internal/forgo) walks its syntax package's
// trees; go/types walks go/ast's, so that tools built on it (forgopls)
// fold the same constants.
package fgoeval

import (
	stdjson "encoding/json"
	"fmt"
	"go/constant"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Value is a go/constant.Value (scalar comptime evaluation), an ObjectVal
// or ArrayVal (composite comptime evaluation, e.g. a struct/map or
// slice/array literal, or the result of json.Unmarshal), or a NodeVal
// (macro evaluation).
type Value interface{}

// ObjectVal is a struct- or map[string]T-shaped composite value: a struct
// or map composite literal evaluated at compile time, or a JSON object
// produced by comptime/json's Unmarshal. Field access (x.Field) and string
// indexing (x["field"]) both read from fields, keyed by struct field name
// (for a struct literal), map key (for a map literal), or JSON object key
// (for an Unmarshal result).
type ObjectVal struct {
	Keys   []string // preserves literal/decode order, for json.Marshal output
	Fields map[string]Value
}

// ArrayVal is a slice-, array-, or JSON-array-shaped composite value: a
// slice/array composite literal evaluated at compile time, or a JSON array
// produced by comptime/json's Unmarshal.
type ArrayVal struct {
	Elems []Value
}

type Error struct{ Err error }

func Fail(format string, a ...any) {
	panic(Error{fmt.Errorf(format, a...)})
}

// BinaryOp applies op to x and y with Go's semantics for values of basic
// type t ("" for untyped constants): integer division truncates, shifts
// shift, and integer results wrap around to t's size.
func BinaryOp(x constant.Value, op token.Token, y constant.Value, t string) constant.Value {
	switch op {
	case token.SHL, token.SHR:
		n, ok := constant.Uint64Val(constant.ToInt(y))
		if !ok || n > 1<<16 {
			Fail("forgo: invalid shift count in comptime evaluation")
		}
		return Convert(constant.Shift(constant.ToInt(x), op, uint(n)), t)
	case token.QUO:
		if constant.Sign(y) == 0 {
			Fail("forgo: division by zero in comptime evaluation")
		}
		if x.Kind() == constant.Int && y.Kind() == constant.Int || IsIntType(t) {
			op = token.QUO_ASSIGN // integer division
			x, y = constant.ToInt(x), constant.ToInt(y)
		}
	case token.REM:
		if constant.Sign(y) == 0 {
			Fail("forgo: division by zero in comptime evaluation")
		}
	}
	return Convert(constant.BinaryOp(x, op, y), t)
}

// basicInts gives the size in bits and signedness of each integer type.
var basicInts = map[string]struct {
	bits   uint
	signed bool
}{
	"int": {64, true}, "int8": {8, true}, "int16": {16, true}, "int32": {32, true}, "int64": {64, true}, "rune": {32, true},
	"uint": {64, false}, "uint8": {8, false}, "uint16": {16, false}, "uint32": {32, false}, "uint64": {64, false},
	"uintptr": {64, false}, "byte": {8, false},
}

func IsIntType(t string) bool { _, ok := basicInts[t]; return ok }

func IsFloatType(t string) bool { return t == "float32" || t == "float64" }

func IsBasicType(t string) bool {
	return IsIntType(t) || IsFloatType(t) || t == "string" || t == "bool"
}

// Convert converts v to basic type t as a Go conversion would (integers
// wrap around, floats truncate toward zero, an integer becomes a
// one-rune string); other types leave v as it is.
func Convert(v constant.Value, t string) constant.Value {
	switch {
	case IsIntType(t):
		switch v.Kind() {
		case constant.Float:
			v = constant.ToInt(constant.MakeFromLiteral(truncFloat(v), token.INT, 0))
		case constant.Int:
		default:
			return v
		}
		info := basicInts[t]
		mod := constant.Shift(constant.MakeInt64(1), token.SHL, info.bits)
		v = constant.BinaryOp(v, token.AND, constant.BinaryOp(mod, token.SUB, constant.MakeInt64(1)))
		if info.signed && constant.Compare(v, token.GEQ, constant.Shift(constant.MakeInt64(1), token.SHL, info.bits-1)) {
			v = constant.BinaryOp(v, token.SUB, mod)
		}
		return v
	case IsFloatType(t):
		if v.Kind() == constant.Int || v.Kind() == constant.Float {
			return constant.ToFloat(v)
		}
	case t == "string":
		if v.Kind() == constant.Int {
			n, _ := constant.Int64Val(v)
			return constant.MakeString(string(rune(n)))
		}
	}
	return v
}

// truncFloat returns the integer part of float v as a decimal literal.
func truncFloat(v constant.Value) string {
	f, _ := constant.Float64Val(v)
	return fmt.Sprintf("%.0f", math.Trunc(f))
}

// DefaultType is the type an untyped constant takes in a := declaration.
func DefaultType(v constant.Value) string {
	switch v.Kind() {
	case constant.Int:
		return "int"
	case constant.Float:
		return "float64"
	case constant.String:
		return "string"
	case constant.Bool:
		return "bool"
	}
	return ""
}

// ValueType is DefaultType for any value, "" for composites.
func ValueType(v Value) string {
	if cv, ok := v.(constant.Value); ok {
		return DefaultType(cv)
	}
	return ""
}

// HasNative reports whether pkg.fn is one of forgo's natively evaluated
// compile-time helpers, without actually invoking it. It lets callers (see
// types2's forgoEvalConstCall) tell a real forgo native call apart from an
// arbitrary function call that just happens to not be constant, before
// committing to evaluating it.
func HasNative(pkg, fn string) bool {
	switch pkg + "." + fn {
	case "fmt.Sprintf", "strconv.Itoa",
		"embed.ReadFile", "embed.ReadFileRange", "embed.Exists",
		"embed.IsDir", "embed.ReadDir", "embed.Getwd", "embed.Load",
		"json.Marshal", "json.Unmarshal":
		return true
	}
	return false
}

// nativeCall implements the fixed allow-list of package-qualified
// functions forgo evaluates natively at compile time, either as calls
// inside a //fgo:comptime function body (see evalStdlibCall) or, via a
// chain of field/index access, written directly as a const initializer
// (see types2's forgoEvalConstCall). pos is the call site, used by the
// "embed" (comptime/embed) helpers to resolve relative file paths.
func NativeCall(dir, pkg, fn string, args []Value) (Value, bool) {
	switch pkg + "." + fn {
	case "fmt.Sprintf":
		if len(args) == 0 {
			Fail("forgo: fmt.Sprintf requires a format string")
		}
		format := ScalarString(args[0], "fmt.Sprintf")
		rest := make([]any, len(args)-1)
		for i, a := range args[1:] {
			rest[i] = Native(Scalar(a, "fmt.Sprintf"))
		}
		return constant.MakeString(fmt.Sprintf(format, rest...)), true

	case "strconv.Itoa":
		if len(args) != 1 {
			Fail("forgo: strconv.Itoa takes exactly one argument")
		}
		return constant.MakeString(fmt.Sprintf("%d", ScalarInt(args[0], "strconv.Itoa"))), true

	case "embed.ReadFile":
		path := ScalarString(args[0], "embed.ReadFile")
		data, err := os.ReadFile(resolveEmbedPath(dir, path))
		if err != nil {
			Fail("forgo: embed.ReadFile(%q): %s", path, err)
		}
		return constant.MakeString(string(data)), true

	case "embed.ReadFileRange":
		if len(args) != 3 {
			Fail("forgo: embed.ReadFileRange takes exactly 3 arguments")
		}
		path := ScalarString(args[0], "embed.ReadFileRange")
		offset := ScalarInt(args[1], "embed.ReadFileRange")
		length := ScalarInt(args[2], "embed.ReadFileRange")
		data, err := os.ReadFile(resolveEmbedPath(dir, path))
		if err != nil {
			Fail("forgo: embed.ReadFileRange(%q): %s", path, err)
		}
		if offset < 0 || length < 0 || offset+length > int64(len(data)) {
			Fail("forgo: embed.ReadFileRange(%q): range [%d:%d] out of bounds for %d-byte file", path, offset, offset+length, len(data))
		}
		return constant.MakeString(string(data[offset : offset+length])), true

	case "embed.Exists":
		path := ScalarString(args[0], "embed.Exists")
		_, err := os.Stat(resolveEmbedPath(dir, path))
		return constant.MakeBool(err == nil), true

	case "embed.IsDir":
		path := ScalarString(args[0], "embed.IsDir")
		info, err := os.Stat(resolveEmbedPath(dir, path))
		if err != nil {
			Fail("forgo: embed.IsDir(%q): %s", path, err)
		}
		return constant.MakeBool(info.IsDir()), true

	case "embed.ReadDir":
		path := ScalarString(args[0], "embed.ReadDir")
		entries, err := os.ReadDir(resolveEmbedPath(dir, path))
		if err != nil {
			Fail("forgo: embed.ReadDir(%q): %s", path, err)
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		return constant.MakeString(strings.Join(names, "\n")), true

	case "embed.Getwd":
		if len(args) != 0 {
			Fail("forgo: embed.Getwd takes no arguments")
		}
		wd, err := os.Getwd()
		if err != nil {
			Fail("forgo: embed.Getwd: %s", err)
		}
		return constant.MakeString(wd), true

	case "embed.Load":
		if len(args) == 0 {
			Fail("forgo: embed.Load requires at least one pattern")
		}
		patterns := make([]string, len(args))
		for i, a := range args {
			patterns[i] = ScalarString(a, "embed.Load")
		}
		return loadEmbedFS(dir, patterns), true

	case "json.Marshal":
		if len(args) != 1 {
			Fail("forgo: json.Marshal takes exactly one argument")
		}
		s, err := marshalJSON(args[0])
		if err != nil {
			Fail("forgo: json.Marshal: %s", err)
		}
		return constant.MakeString(s), true

	case "json.Unmarshal":
		if len(args) != 1 {
			Fail("forgo: json.Unmarshal takes exactly one argument")
		}
		s := ScalarString(args[0], "json.Unmarshal")
		var raw any
		if err := stdjson.Unmarshal([]byte(s), &raw); err != nil {
			Fail("forgo: json.Unmarshal: %s", err)
		}
		return fromJSON(raw), true
	}
	return nil, false
}

// resolveEmbedPath resolves a relative path passed to one of the "embed"
// (comptime/embed) helpers against the directory of the source file
// containing the call, so `embed.ReadFile("data.txt")` reads the file next
// to the .go/.fgo file it's written in, not relative to whatever directory
// the compiler happens to be invoked from.
func resolveEmbedPath(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, path)
}

// loadEmbedFS implements embed.Load: it walks patterns (each either a
// plain file path or a directory, resolved against the call site's
// source directory the same way resolveEmbedPath does) and builds an
// ObjectVal shaped like comptime/embed's FS{files []file{name, data}}
// struct -- an unexported "files" field holding an ArrayVal of
// name/data objectVals, sorted by (dir, elem) exactly as
// comptime/embed's split/lookup/readDir expect, so the resulting
// constant materializes into a real, queryable FS value (see
// staticdata's slice-composite handling in InitConst).
//
// A directory pattern embeds every file in its subtree, skipping names
// beginning with '.' or '_', matching the `//go:embed` directive's
// rules; a plain path embeds exactly that file.
func loadEmbedFS(dir string, patterns []string) Value {
	type embedFile struct{ name, data string }
	var files []embedFile
	seen := map[string]bool{}

	var addFile func(fsPath, diskPath string)
	addFile = func(fsPath, diskPath string) {
		if seen[fsPath] {
			return
		}
		seen[fsPath] = true
		data, err := os.ReadFile(diskPath)
		if err != nil {
			Fail("forgo: embed.Load: %s", err)
		}
		files = append(files, embedFile{name: fsPath, data: string(data)})
	}
	var walkDir func(diskDir, fsDir string)
	walkDir = func(diskDir, fsDir string) {
		entries, err := os.ReadDir(diskDir)
		if err != nil {
			Fail("forgo: embed.Load: %s", err)
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			diskPath := filepath.Join(diskDir, name)
			fsPath := fsDir + "/" + name
			if e.IsDir() {
				walkDir(diskPath, fsPath)
				continue
			}
			addFile(fsPath, diskPath)
		}
	}

	for _, p := range patterns {
		diskPath := resolveEmbedPath(dir, p)
		info, err := os.Stat(diskPath)
		if err != nil {
			Fail("forgo: embed.Load(%q): %s", p, err)
		}
		if info.IsDir() {
			walkDir(diskPath, p)
		} else {
			addFile(p, diskPath)
		}
	}

	// Add a directory marker entry (name ending in '/', no data) for
	// every ancestor directory of every file, mirroring
	// comptime/embed's addDirEntries: without it, FS.lookup/readDir
	// can't find/list "template" as a directory, only its leaf files.
	dirSeen := map[string]bool{}
	var dirs []embedFile
	for _, f := range files {
		for dir, _, _ := splitEmbedName(f.name); dir != "."; {
			if !dirSeen[dir] {
				dirSeen[dir] = true
				dirs = append(dirs, embedFile{name: dir + "/"})
			}
			dir, _, _ = splitEmbedName(dir)
		}
	}
	files = append(files, dirs...)

	sort.Slice(files, func(i, j int) bool {
		idir, ielem, _ := splitEmbedName(files[i].name)
		jdir, jelem, _ := splitEmbedName(files[j].name)
		if idir != jdir {
			return idir < jdir
		}
		return ielem < jelem
	})

	elems := make([]Value, len(files))
	for i, f := range files {
		elems[i] = ObjectVal{
			Keys: []string{"name", "data"},
			Fields: map[string]Value{
				"name": constant.MakeString(f.name),
				"data": constant.MakeString(f.data),
			},
		}
	}
	return ObjectVal{
		Keys:   []string{"files"},
		Fields: map[string]Value{"files": ArrayVal{Elems: elems}},
	}
}

// splitEmbedName mirrors comptime/embed's split: it separates name into
// (dir, elem), taking dir to be "." at the root, and reports whether
// name ended in a trailing slash (a directory entry). It's used to
// reproduce the FS files list's (dir, elem) sort order here at compile
// time.
func splitEmbedName(name string) (dir, elem string, isDir bool) {
	name, isDir = strings.CutSuffix(name, "/")
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return ".", name, isDir
	}
	return name[:i], name[i+1:], isDir
}

// marshalJSON renders v (a scalar, or an ObjectVal/ArrayVal built by
// a composite literal or another comptime call) as JSON text. Object keys
// are emitted in their original literal/decode order (see ObjectVal),
// matching struct-field-order marshaling rather than alphabetically
// sorting map keys the way encoding/json does for map[string]T.
func marshalJSON(v Value) (string, error) {
	switch x := v.(type) {
	case constant.Value:
		b, err := stdjson.Marshal(Native(x))
		if err != nil {
			return "", err
		}
		return string(b), nil

	case ObjectVal:
		var buf strings.Builder
		buf.WriteByte('{')
		for i, k := range x.Keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := stdjson.Marshal(k)
			if err != nil {
				return "", err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			s, err := marshalJSON(x.Fields[k])
			if err != nil {
				return "", err
			}
			buf.WriteString(s)
		}
		buf.WriteByte('}')
		return buf.String(), nil

	case ArrayVal:
		var buf strings.Builder
		buf.WriteByte('[')
		for i, e := range x.Elems {
			if i > 0 {
				buf.WriteByte(',')
			}
			s, err := marshalJSON(e)
			if err != nil {
				return "", err
			}
			buf.WriteString(s)
		}
		buf.WriteByte(']')
		return buf.String(), nil
	}
	return "", fmt.Errorf("unsupported value of type %T", v)
}

// fromJSON converts a value decoded by encoding/json (into `any`)
// into the interpreter's own Value representation. Object keys are
// sorted, since Go map iteration order (and so encoding/json's decoded
// map[string]any) is random.
func fromJSON(x any) Value {
	switch v := x.(type) {
	case nil:
		return constant.MakeUnknown()
	case bool:
		return constant.MakeBool(v)
	case float64:
		// encoding/json decodes every JSON number into `any` as float64,
		// even whole numbers like 8080. Represent it as an Int constant
		// when it's exactly representable as one, so it matches a real
		// int-typed struct field (e.g. `.Port`) the way a plain integer
		// literal in source would -- an untyped Float constant assigned
		// to an int-typed const would otherwise panic the compiler.
		if i := int64(v); float64(i) == v {
			return constant.MakeInt64(i)
		}
		return constant.MakeFloat64(v)
	case string:
		return constant.MakeString(v)
	case []any:
		elems := make([]Value, len(v))
		for i, e := range v {
			elems[i] = fromJSON(e)
		}
		return ArrayVal{Elems: elems}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make(map[string]Value, len(v))
		for _, k := range keys {
			fields[k] = fromJSON(v[k])
		}
		return ObjectVal{Keys: keys, Fields: fields}
	}
	Fail("forgo: unsupported JSON value of type %T", x)
	panic("unreachable")
}

// Scalar asserts that v is a scalar (int/float/string/bool)
// value, failing with a message naming ctx (the argument, operand, or
// variable it came from) otherwise.
func Scalar(v Value, ctx string) constant.Value {
	cv, ok := v.(constant.Value)
	if !ok {
		Fail("forgo: %s must be a scalar (int/float/string/bool) value in comptime evaluation, not a struct/slice/map", ctx)
	}
	return cv
}

func ScalarString(v Value, ctx string) string {
	s, ok := Native(Scalar(v, ctx)).(string)
	if !ok {
		Fail("forgo: %s requires a string argument", ctx)
	}
	return s
}

func ScalarInt(v Value, ctx string) int64 {
	n, ok := Native(Scalar(v, ctx)).(int64)
	if !ok {
		Fail("forgo: %s requires an integer argument", ctx)
	}
	return n
}

// ToConstant converts v (whatever EvalExprValue produced: a scalar, or an
// ObjectVal/ArrayVal built by a composite literal or a call like
// json.Unmarshal) into a real go/constant.Value, recursively, using
// go/constant's Composite kind for ObjectVal/ArrayVal. It's used by
// types2's forgoEvalConstCall to fold a const initializer that evaluates
// to a struct/map/slice, not just a scalar. It reports false if v is
// something ToConstant doesn't know how to represent (in practice, this
// shouldn't happen: every Value EvalExprValue can produce is one of the
// cases below).
func ToConstant(v Value) (constant.Value, bool) {
	switch x := v.(type) {
	case constant.Value:
		return x, true
	case ObjectVal:
		fields := make(map[string]constant.Value, len(x.Fields))
		for k, fv := range x.Fields {
			cv, ok := ToConstant(fv)
			if !ok {
				return nil, false
			}
			fields[k] = cv
		}
		return constant.MakeCompositeStruct(x.Keys, fields), true
	case ArrayVal:
		elems := make([]constant.Value, len(x.Elems))
		for i, ev := range x.Elems {
			cv, ok := ToConstant(ev)
			if !ok {
				return nil, false
			}
			elems[i] = cv
		}
		return constant.MakeCompositeArray(elems), true
	}
	return nil, false
}

func Native(v constant.Value) any {
	switch v.Kind() {
	case constant.Bool:
		return constant.BoolVal(v)
	case constant.String:
		return constant.StringVal(v)
	case constant.Int:
		n, exact := constant.Int64Val(v)
		if !exact {
			Fail("forgo: integer constant too large for compile-time evaluation")
		}
		return n
	case constant.Float:
		f, _ := constant.Float64Val(v)
		return f
	}
	Fail("forgo: unsupported constant kind in comptime function")
	panic("unreachable")
}
