// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// `forgo run --watch` — recompile on save and inject the new code into the
// program that is already running, without restarting it.
//
// This is the driver half of forgo's hot reload. It builds the program once,
// normally, asking the linker to record what it linked and where. It then runs
// it with a control directory in the environment, which the agent inside the
// program (runtime/fgohot) uses to announce the region of address space it has
// reserved for new code.
//
// From then on, every time a source file changes, the program is linked again
// against that record: everything that did not change keeps the address it
// already has in the running process — including every package-level variable
// — and only what changed is laid out afresh, inside the reserved region. The
// result is handed to the agent, which maps it, registers it with the runtime,
// and points the old functions at their new bodies.
//
// Goroutines keep running, the heap is untouched, and open files and
// connections stay open. A function already executing finishes its current
// call in the old code; the next call runs the new code. That is the same
// bargain Dart's hot reload makes.
//
// Some changes cannot be applied to a running process — changing a struct's
// layout, a function's signature, or a package's initialization. The linker
// detects those and says so, and the program keeps running the old code until
// it is restarted.

package run

import (
	"context"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cmd/go/internal/base"
)

// watchFlag is the -watch flag of `forgo run`.
var watchFlag bool

// pollInterval is how often the source tree is checked for changes.
const pollInterval = 300 * time.Millisecond

// generationGap is the alignment between successive images in the reserved
// region.
const generationGap = 0x10000

// headroom is space left below an image's code for the object file headers the
// linker writes just before it.
const headroom = 0x10000

// forgoWatch runs pkg and reloads it in place whenever its sources change.
func forgoWatch(ctx context.Context, args []string) {
	switch {
	case runtime.GOARCH != "amd64" && !(runtime.GOARCH == "arm64" && runtime.GOOS == "darwin"):
		base.Fatalf("forgo run --watch: hot reload currently requires amd64, or arm64 on darwin, not %s/%s", runtime.GOOS, runtime.GOARCH)
	case runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "darwin":
		base.Fatalf("forgo run --watch: hot reload currently requires linux, windows or darwin, not %s", runtime.GOOS)
	}
	pkgArgs, progArgs := splitRunArgs(args)
	if len(pkgArgs) == 0 {
		base.Fatalf("forgo run --watch: no package to run")
	}

	self, err := os.Executable()
	if err != nil {
		base.Fatalf("forgo run --watch: %v", err)
	}
	ctl, err := os.MkdirTemp("", "forgo-hot-")
	if err != nil {
		base.Fatalf("forgo run --watch: %v", err)
	}
	defer os.RemoveAll(ctl)

	w := &watcher{
		ctx:      ctx,
		self:     self,
		ctl:      ctl,
		pkgArgs:  pkgArgs,
		progArgs: progArgs,
		record:   filepath.Join(ctl, "host.syms"),
		binary:   filepath.Join(ctl, "prog"+exeSuffix()),
	}
	w.roots = watchRoots(pkgArgs)
	if err := w.writeAgentOverlay(); err != nil {
		base.Fatalf("forgo run --watch: %v", err)
	}
	w.run()
}

// writeAgentOverlay arranges for the program to contain the hot reload agent
// without touching the user's source tree.
//
// The agent has to be a real import of the main package: a package that
// nothing imports is dropped by the linker, initializers and all. So the build
// is given an overlay that adds one file to the package directory, importing
// runtime/fgohot for its side effect. The file exists only in the overlay.
func (w *watcher) writeAgentOverlay() error {
	dir, err := w.packageDir()
	if err != nil {
		return err
	}
	src := filepath.Join(w.ctl, "agent_import.go")
	const content = "package main\n\n" +
		"// Added by `forgo run --watch`; not part of your source tree.\n" +
		"import _ \"runtime/fgohot\"\n"
	if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
		return err
	}
	added := filepath.Join(dir, "zz_forgo_hotreload.go")
	overlay := fmt.Sprintf("{\"Replace\": {%s: %s}}\n", quoteJSON(added), quoteJSON(src))
	w.overlay = filepath.Join(w.ctl, "overlay.json")
	return os.WriteFile(w.overlay, []byte(overlay), 0o600)
}

