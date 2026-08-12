// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package staticdata

import (
	"encoding/base64"
	"fmt"
	"go/constant"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/types"
	"cmd/internal/hash"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
)

// InitAddrOffset writes the static name symbol lsym to n, it does not modify n.
// It's the caller responsibility to make sure lsym is from ONAME/PEXTERN node.
func InitAddrOffset(n *ir.Name, noff int64, lsym *obj.LSym, off int64) {
	if n.Op() != ir.ONAME {
		base.Fatalf("InitAddr n op %v", n.Op())
	}
	if n.Sym() == nil {
		base.Fatalf("InitAddr nil n sym")
	}
	s := n.Linksym()
	s.WriteAddr(base.Ctxt, noff, types.PtrSize, lsym, off)
}

// InitAddr is InitAddrOffset, with offset fixed to 0.
func InitAddr(n *ir.Name, noff int64, lsym *obj.LSym) {
	InitAddrOffset(n, noff, lsym, 0)
}

// InitSlice writes a static slice symbol {lsym, lencap, lencap} to n+noff, it does not modify n.
// It's the caller responsibility to make sure lsym is from ONAME node.
func InitSlice(n *ir.Name, noff int64, lsym *obj.LSym, lencap int64) {
	s := n.Linksym()
	s.WriteAddr(base.Ctxt, noff, types.PtrSize, lsym, 0)
	s.WriteInt(base.Ctxt, noff+types.SliceLenOffset, types.PtrSize, lencap)
	s.WriteInt(base.Ctxt, noff+types.SliceCapOffset, types.PtrSize, lencap)
}

func InitSliceBytes(nam *ir.Name, off int64, s string) {
	if nam.Op() != ir.ONAME {
		base.Fatalf("InitSliceBytes %v", nam)
	}
	InitSlice(nam, off, slicedata(nam.Pos(), s), int64(len(s)))
}

const (
	stringSymPrefix  = "go:string."
	stringSymPattern = ".gostring.%d.%s"
)

// shortHashString converts the hash to a string for use with stringSymPattern.
// We cut it to 16 bytes and then base64-encode to make it even smaller.
func shortHashString(hash []byte) string {
	return base64.StdEncoding.EncodeToString(hash[:16])
}

// StringSym returns a symbol containing the string s.
// The symbol contains the string data, not a string header.
func StringSym(pos src.XPos, s string) (data *obj.LSym) {
	var symname string
	if len(s) > 100 {
		// Huge strings are hashed to avoid long names in object files.
		// Indulge in some paranoia by writing the length of s, too,
		// as protection against length extension attacks.
		// Same pattern is known to fileStringSym below.
		h := hash.New32()
		io.WriteString(h, s)
		symname = fmt.Sprintf(stringSymPattern, len(s), shortHashString(h.Sum(nil)))
	} else {
		// Small strings get named directly by their contents.
		symname = strconv.Quote(s)
	}

	symdata := base.Ctxt.Lookup(stringSymPrefix + symname)
	if !symdata.OnList() {
		off := dstringdata(symdata, 0, s, pos, "string")
		objw.Global(symdata, int32(off), obj.DUPOK|obj.RODATA|obj.LOCAL)
		symdata.Set(obj.AttrContentAddressable, true)
	}

	return symdata
}

// StringSymNoCommon is like StringSym, but produces a symbol that is not content-
// addressable. This symbol is not supposed to appear in the final binary, it is
// only used to pass string arguments to the linker like R_USENAMEDMETHOD does.
func StringSymNoCommon(s string) (data *obj.LSym) {
	var nameSym obj.LSym
	nameSym.WriteString(base.Ctxt, 0, len(s), s)
	objw.Global(&nameSym, int32(len(s)), obj.RODATA)
	return &nameSym
}

// maxFileSize is the maximum file size permitted by the linker
// (see issue #9862).
const maxFileSize = int64(2e9)

