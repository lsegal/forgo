// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// forgo hot reload: linking new code against a program that is already running.
//
// A hot link is an ordinary full link of the whole program with one twist:
// every symbol whose contents are byte-for-byte what they were when the
// running program was linked is *pinned* to the address it already occupies in
// that process, instead of being laid out again. Only the symbols that
// actually changed — plus anything genuinely new — get fresh addresses, drawn
// from a region the running process reserved for exactly this purpose.
//
// The consequence is the whole point of the feature: the new code refers to
// the same package-level variables, the same runtime, the same type
// descriptors and the same itabs as the code it replaces. Nothing is copied
// and no state is migrated, because nothing moved.
//
// Two flags drive it:
//
//	-fgohotsyms file    when linking normally, record what was linked, so a
//	                    later hot link knows what is already in the process
//	-fgohotpin file     link in hot mode against such a record
//	-fgohotmanifest f   where to describe the resulting image to the agent
//
// The output of a hot link is an ordinary executable file that is never
// executed: `forgo run --watch` maps its loadable segments into the running
// process at the addresses this linker chose, then redirects the entry point
// of each changed function to its new body.

package ld

import (
	"bufio"
	"cmd/internal/objabi"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

var (
	flagFgoHotSyms     = flag.String("fgohotsyms", "", "record the linked symbol table in `file` for later forgo hot reloads")
	flagFgoHotPin      = flag.String("fgohotpin", "", "hot reload: link against the symbol record in `file`, reusing the addresses it names")
	flagFgoHotManifest = flag.String("fgohotmanifest", "", "hot reload: describe the resulting image in `file`")
)

// fgohotLinking reports whether this is a hot link — a link against a program
// that is already running.
func fgohotLinking() bool { return *flagFgoHotPin != "" }

// fgohotRecording reports whether this link should record its symbol table for
// later hot links.
func fgohotRecording() bool { return *flagFgoHotSyms != "" }

// fgohotActive reports whether either half of the feature is in play, and so
// whether the snapshot below needs taking.
func fgohotActive() bool { return fgohotLinking() || fgohotRecording() }

// fgohotPEBase returns the PE ImageBase a hot link should use, or 0 if this
// isn't one.
//
// A hot-linked image is never loaded by the OS loader — the watcher gives it
// an explicit load address via -T, chosen to sit within jump range of the
// running program's own code, and runtime/fgohot maps it there directly. PE
// addresses are RVAs relative to ImageBase, so ImageBase has to be derived
// from that chosen address (backing out the section header region that sits
// in front of it) rather than the fixed default cmd/link normally uses, or
// the RVAs it computes would describe a different address than the one the
// image actually ends up at.
func fgohotPEBase(sectHeadr int32) int64 {
	if !fgohotLinking() || *FlagTextAddr == -1 {
		return 0
	}
	return *FlagTextAddr - int64(sectHeadr)
}

// fgohotSym is what a hot link needs to know about one symbol of the running
// program.
type fgohotSym struct {
	name string
	abi  int
	kind sym.SymKind
	hash string // digest of the symbol's contents and relocation targets
	args int    // argument frame size, for text symbols
	addr uint64 // address in the running program; only set in a record
}

// fgohotState is gathered once, straight after dead code elimination, while
// symbol contents are still exactly as the compiler emitted them.
type fgohotState struct {
	// snapshot of every reachable symbol, by loader index. Names are taken
	// here because mangleTypeSym rewrites some of them later on.
	snap map[loader.Sym]*fgohotSym

	// symbols this link reused from the running program rather than laying
	// out again.
	isPinned map[loader.Sym]bool

	// functions that changed and so need their old entry point redirected.
	patches []fgohotPatch

	// packages that are new in this image and whose init must still run.
	newInitTasks []loader.Sym

	// reasons this reload cannot be applied to the running process.
	refusals []string
}

type fgohotKey struct {
	name string
	abi  int
}

type fgohotPatch struct {
	name string
	sym  loader.Sym
	old  uint64
}

var fgohot fgohotState

// fgohotNeverPin names the symbols that describe a module's own layout. Every
// module needs its own, so they are laid out fresh even though the running
// program has them too.
var fgohotNeverPin = map[string]bool{
	"runtime.text": true, "runtime.etext": true,
	"runtime.rodata": true, "runtime.erodata": true,
	"runtime.types": true, "runtime.etypes": true,
	"runtime.noptrdata": true, "runtime.enoptrdata": true,
	"runtime.data": true, "runtime.edata": true,
	"runtime.bss": true, "runtime.ebss": true,
	"runtime.noptrbss": true, "runtime.enoptrbss": true,
	"runtime.covctrs": true, "runtime.ecovctrs": true,
	"runtime.end": true, "runtime.gcdata": true, "runtime.gcbss": true,
	"runtime.pclntab": true, "runtime.epclntab": true,
	"runtime.findfunctab": true, "runtime.textsectionmap": true,
	"runtime.typelink": true, "runtime.itablink": true,
	"runtime.firstmoduledata": true, "runtime.lastmoduledatap": true,
	"runtime.runtime_inittasks": true,
	"go:func.*":                 true,
	"go:buildid":                true,
}

// fgohotNeverPinKind names symbol kinds that are never pinned regardless of
// content: symbols resolved by a platform-specific per-link mechanism rather
// than actually laid out by cmd/link. SDYNIMPORT covers Windows DLL imports —
// even a trivial Go program depends on a handful, since the runtime itself
// calls into kernel32.dll — SHOSTOBJ and SUNDEFEXT cover the equivalent for
// external/cgo linking.
var fgohotNeverPinKind = map[sym.SymKind]bool{
	sym.SDYNIMPORT: true,
	sym.SHOSTOBJ:   true,
	sym.SUNDEFEXT:  true,
}

// fgohotSnapshot records what every reachable symbol contains. It runs
// immediately after deadcode, and, when a pin record was supplied, decides
// there and then which symbols this link may reuse from the running program.
func fgohotSnapshot(ctxt *Link) {
	if ctxt.BuildMode == BuildModePIE {
		// A PIE binary's addresses are meaningless to record: the OS assigns
		// its real load address at each launch, not this link.
		Exitf("hot reload requires a non-PIE binary (-buildmode=exe)")
	}

	ldr := ctxt.loader
	fgohot.snap = make(map[loader.Sym]*fgohotSym, ldr.NSym())
	shallow := make(map[loader.Sym]string, ldr.NSym())
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if ldr.AttrReachable(s) {
			shallow[s] = fgohotShallow(ldr, s)
		}
	}
	for s := range shallow {
		fgohot.snap[s] = &fgohotSym{
			name: ldr.SymName(s),
			abi:  ldr.SymVersion(s),
			kind: ldr.SymType(s),
			hash: fgohotHash(ldr, s, shallow),
			args: fgohotArgs(ldr, s),
		}
	}
	if fgohotLinking() {
		fgohotPin(ctxt)
	}
}