// packageDir asks this same forgo binary where the package being run lives.
func (w *watcher) packageDir() (string, error) {
	if isSourceFile(w.pkgArgs[0]) {
		return filepath.Abs(filepath.Dir(w.pkgArgs[0]))
	}
	out, err := exec.Command(w.self, append([]string{"list", "-f", "{{.Dir}}"}, w.pkgArgs...)...).Output()
	if err != nil {
		return "", fmt.Errorf("cannot locate %s: %v", strings.Join(w.pkgArgs, " "), err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("cannot locate %s", strings.Join(w.pkgArgs, " "))
	}
	return dir, nil
}

// quoteJSON renders a path as a JSON string. Windows paths contain
// backslashes, so this cannot be a bare %q.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

type watcher struct {
	ctx      context.Context
	self     string // path to this forgo binary, used to run builds
	ctl      string // control directory shared with the agent
	pkgArgs  []string
	progArgs []string
	record   string // symbol record of the running program
	binary   string
	overlay  string   // overlay that adds the agent import to the main package
	roots    []string // directories whose .go files are watched

	proc     *exec.Cmd
	nextBase uint64 // where the next image goes in the reserved region
	regionTo uint64 // end of the reserved region
	gen      int
}

func (w *watcher) run() {
	fmt.Fprintf(os.Stderr, "forgo: building\n")
	if err := w.build(nil, w.binary, "-fgohotsyms="+w.record); err != nil {
		base.Fatalf("forgo run --watch: %v", err)
	}
	if err := w.start(); err != nil {
		base.Fatalf("forgo run --watch: %v", err)
	}
	if err := w.handshake(); err != nil {
		fmt.Fprintf(os.Stderr, "forgo: %v; the program is running but will not reload\n", err)
	}

	done := make(chan error, 1)
	go func() { done <- w.proc.Wait() }()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	state := snapshotTree(w.roots)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				fmt.Fprintf(os.Stderr, "forgo: program exited: %v\n", err)
				base.SetExitStatus(1)
			}
			return
		case <-interrupt:
			w.proc.Process.Signal(os.Interrupt)
			<-done
			return
		case <-w.ctx.Done():
			w.proc.Process.Kill()
			return
		case <-tick.C:
			next := snapshotTree(w.roots)
			if sameTree(state, next) {
				continue
			}
			state = next
			w.reload()
		}
	}
}

// build shells out to this same forgo binary. Doing it as a subprocess keeps
// each build's state — flags, caches, error reporting — exactly what a plain
// `forgo build` would produce.
func (w *watcher) build(extraGcflags []string, out string, ldflags ...string) error {
	argv := []string{"build"}
	// Inlining would copy a changed function's body into callers that are not
	// being replaced, so a reload would only take effect in some of them.
	argv = append(argv, "-gcflags=all=-l")
	// The addresses recorded for pinning are the ones this link assigns; they
	// only describe the running process if the OS loads it at exactly those
	// addresses. Go defaults to PIE on windows/amd64, which the OS instead
	// relocates to a random base at every launch — silently invalidating
	// every recorded address. -buildmode=exe forces the fixed-base default
	// every other platform already gets, so a hot link's -T actually lands
	// where the running program's pinned symbols are.
	argv = append(argv, "-buildmode=exe")
	if w.overlay != "" {
		argv = append(argv, "-overlay="+w.overlay)
	}
	argv = append(argv, extraGcflags...)
	// On darwin/arm64 (and windows/arm64) the OS forces PIE regardless of
	// -buildmode=exe — see cmd/link/internal/ld/config.go. -fgohotpie tells
	// the linker this caller knows and will correct recorded addresses for
	// the OS's ASLR slide (see (*watcher).aslrSlide) before reusing them, so
	// it should record/pin anyway instead of refusing a PIE binary outright.
	ldflags = append(ldflags, "-fgohotpie")
	// External linking (cgo's default) hands relocations to the system
	// linker, which needs every referenced symbol to have a section — but a
	// pinned symbol deliberately has none; it isn't laid out by this link
	// at all, just reused at the address it already has. That combination
	// fails outright ("missing section for ..."). Internal linking doesn't
	// have this requirement, so watched builds force it. This does mean a
	// cgo package that specifically needs external linking (net or os/user
	// on some platforms, C++ code, static libraries that don't support
	// internal linking) can't be hot-reloaded — a real limitation, not a
	// bug being papered over.
	ldflags = append(ldflags, "-linkmode=internal")
	argv = append(argv, "-ldflags="+strings.Join(ldflags, " "), "-o", out)
	argv = append(argv, w.pkgArgs...)

	cmd := exec.Command(w.self, argv...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (w *watcher) start() error {
	cmd := exec.Command(w.binary, w.progArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "FORGO_HOT_DIR="+w.ctl)
	if err := cmd.Start(); err != nil {
		return err
	}
	w.proc = cmd
	return nil
}

// handshake waits for the agent to report the address space it reserved.
func (w *watcher) handshake() error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if msg, err := os.ReadFile(filepath.Join(w.ctl, "agent.err")); err == nil {
			return fmt.Errorf("%s", strings.TrimSpace(string(msg)))
		}
		data, err := os.ReadFile(filepath.Join(w.ctl, "agent"))
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var base, size, landmark uint64
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			switch f[0] {
			case "base":
				base, _ = strconv.ParseUint(strings.TrimPrefix(f[1], "0x"), 16, 64)
			case "size":
				size, _ = strconv.ParseUint(strings.TrimPrefix(f[1], "0x"), 16, 64)
			case "landmark":
				landmark, _ = strconv.ParseUint(strings.TrimPrefix(f[1], "0x"), 16, 64)
			}
		}
		if base == 0 || size == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		w.nextBase = base
		w.regionTo = base + size
		if err := w.correctASLR(landmark); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("the program did not start a hot reload agent")
}