// fileStringSym returns a symbol for the contents and the size of file.
// If readonly is true, the symbol shares storage with any literal string
// or other file with the same content and is placed in a read-only section.
// If readonly is false, the symbol is a read-write copy separate from any other,
// for use as the backing store of a []byte.
// The content hash of file is copied into hashBytes. (If hash is nil, nothing is copied.)
// The returned symbol contains the data itself, not a string header.
func fileStringSym(pos src.XPos, file string, readonly bool, hashBytes []byte) (*obj.LSym, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("not a regular file")
	}
	size := info.Size()
	if size <= 1*1024 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, 0, err
		}
		if int64(len(data)) != size {
			return nil, 0, fmt.Errorf("file changed between reads")
		}
		var sym *obj.LSym
		if readonly {
			sym = StringSym(pos, string(data))
		} else {
			sym = slicedata(pos, string(data))
		}
		if len(hashBytes) > 0 {
			sum := hash.Sum32(data)
			copy(hashBytes, sum[:])
		}
		return sym, size, nil
	}
	if size > maxFileSize {
		// ggloblsym takes an int32,
		// and probably the rest of the toolchain
		// can't handle such big symbols either.
		// See golang.org/issue/9862.
		return nil, 0, fmt.Errorf("file too large (%d bytes > %d bytes)", size, maxFileSize)
	}

	// File is too big to read and keep in memory.
	// Compute hashBytes if needed for read-only content hashing or if the caller wants it.
	var sum []byte
	if readonly || len(hashBytes) > 0 {
		h := hash.New32()
		n, err := io.Copy(h, f)
		if err != nil {
			return nil, 0, err
		}
		if n != size {
			return nil, 0, fmt.Errorf("file changed between reads")
		}
		sum = h.Sum(nil)
		copy(hashBytes, sum)
	}

	var symdata *obj.LSym
	if readonly {
		symname := fmt.Sprintf(stringSymPattern, size, shortHashString(sum))
		symdata = base.Ctxt.Lookup(stringSymPrefix + symname)
		if !symdata.OnList() {
			info := symdata.NewFileInfo()
			info.Name = file
			info.Size = size
			objw.Global(symdata, int32(size), obj.DUPOK|obj.RODATA|obj.LOCAL)
			// Note: AttrContentAddressable cannot be set here,
			// because the content-addressable-handling code
			// does not know about file symbols.
		}
	} else {
		// Emit a zero-length data symbol
		// and then fix up length and content to use file.
		symdata = slicedata(pos, "")
		symdata.Size = size
		symdata.Type = objabi.SNOPTRDATA
		info := symdata.NewFileInfo()
		info.Name = file
		info.Size = size
	}

	return symdata, size, nil
}

var slicedataGen int

func slicedata(pos src.XPos, s string) *obj.LSym {
	slicedataGen++
	symname := fmt.Sprintf(".gobytes.%d", slicedataGen)
	lsym := types.LocalPkg.Lookup(symname).LinksymABI(obj.ABI0)
	off := dstringdata(lsym, 0, s, pos, "slice")
	objw.Global(lsym, int32(off), obj.NOPTR|obj.LOCAL)

	return lsym
}

func dstringdata(s *obj.LSym, off int, t string, pos src.XPos, what string) int {
	// Objects that are too large will cause the data section to overflow right away,
	// causing a cryptic error message by the linker. Check for oversize objects here
	// and provide a useful error message instead.
	if int64(len(t)) > 2e9 {
		base.ErrorfAt(pos, 0, "%v with length %v is too big", what, len(t))
		return 0
	}

	s.WriteString(base.Ctxt, int64(off), len(t), t)
	return off + len(t)
}

var (
	funcsymsmu sync.Mutex // protects funcsyms and associated package lookups (see func funcsym)
	funcsyms   []*ir.Name // functions that need function value symbols
)

// FuncLinksym returns n·f, the function value symbol for n.
func FuncLinksym(n *ir.Name) *obj.LSym {
	if n.Op() != ir.ONAME || n.Class != ir.PFUNC {
		base.Fatalf("expected func name: %v", n)
	}
	s := n.Sym()

	// funcsymsmu here serves to protect not just mutations of funcsyms (below),
	// but also the package lookup of the func sym name,
	// since this function gets called concurrently from the backend.
	// There are no other concurrent package lookups in the backend,
	// except for the types package, which is protected separately.
	// Reusing funcsymsmu to also cover this package lookup
	// avoids a general, broader, expensive package lookup mutex.
	funcsymsmu.Lock()
	sf, existed := s.Pkg.LookupOK(ir.FuncSymName(s))
	if !existed {
		funcsyms = append(funcsyms, n)
	}
	funcsymsmu.Unlock()

	return sf.Linksym()
}

func GlobalLinksym(n *ir.Name) *obj.LSym {
	if n.Op() != ir.ONAME || n.Class != ir.PEXTERN {
		base.Fatalf("expected global variable: %v", n)
	}
	return n.Linksym()
}

func WriteFuncSyms() {
	slices.SortFunc(funcsyms, func(a, b *ir.Name) int {
		return strings.Compare(a.Linksym().Name, b.Linksym().Name)
	})
	for _, nam := range funcsyms {
		s := nam.Sym()
		sf := s.Pkg.Lookup(ir.FuncSymName(s)).Linksym()

		// While compiling package runtime, we might try to create
		// funcsyms for functions from both types.LocalPkg and
		// ir.Pkgs.Runtime.
		if base.Flag.CompilingRuntime && sf.OnList() {
			continue
		}

		// Function values must always reference ABIInternal
		// entry points.
		target := s.Linksym()
		if target.ABI() != obj.ABIInternal {
			base.Fatalf("expected ABIInternal: %v has %v", target, target.ABI())
		}
		objw.SymPtr(sf, 0, target, 0)
		objw.Global(sf, int32(types.PtrSize), obj.DUPOK|obj.RODATA)
	}
}