// fgohotArgs returns a text symbol's argument frame size, which is part of its
// calling convention and so must not change under a running caller.
func fgohotArgs(ldr *loader.Loader, s loader.Sym) int {
	if ldr.SymType(s) != sym.STEXT {
		return -1
	}
	fi := ldr.FuncInfo(s)
	if !fi.Valid() {
		return -1
	}
	fi.Preload()
	return fi.Args()
}

// fgohotShallow digests a symbol's own bytes and the shape of its relocations,
// but nothing about what those relocations point at.
//
// Much of a Go binary is made of symbols the compiler leaves unnamed and
// identifies purely by content — stack maps, argument maps, and most read-only
// data. This digest is the only identity such a symbol has, so it is what the
// record keys them by.
func fgohotShallow(ldr *loader.Loader, s loader.Sym) string {
	h := sha256.New()
	var buf [8]byte
	put := func(v uint64) {
		binary.LittleEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	put(uint64(ldr.SymType(s)))
	put(uint64(ldr.SymSize(s)))
	put(uint64(ldr.SymAlign(s)))
	h.Write(ldr.Data(s))
	relocs := ldr.Relocs(s)
	put(uint64(relocs.Count()))
	for i := 0; i < relocs.Count(); i++ {
		r := relocs.At(i)
		put(uint64(r.Off()))
		put(uint64(r.Siz()))
		put(uint64(r.Type()))
		put(uint64(r.Add()))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// fgohotHash digests a symbol's contents together with the identity of
// everything its relocations point at — by name, or by content for the unnamed
// ones, and deliberately never by address, since addresses are exactly what
// differ between the running program and this link.
//
// Two symbols with the same digest behave identically, so the running
// program's copy of one can be reused in place of the other.
func fgohotHash(ldr *loader.Loader, s loader.Sym, shallow map[loader.Sym]string) string {
	h := sha256.New()
	h.Write([]byte(shallow[s]))
	relocs := ldr.Relocs(s)
	for i := 0; i < relocs.Count(); i++ {
		rs := relocs.At(i).Sym()
		if rs == 0 {
			h.Write([]byte{0})
			continue
		}
		if name := ldr.SymName(rs); name != "" {
			h.Write([]byte(name))
			h.Write([]byte{byte(ldr.SymVersion(rs))})
		} else {
			h.Write([]byte(shallow[rs]))
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// fgohotIdentity is what a symbol is keyed by in the record: its name, or, for
// the unnamed content-addressed ones, its contents.
func fgohotIdentity(e *fgohotSym) fgohotKey {
	if e.name != "" {
		return fgohotKey{e.name, e.abi}
	}
	return fgohotKey{"#" + e.hash, 0}
}

// fgohotPin reads the record of the running program and decides, for every
// symbol, whether this link reuses the running program's copy or lays out a
// fresh one.
func fgohotPin(ctxt *Link) {
	// Debug information describes a whole program at fixed addresses; an image
	// that deliberately leaves most of the program out cannot produce coherent
	// DWARF, and nothing consumes it here.
	*FlagW = true

	ldr := ctxt.loader
	rec, err := fgohotReadRecord(*flagFgoHotPin)
	if err != nil {
		Exitf("hot reload: %v", err)
	}
	fgohot.isPinned = make(map[loader.Sym]bool, len(rec))

	for s, cur := range fgohot.snap {
		if fgohotNeverPinKind[cur.kind] {
			// Not really "laid out" at all — resolved per-link by a
			// platform-specific mechanism (the PE import table on Windows,
			// the PLT/GOT for cgo elsewhere). A hash comparison here would be
			// meaningless, and reusing the host's copy would leave this
			// image's own generated import structures pointing at a symbol
			// that isn't actually part of it.
			continue
		}
		old := rec[fgohotIdentity(cur)]
		if old == nil {
			continue // genuinely new; lay it out fresh
		}
		if old.hash == cur.hash && !fgohotNeverPin[cur.name] {
			ldr.SetSymValue(s, int64(old.addr))
			// A pinned symbol is not part of this image, so it has no section
			// and does not belong in the image's symbol table either.
			ldr.SetAttrNotInSymbolTable(s, true)
			fgohot.isPinned[s] = true
			continue
		}

		// The symbol changed. Some kinds of change cannot be applied to a
		// process that is already running.
		switch {
		case strings.HasPrefix(cur.name, "type:"):
			// Live objects on the heap were laid out by the old descriptor and
			// code that is not being replaced still uses it.
			fgohot.refusals = append(fgohot.refusals,
				"the type "+strings.TrimPrefix(cur.name, "type:")+" changed shape")
		case strings.HasSuffix(cur.name, "..inittask"):
			// Package initialization runs once per process. If the set of init
			// functions changed, applying it would either skip the new ones or
			// re-run the old ones over state they already built.
			fgohot.refusals = append(fgohot.refusals,
				"the initialization of package "+strings.TrimSuffix(cur.name, "..inittask")+" changed")
		case cur.kind == sym.STEXT && old.args >= 0 && cur.args >= 0 && old.args != cur.args:
			// Callers that are not being replaced would still pass arguments
			// the old way.
			fgohot.refusals = append(fgohot.refusals,
				"the signature of "+cur.name+" changed")
		case cur.kind == sym.STEXT && !fgohotNeverPin[cur.name]:
			fgohot.patches = append(fgohot.patches, fgohotPatch{cur.name, s, old.addr})
		}
	}

	// Package initialization runs exactly once per package for the life of the
	// process. A package the running program already has keeps the state its
	// init built; only a package that is new here gets initialized.
	for s, cur := range fgohot.snap {
		if strings.HasSuffix(cur.name, "..inittask") && rec[fgohotIdentity(cur)] == nil {
			fgohot.newInitTasks = append(fgohot.newInitTasks, s)
		}
	}
	sort.Slice(fgohot.newInitTasks, func(i, j int) bool {
		return ldr.SymName(fgohot.newInitTasks[i]) < ldr.SymName(fgohot.newInitTasks[j])
	})

	fgohotUnpinSectionRelative(ctxt)

	if os.Getenv("FORGO_HOT_DEBUG") != "" {
		var changed []string
		for s, cur := range fgohot.snap {
			if !fgohot.isPinned[s] && cur.name != "" {
				changed = append(changed, cur.name)
			}
		}
		sort.Strings(changed)
		fmt.Fprintf(os.Stderr, "fgohot: %d symbols, %d reused from the running program\n",
			len(fgohot.snap), len(fgohot.isPinned))
		fmt.Fprintf(os.Stderr, "fgohot: in this image: %s\n", strings.Join(changed, " "))
	}

	// Everything pinned keeps the address it already has, so drop it from the
	// text list; the layout, pclntab and symbol table passes all work from
	// there and will now cover only the code this image actually contains.
	textp := ctxt.Textp[:0]
	for _, s := range ctxt.Textp {
		if !fgohot.isPinned[s] {
			textp = append(textp, s)
		}
	}
	ctxt.Textp = textp

	if len(fgohot.refusals) > 0 {
		// Report through the manifest rather than as a link failure: the
		// watcher turns it into an explanation of why a restart is needed.
		fgohotWriteManifest(ctxt)
		Exitf("hot reload not possible: %s", strings.Join(fgohot.refusals, "; "))
	}
}

// fgohotSectionRelative reports whether a relocation is resolved as an offset
// from the start of its target's section rather than as an address.
func fgohotSectionRelative(t objabi.RelocType) bool {
	switch t {
	case objabi.R_ADDROFF, objabi.R_WEAKADDROFF, objabi.R_METHODOFF,
		objabi.R_DWARFSECREF, objabi.R_PEIMAGEOFF:
		return true
	}
	return false
}

// fgohotUnpinSectionRelative gives up on reusing symbols that new code refers
// to by section offset.
//
// Such a reference is only meaningful within one module: the runtime reads it
// as a distance from this image's own rodata or type area. A symbol still
// sitting in the running program is not at any such distance, so it has to be
// copied into the image after all. In practice this is the garbage collection
// bitmaps and name tables that stack maps point at — data with no identity of
// its own, which is why copying it is harmless.
func fgohotUnpinSectionRelative(ctxt *Link) {
	ldr := ctxt.loader
	// Every symbol, not just the reachable ones: stack maps and other funcdata
	// only become reachable when the pclntab is generated, well after this.
	work := make([]loader.Sym, 0, ldr.NSym())
	for s := loader.Sym(1); s < loader.Sym(ldr.NSym()); s++ {
		if !fgohot.isPinned[s] {
			work = append(work, s)
		}
	}
	// The stack maps and argument maps of every function this image contains
	// are copied into it wholesale, so they and anything they point at by
	// offset have to be part of the image too.
	var fd []loader.Sym
	for _, s := range ctxt.Textp {
		if fgohot.isPinned[s] {
			continue
		}
		fi := ldr.FuncInfo(s)
		if !fi.Valid() {
			continue
		}
		fi.Preload()
		for _, fdSym := range funcData(ldr, s, fi, 0, fd[:0]) {
			if fdSym != 0 && fgohot.isPinned[fdSym] {
				delete(fgohot.isPinned, fdSym)
				ldr.SetSymValue(fdSym, 0)
				ldr.SetAttrNotInSymbolTable(fdSym, false)
				work = append(work, fdSym)
			}
		}
	}

	for len(work) > 0 {
		s := work[len(work)-1]
		work = work[:len(work)-1]
		relocs := ldr.Relocs(s)
		for i := 0; i < relocs.Count(); i++ {
			r := relocs.At(i)
			rs := r.Sym()
			if rs == 0 || !fgohot.isPinned[rs] || !fgohotSectionRelative(r.Type()) {
				continue
			}
			delete(fgohot.isPinned, rs)
			ldr.SetSymValue(rs, 0)
			ldr.SetAttrNotInSymbolTable(rs, false)
			work = append(work, rs)
		}
	}
}

// fgohotPinnedSym reports whether s keeps an address it already has in the
// running program, and so must be left out of this image's layout.
func fgohotPinnedSym(s loader.Sym) bool {
	return fgohot.isPinned != nil && fgohot.isPinned[s]
}

// fgohotFinish writes out whatever this link owes: a record of its symbol
// table, a manifest for the agent, or both. It runs once addresses are final.
func fgohotFinish(ctxt *Link) {
	if fgohotRecording() {
		fgohotWriteRecord(ctxt, *flagFgoHotSyms)
	}
	if fgohotLinking() {
		fgohotWriteManifest(ctxt)
	}
}

// fgohotWriteRecord saves the address, contents digest and calling convention
// of every symbol in this link, so a later hot link can tell what is already
// in the running process and where.
func fgohotWriteRecord(ctxt *Link, path string) {
	f, err := os.Create(path)
	if err != nil {
		Exitf("hot reload: %v", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	ldr := ctxt.loader
	syms := make([]loader.Sym, 0, len(fgohot.snap))
	for s := range fgohot.snap {
		syms = append(syms, s)
	}
	sort.Slice(syms, func(i, j int) bool { return syms[i] < syms[j] })

	// A patched symbol's own address in this link is a throwaway location in
	// the reload's reserved region — nothing outside this generation ever
	// refers to it. Every caller that isn't part of this image still calls
	// through the *original* entry point, which patchFuncs permanently
	// turned into a jump. That original address is the one a later hot link
	// needs to record: it is what stays reachable, and, once patched, it no
	// longer holds the symbol's real code — so recording anything else here
	// would let a later edit that happens to match some earlier generation's
	// bytes get pinned back to an address that isn't that code anymore.
	stableAddr := make(map[loader.Sym]uint64, len(fgohot.patches))
	for _, p := range fgohot.patches {
		stableAddr[p.sym] = p.old
	}

	fmt.Fprintln(w, "forgohot 1")
	for _, s := range syms {
		e := fgohot.snap[s]
		v := uint64(ldr.SymValue(s))
		if addr, ok := stableAddr[s]; ok {
			v = addr
		}
		if v == 0 || ldr.AttrSpecial(s) {
			// Either never placed, or placed by hand — stack maps and the
			// like, whose "value" is an offset inside a generated container
			// rather than an address. Nothing a later link could reuse.
			continue
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%d\t%x\n", e.name, e.abi, e.kind, e.hash, e.args, v)
	}
}

// fgohotReadRecord parses a record written by fgohotWriteRecord.
func fgohotReadRecord(path string) (map[fgohotKey]*fgohotSym, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "forgohot ") {
		return nil, fmt.Errorf("%s is not a forgo hot reload symbol record", path)
	}
	out := make(map[fgohotKey]*fgohotSym, len(lines))
	for _, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			continue
		}
		abi, _ := strconv.Atoi(f[1])
		kind, _ := strconv.Atoi(f[2])
		args, _ := strconv.Atoi(f[4])
		addr, _ := strconv.ParseUint(f[5], 16, 64)
		e := &fgohotSym{
			name: f[0], abi: abi, kind: sym.SymKind(kind),
			hash: f[3], args: args, addr: addr,
		}
		out[fgohotIdentity(e)] = e
	}
	return out, nil
}

// fgohotWriteManifest tells the agent what to do with this image: which of the
// output file's segments to map where, where the moduledata is, which inits to
// run, and which entry points to redirect.
func fgohotWriteManifest(ctxt *Link) {
	if *flagFgoHotManifest == "" {
		return
	}
	f, err := os.Create(*flagFgoHotManifest)
	if err != nil {
		Exitf("hot reload: %v", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	if len(fgohot.refusals) > 0 {
		for _, r := range fgohot.refusals {
			fmt.Fprintf(w, "refuse %s\n", r)
		}
		return
	}

	ldr := ctxt.loader
	fmt.Fprintf(w, "moduledata %x\n", uint64(ldr.SymValue(ctxt.Moduledata)))
	for _, p := range fgohot.patches {
		if v := ldr.SymValue(p.sym); v != 0 {
			fmt.Fprintf(w, "patch %x %x # %s\n", p.old, uint64(v), p.name)
		}
	}
	for _, s := range fgohot.newInitTasks {
		fmt.Fprintf(w, "newpkg %s\n", strings.TrimSuffix(ldr.SymName(s), "..inittask"))
	}
}