// correctASLR rebases the host build's recorded symbol addresses onto the
// actual addresses of the process now running them.
//
// Every platform forgo hot reload supports keeps one property even when the
// OS uses ASLR: the whole image is displaced by, at most, a single constant
// offset from where the linker put it — never per-symbol. Most platforms
// keep that offset at zero by building with -buildmode=exe; where the OS
// forces PIE regardless (darwin/arm64, windows/arm64 — see cmd/link's
// config.go), this recovers the offset from one landmark both sides agree
// on: runtime/fgohot.serve, an ordinary function whose linked address the
// record names and whose actual address the agent just reported as
// "landmark" in the handshake (computed with reflect, from inside the very
// process the record needs correcting for) — and applies it to every
// recorded address, so the rest of the pin mechanism never has to know
// ASLR happened at all.
func (w *watcher) correctASLR(actualLandmark uint64) error {
	if actualLandmark == 0 {
		return nil // this agent build predates the "landmark" handshake line
	}
	recorded, err := recordedAddr(w.record, "runtime/fgohot.serve")
	if err != nil {
		return err
	}
	slide := int64(actualLandmark) - int64(recorded)
	if slide == 0 {
		return nil
	}
	return rebaseRecord(w.record, slide)
}

// recordedAddr returns the address a forgo hot reload symbol record (written
// by the linker's -fgohotsyms, and parsed the same way by fgohotReadRecord in
// cmd/link/internal/ld/forgo_hot.go) gives name.
func recordedAddr(path, name string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 6 && f[0] == name {
			return strconv.ParseUint(f[5], 16, 64)
		}
	}
	return 0, fmt.Errorf("%s: no %s symbol recorded", path, name)
}

// rebaseRecord adds slide to every address in a forgo hot reload symbol
// record, rewriting the file in place. See (*watcher).correctASLR.
func rebaseRecord(path string, slide int64) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			continue
		}
		addr, err := strconv.ParseUint(f[5], 16, 64)
		if err != nil {
			continue
		}
		f[5] = strconv.FormatUint(uint64(int64(addr)+slide), 16)
		lines[i] = strings.Join(f, "\t")
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