// InitConst writes the static literal c to n.
// Neither n nor c is modified.
func InitConst(n *ir.Name, noff int64, c ir.Node, wid int) {
	if n.Op() != ir.ONAME {
		base.Fatalf("InitConst n op %v", n.Op())
	}
	if n.Sym() == nil {
		base.Fatalf("InitConst nil n sym")
	}
	initConstAt(n.Linksym(), n.Pos(), noff, c, wid)
}

// initConstAt is InitConst generalized to write into an arbitrary
// symbol, so composite-constant recursion (initConstComposite) can
// target a freshly synthesized backing-array symbol for a slice field,
// not just the top-level ir.Name being initialized.
func initConstAt(s *obj.LSym, pos src.XPos, noff int64, c ir.Node, wid int) {
	if c.Op() == ir.ONIL {
		return
	}
	if c.Op() != ir.OLITERAL {
		base.Fatalf("InitConst c op %v", c.Op())
	}
	switch u := c.Val(); u.Kind() {
	case constant.Bool:
		i := int64(obj.Bool2int(constant.BoolVal(u)))
		s.WriteInt(base.Ctxt, noff, wid, i)

	case constant.Int:
		s.WriteInt(base.Ctxt, noff, wid, ir.IntVal(c.Type(), u))

	case constant.Float:
		f, _ := constant.Float64Val(u)
		switch c.Type().Kind() {
		case types.TFLOAT32:
			s.WriteFloat32(base.Ctxt, noff, float32(f))
		case types.TFLOAT64:
			s.WriteFloat64(base.Ctxt, noff, f)
		}

	case constant.Complex:
		re, _ := constant.Float64Val(constant.Real(u))
		im, _ := constant.Float64Val(constant.Imag(u))
		switch c.Type().Kind() {
		case types.TCOMPLEX64:
			s.WriteFloat32(base.Ctxt, noff, float32(re))
			s.WriteFloat32(base.Ctxt, noff+4, float32(im))
		case types.TCOMPLEX128:
			s.WriteFloat64(base.Ctxt, noff, re)
			s.WriteFloat64(base.Ctxt, noff+8, im)
		}

	case constant.String:
		i := constant.StringVal(u)
		symdata := StringSym(pos, i)
		s.WriteAddr(base.Ctxt, noff, types.PtrSize, symdata, 0)
		s.WriteInt(base.Ctxt, noff+int64(types.PtrSize), types.PtrSize, int64(len(i)))

	case constant.Composite:
		initConstComposite(s, pos, noff, c)

	default:
		base.Fatalf("InitConst unhandled OLITERAL %v", c)
	}
}

// initConstComposite writes a forgo composite constant (a struct,
// [N]T array, or slice value folded by the comptime interpreter, see
// go/constant's Composite kind) into static memory, recursing
// field-by-field/element-by-element. A slice field gets its elements
// written into a freshly synthesized backing-array symbol (see
// compositeBackingSym), then a {ptr, len, cap} slice header pointing at
// it is written at the field's offset -- the same shape InitSlice
// writes for a []byte string conversion. Map-typed composite constants
// still aren't materializable this way (Go doesn't statically lay out
// map data at all); using one directly as a value (rather than only
// folding a field/index access off of it, which types2 already resolves
// to a plain scalar constant before this ever runs) isn't supported.
func initConstComposite(s *obj.LSym, pos src.XPos, noff int64, c ir.Node) {
	val := c.Val()
	typ := c.Type()
	switch {
	case typ.IsStruct():
		if constant.CompositeIsArray(val) {
			base.Fatalf("InitConst: composite constant shape mismatch for struct %v", typ)
		}
		for i := 0; i < typ.NumFields(); i++ {
			f := typ.Field(i)
			fv, ok := constant.CompositeField(val, f.Sym.Name)
			if !ok {
				continue // zero value: static memory is already zeroed
			}
			writeConstField(s, pos, noff+f.Offset, fv, f.Type)
		}

	case typ.IsArray():
		if !constant.CompositeIsArray(val) {
			base.Fatalf("InitConst: composite constant shape mismatch for array %v", typ)
		}
		elems := constant.CompositeElems(val)
		elemType := typ.Elem()
		width := elemType.Size()
		for i, ev := range elems {
			writeConstField(s, pos, noff+int64(i)*width, ev, elemType)
		}

	case typ.IsSlice():
		if !constant.CompositeIsArray(val) {
			base.Fatalf("InitConst: composite constant shape mismatch for slice %v", typ)
		}
		elems := constant.CompositeElems(val)
		if len(elems) == 0 {
			return // nil slice: static memory is already zeroed
		}
		elemType := typ.Elem()
		width := elemType.Size()
		backing := compositeBackingSym(pos, elemType, len(elems))
		for i, ev := range elems {
			writeConstField(backing, pos, int64(i)*width, ev, elemType)
		}
		InitSliceAt(s, noff, backing, int64(len(elems)))

	default:
		base.Fatalf("forgo: a compile-time struct/map/slice/array constant of type %v can't be used directly as a value here -- only field/index access on it (e.g. x.Field, x[i]) is supported", typ)
	}
}