// reload rebuilds, links against the running program, and hands the result to
// the agent.
func (w *watcher) reload() {
	if w.nextBase == 0 {
		return
	}
	w.gen++
	gen := strconv.Itoa(w.gen)
	image := filepath.Join(w.ctl, "image-"+gen)
	manifest := filepath.Join(w.ctl, "link-"+gen)
	// A patched function's stable entry point is a permanent trampoline
	// after this reload, not its own code; only this fresh record — not the
	// one from the very first build — knows that. Comparing the next reload
	// against anything older would let an edit that happens to match an
	// earlier generation's bytes get silently pinned back to a trampoline
	// that no longer runs that code.
	newRecord := filepath.Join(w.ctl, "syms-"+gen)

	start := time.Now()
	err := w.build(nil, image,
		"-fgohotpin="+w.record,
		"-fgohotsyms="+newRecord,
		"-fgohotmanifest="+manifest,
		"-T", fmt.Sprintf("%#x", w.nextBase+headroom))
	if err != nil {
		if reasons := refusals(manifest); len(reasons) > 0 {
			fmt.Fprintf(os.Stderr, "forgo: cannot reload without restarting — %s\n",
				strings.Join(reasons, "; "))
			return
		}
		fmt.Fprintf(os.Stderr, "forgo: reload failed: %v\n", err)
		return
	}

	segs, lo, hi, err := loadSegments(image)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forgo: reload failed: %v\n", err)
		return
	}
	if lo < w.nextBase || hi > w.regionTo {
		fmt.Fprintf(os.Stderr, "forgo: no room left for reloaded code; restart to continue\n")
		return
	}

	linked, err := os.ReadFile(manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forgo: reload failed: %v\n", err)
		return
	}
	var req strings.Builder
	fmt.Fprintf(&req, "base %#x\nsize %#x\n", lo, hi-lo)
	for _, s := range segs {
		fmt.Fprintf(&req, "segment %#x %#x %#x %#x %s\n", s.addr, s.fileoff, s.filesz, s.memsz, s.prot)
	}
	req.WriteString(string(linked))

	npatch := 0
	for _, line := range strings.Split(string(linked), "\n") {
		if strings.HasPrefix(line, "patch ") {
			npatch++
		}
	}
	if err := w.deliver(gen, image, req.String()); err != nil {
		fmt.Fprintf(os.Stderr, "forgo: reload failed: %v\n", err)
		return
	}
	// Only now, once the agent has confirmed the image is actually live, does
	// this generation's record become the truth to compare the next edit
	// against.
	w.record = newRecord
	w.nextBase = (hi + generationGap - 1) &^ (generationGap - 1)
	fmt.Fprintf(os.Stderr, "forgo: reloaded %s in %v\n", plural(npatch, "function"), time.Since(start).Round(time.Millisecond))
}

// deliver hands one image to the agent and waits for its verdict.
func (w *watcher) deliver(gen, image, manifest string) error {
	data, err := os.ReadFile(image)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.ctl, "img-"+gen), data, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.ctl, "req-"+gen), []byte(manifest), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.ctl, "request"), []byte(gen), 0o600); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := os.ReadFile(filepath.Join(w.ctl, "resp-"+gen))
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		text := strings.TrimSpace(string(resp))
		if text == "ok" {
			return nil
		}
		return fmt.Errorf("%s", strings.TrimPrefix(text, "err "))
	}
	return fmt.Errorf("the program did not answer")
}

// refusals reports the reasons the linker gave for a reload it would not
// produce.
func refusals(manifest string) []string {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if r, ok := strings.CutPrefix(line, "refuse "); ok {
			out = append(out, r)
		}
	}
	return out
}

// A loadSeg is one loadable piece of a linked image.
type loadSeg struct {
	addr    uint64
	fileoff uint64
	filesz  uint64
	memsz   uint64
	prot    string
}

// loadSegments reads the loadable segments of a linked image, which is an
// ordinary executable file that happens never to be executed — dispatched by
// the format the running program's own toolchain produces.
func loadSegments(path string) (segs []loadSeg, lo, hi uint64, err error) {
	switch runtime.GOOS {
	case "windows":
		return loadSegmentsPE(path)
	case "darwin":
		return loadSegmentsMachO(path)
	default:
		return loadSegmentsELF(path)
	}
}

func loadSegmentsELF(path string) (segs []loadSeg, lo, hi uint64, err error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	lo = ^uint64(0)
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Memsz == 0 {
			continue
		}
		prot := "r"
		switch {
		case p.Flags&elf.PF_X != 0:
			prot = "rx"
		case p.Flags&elf.PF_W != 0:
			prot = "rw"
		}
		segs = append(segs, loadSeg{p.Vaddr, p.Off, p.Filesz, p.Memsz, prot})
		lo = min(lo, p.Vaddr)
		hi = max(hi, p.Vaddr+p.Memsz)
	}
	if len(segs) == 0 {
		return nil, 0, 0, fmt.Errorf("%s has nothing to load", path)
	}
	return segs, lo, hi, nil
}

// loadSegmentsPE reads the loadable sections of a PE image linked with
// -fgohotpin. Unlike ELF, PE encodes addresses as RVAs relative to the
// image's own ImageBase rather than as absolute addresses, so lo has to be
// ImageBase itself — not just the lowest section's address — for the agent's
// RVA arithmetic (used to fix up the image's import table) to land on the
// bytes actually mapped there.
func loadSegmentsPE(path string) (segs []loadSeg, lo, hi uint64, err error) {
	f, err := pe.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	oh, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok {
		return nil, 0, 0, fmt.Errorf("%s is not a PE32+ (64-bit) image", path)
	}
	lo = oh.ImageBase
	hi = oh.ImageBase + uint64(oh.SizeOfImage)
	for _, s := range f.Sections {
		if s.VirtualSize == 0 {
			continue
		}
		prot := "r"
		switch {
		case s.Characteristics&0x20000000 != 0: // IMAGE_SCN_MEM_EXECUTE
			prot = "rx"
		case s.Characteristics&0x80000000 != 0: // IMAGE_SCN_MEM_WRITE
			prot = "rw"
		}
		filesz := uint64(s.Size)
		if filesz > uint64(s.VirtualSize) {
			filesz = uint64(s.VirtualSize) // raw data is file-aligned and may overrun the section
		}
		segs = append(segs, loadSeg{
			addr: oh.ImageBase + uint64(s.VirtualAddress), fileoff: uint64(s.Offset),
			filesz: filesz, memsz: uint64(s.VirtualSize), prot: prot,
		})
	}
	if len(segs) == 0 {
		return nil, 0, 0, fmt.Errorf("%s has nothing to load", path)
	}
	return segs, lo, hi, nil
}

// loadSegmentsMachO reads the loadable segments of a Mach-O image. Addresses
// are absolute, as in ELF — __PAGEZERO (vmaddr 0, no protection at all) is
// the one segment every macOS executable has that isn't meant to be loaded.
func loadSegmentsMachO(path string) (segs []loadSeg, lo, hi uint64, err error) {
	f, err := macho.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	lo = ^uint64(0)
	for _, l := range f.Loads {
		seg, ok := l.(*macho.Segment)
		if !ok || seg.Memsz == 0 || seg.Prot == 0 {
			continue
		}
		prot := "r"
		switch {
		case seg.Prot&0x4 != 0: // VM_PROT_EXECUTE
			prot = "rx"
		case seg.Prot&0x2 != 0: // VM_PROT_WRITE
			prot = "rw"
		}
		segs = append(segs, loadSeg{seg.Addr, seg.Offset, seg.Filesz, seg.Memsz, prot})
		lo = min(lo, seg.Addr)
		hi = max(hi, seg.Addr+seg.Memsz)
	}
	if len(segs) == 0 {
		return nil, 0, 0, fmt.Errorf("%s has nothing to load", path)
	}
	return segs, lo, hi, nil
}

// splitRunArgs separates the package to run from the arguments to pass it,
// following the same rule as `forgo run`: either a list of .go files, or a
// single package.
func splitRunArgs(args []string) (pkg, prog []string) {
	i := 0
	for i < len(args) && isSourceFile(args[i]) {
		i++
	}
	if i == 0 && len(args) > 0 {
		i = 1
	}
	return args[:i], args[i:]
}

// watchRoots returns the directories whose .go files are worth watching: the
// module the package lives in, or failing that the package's own directory.
func watchRoots(pkgArgs []string) []string {
	dir := "."
	if len(pkgArgs) > 0 && (strings.HasPrefix(pkgArgs[0], ".") || filepath.IsAbs(pkgArgs[0]) || isSourceFile(pkgArgs[0])) {
		dir = filepath.Dir(pkgArgs[0])
		if !isSourceFile(pkgArgs[0]) {
			dir = pkgArgs[0]
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return []string{dir}
	}
	for d := abs; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return []string{d}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return []string{abs}
}

// snapshotTree records the size and modification time of every source file
// under the watched roots.
func snapshotTree(roots []string) map[string]string {
	out := map[string]string{}
	for _, root := range roots {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "vendor", "testdata", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !isSourceFile(path) {
				return nil
			}
			if info, err := d.Info(); err == nil {
				out[path] = fmt.Sprintf("%d/%d", info.Size(), info.ModTime().UnixNano())
			}
			return nil
		})
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// exeSuffix is the suffix a program the driver runs directly needs — Windows'
// os/exec insists on it even for an absolute path, unlike Unix. Images are
// never executed, only parsed for their sections, so they don't need it.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// isSourceFile reports whether path names a forgo source file — either
// extension the toolchain accepts.
func isSourceFile(path string) bool {
	return strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".fgo")
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}