// compositeBackingSym allocates and returns a fresh read-only symbol
// sized to hold n elements of elemType, for a slice-typed composite
// constant's backing array (see initConstComposite). It doesn't mark
// the symbol NOPTR: unlike slicedata's raw bytes, elemType may itself
// contain pointers (e.g. a struct with a string field), which need
// relocations the GC can follow.
var compositeBackingGen int

func compositeBackingSym(pos src.XPos, elemType *types.Type, n int) *obj.LSym {
	compositeBackingGen++
	symname := fmt.Sprintf(".gocompositedata.%d", compositeBackingGen)
	lsym := types.LocalPkg.Lookup(symname).LinksymABI(obj.ABI0)
	size := elemType.Size() * int64(n)
	objw.Global(lsym, int32(size), obj.RODATA|obj.LOCAL)
	return lsym
}

// InitSliceAt writes a static slice header {lsym, lencap, lencap} at
// noff into s directly, the way InitSlice does for an ir.Name's own
// symbol. It's used when the slice header's destination is itself a
// synthesized symbol (a composite constant nested inside another
// composite), not a real declared name.
func InitSliceAt(s *obj.LSym, noff int64, lsym *obj.LSym, lencap int64) {
	s.WriteAddr(base.Ctxt, noff, types.PtrSize, lsym, 0)
	s.WriteInt(base.Ctxt, noff+types.SliceLenOffset, types.PtrSize, lencap)
	s.WriteInt(base.Ctxt, noff+types.SliceCapOffset, types.PtrSize, lencap)
}

// compositeConstSyms memoizes CompositeConstExpr's synthesized globals by
// the identity of the OLITERAL node they came from -- a given `const`
// identifier's references all share the same underlying *ir.Name/Node
// (see typecheck), so this dedupes repeated uses of the same forgo
// composite constant (e.g. `content` used three times in one function)
// down to a single static copy of its data.
var (
	compositeConstMu   sync.Mutex
	compositeConstSyms = map[ir.Node]*obj.LSym{}
	compositeConstGen  int
)

// CompositeConstExpr returns an expression reading the value of n (an
// OLITERAL node holding a forgo composite constant -- a struct/slice/
// array folded by the comptime interpreter, see go/constant's Composite
// kind) from a synthesized static read-only global, materializing that
// global via InitConst on first use. It's how a composite constant used
// in ordinary expression position (a function argument, an assignment
// RHS, ...) becomes something ssagen can actually load -- ssagen has no
// notion of an OLITERAL struct/slice value, only OLINKSYMOFFSET reads
// out of static memory (see walk's OLITERAL case in expr.go).
func CompositeConstExpr(n ir.Node) ir.Node {
	compositeConstMu.Lock()
	defer compositeConstMu.Unlock()

	if lsym, ok := compositeConstSyms[n]; ok {
		return ir.NewLinksymExpr(n.Pos(), lsym, n.Type())
	}

	typ := n.Type()
	types.CalcSize(typ)
	compositeConstGen++
	symname := fmt.Sprintf(".gocomposite.%d", compositeConstGen)
	lsym := types.LocalPkg.Lookup(symname).LinksymABI(obj.ABI0)
	objw.Global(lsym, int32(typ.Size()), obj.RODATA|obj.LOCAL)
	initConstAt(lsym, n.Pos(), 0, n, int(typ.Size()))

	compositeConstSyms[n] = lsym
	return ir.NewLinksymExpr(n.Pos(), lsym, typ)
}

// writeConstField writes a single field/element value (itself possibly a
// nested composite) at the given offset, dispatching back through
// InitConst-shaped logic for scalars.
func writeConstField(s *obj.LSym, pos src.XPos, off int64, v constant.Value, typ *types.Type) {
	if v.Kind() == constant.Composite {
		initConstComposite(s, pos, off, ir.NewBasicLit(pos, typ, v))
		return
	}
	lit := ir.NewBasicLit(pos, typ, v)
	initConstAt(s, pos, off, lit, int(typ.Size()))
}
