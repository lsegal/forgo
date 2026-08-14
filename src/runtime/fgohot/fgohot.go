// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fgohot is the in-process half of forgo's hot reload.
//
// It is linked into a program only when it is built by `forgo run --watch`,
// and it does nothing at all unless the environment variable FORGO_HOT_DIR
// names a directory the watcher has prepared. When it is active it:
//
//   - reserves a region of address space close to the program's own code,
//     and tells the watcher where that region is;
//   - waits for the watcher to hand it a freshly linked image of the code
//     that changed;
//   - maps the image, registers it with the runtime, runs the init functions
//     of packages that are new, and redirects every changed function's entry
//     point at its new body.
//
// Nothing in the running program is restarted or re-initialized: goroutines
// keep running, the heap is untouched, and package-level variables keep their
// values, because the image the watcher hands over refers to those variables
// at the addresses they already live at.
//
// The protocol is deliberately plain files in a directory, so it behaves the
// same on Linux, macOS and Windows and pulls in no dependencies beyond os.
package fgohot

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// reserveSize is how much address space the agent asks reserve for up front
// for reloaded code. Each reload consumes a fresh slice of it, so this
// bounds how many times a program can be reloaded before it must be
// restarted. Only a target: reserve may hand back less (see mem_darwin.go),
// in which case regSize — what the agent actually tells the watcher it
// has — reflects the smaller, real amount.
const reserveSize = 512 << 20

// pollInterval is how often the agent looks for a new request.
const pollInterval = 100 * time.Millisecond

var (
	dir      string
	regBase  uintptr
	regSize  uintptr
	lastSeen string
)

func init() {
	dir = os.Getenv("FORGO_HOT_DIR")
	if dir == "" {
		return
	}
	if !supported() {
		writeFile("agent.err", "hot reload is not supported on this architecture\n")
		return
	}
	textStart, textEnd := textRange()
	base, size, err := reserve(textEnd, reserveSize)
	if err != nil {
		writeFile("agent.err", "hot reload could not reserve address space: "+err.Error()+"\n")
		return
	}
	regBase, regSize = base, size

	// Publish what the watcher needs in order to link images that fit into
	// this process: where its code lives and where new code may go.
	//
	// landmark is this process's own actual address for a function (serve,
	// below) that the host build's symbol record also names — the two
	// together are how the watcher recovers the OS's ASLR slide on a
	// platform that forces PIE (darwin/arm64, windows/arm64), where the
	// record's addresses only describe where the linker put things, not
	// where this particular launch of the program actually landed.
	writeFile("agent", strings.Join([]string{
		"pid " + strconv.Itoa(os.Getpid()),
		"base " + hex(regBase),
		"size " + hex(regSize),
		"text " + hex(textStart) + " " + hex(textEnd),
		"landmark " + hex(reflect.ValueOf(serve).Pointer()),
		"",
	}, "\n"))

	go serve()
}

// serve watches the control directory for reload requests.
func serve() {
	for {
		time.Sleep(pollInterval)
		req, err := os.ReadFile(filepath.Join(dir, "request"))
		if err != nil {
			continue
		}
		gen := strings.TrimSpace(string(req))
		if gen == "" || gen == lastSeen {
			continue
		}
		lastSeen = gen
		msg := apply(gen)
		if msg == "" {
			writeFile("resp-"+gen, "ok\n")
		} else {
			writeFile("resp-"+gen, "err "+msg+"\n")
		}
	}
}

// A request describes one reload: an image to map and what to do with it.
type request struct {
	base       uintptr
	size       uintptr
	moduledata uintptr
	segments   []segment
	patches    []uintptr // (old, new) pairs
}

// A segment is one loadable piece of the image: filesz bytes taken from the
// image file, placed at addr, occupying memsz bytes there (anything past
// filesz is zero — freshly committed pages already are).
type segment struct {
	addr    uintptr
	fileoff uintptr
	filesz  uintptr
	memsz   uintptr
	prot    int
}

// apply carries out the reload described by req-<gen> and img-<gen>.
func apply(gen string) string {
	req, msg := readRequest(filepath.Join(dir, "req-"+gen))
	if msg != "" {
		return msg
	}
	image, err := os.ReadFile(filepath.Join(dir, "img-"+gen))
	if err != nil {
		return "cannot read image: " + err.Error()
	}
	if req.base < regBase || req.base+req.size > regBase+regSize {
		return "image does not fit in the reserved region; restart the program"
	}

	// Back the image's address range with memory, lay each segment down where
	// it was linked to run, then give it the protection it runs with.
	if err := commit(req.base, req.size); err != nil {
		return "cannot map image: " + err.Error()
	}
	for _, s := range req.segments {
		if s.addr < req.base || s.addr+s.memsz > req.base+req.size ||
			s.fileoff+s.filesz > uintptr(len(image)) || s.filesz > s.memsz {
			return "manifest describes a segment outside the image"
		}
		copyIn(s.addr, image[s.fileoff:s.fileoff+s.filesz])
	}
	for _, s := range req.segments {
		if err := protect(s.addr, s.memsz, s.prot); err != nil {
			return "cannot protect image: " + err.Error()
		}
	}

	// On Windows, nothing else ever resolves this image's imports: it was
	// never handed to the OS loader. A no-op everywhere else.
	if msg := resolveImports(req.base, image); msg != "" {
		return msg
	}
	if msg := resolveMachOImports(image); msg != "" {
		return msg
	}

	// Publish the new code to the runtime before anything can jump into it,
	// so a garbage collection that starts mid-reload can still unwind it.
	if msg := addModule(req.moduledata); msg != "" {
		return msg
	}
	doInitTasks(moduleInitTasks(req.moduledata))

	if len(req.patches) > 0 {
		if msg := patchText(req.patches); msg != "" {
			return msg
		}
	}
	return ""
}

// patchBounds returns the page-aligned range covering every patch site.
func patchBounds(patches []uintptr) (lo, hi uintptr) {
	lo = ^uintptr(0)
	for i := 0; i < len(patches); i += 2 {
		if patches[i] < lo {
			lo = patches[i]
		}
		if patches[i]+16 > hi {
			hi = patches[i] + 16
		}
	}
	ps := uintptr(pageSize())
	lo &^= ps - 1
	hi = (hi + ps - 1) &^ (ps - 1)
	return
}

// readRequest parses the manifest the hot linker wrote for this reload.
//
// The format is one directive per line:
//
//	base <hex>
//	size <hex>
//	moduledata <hex>
//	segment <hex addr> <hex file offset> <hex file size> <hex mem size> r|rw|rx
//	patch <hex old entry> <hex new entry>
func readRequest(path string) (*request, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "cannot read manifest: " + err.Error()
	}
	req := &request{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "base":
			req.base = parseHex(f, 1)
		case "size":
			req.size = parseHex(f, 1)
		case "moduledata":
			req.moduledata = parseHex(f, 1)
		case "segment":
			if len(f) < 6 {
				return nil, "malformed segment line in manifest"
			}
			p, ok := protByName(f[5])
			if !ok {
				return nil, "unknown segment protection " + f[5]
			}
			req.segments = append(req.segments, segment{
				addr: parseHex(f, 1), fileoff: parseHex(f, 2),
				filesz: parseHex(f, 3), memsz: parseHex(f, 4), prot: p,
			})
		case "patch":
			if len(f) < 3 {
				return nil, "malformed patch line in manifest"
			}
			req.patches = append(req.patches, parseHex(f, 1), parseHex(f, 2))
		}
	}
	if req.base == 0 || req.size == 0 || req.moduledata == 0 {
		return nil, "manifest is missing base, size or moduledata"
	}
	return req, ""
}

func protByName(s string) (int, bool) {
	switch s {
	case "r":
		return protR, true
	case "rw":
		return protRW, true
	case "rx":
		return protRX, true
	}
	return 0, false
}

func parseHex(f []string, i int) uintptr {
	if i >= len(f) {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(f[i], "0x"), 16, 64)
	if err != nil {
		return 0
	}
	return uintptr(v)
}

func hex(v uintptr) string {
	return "0x" + strconv.FormatUint(uint64(v), 16)
}

func writeFile(name, content string) {
	tmp := filepath.Join(dir, name+".tmp")
	if os.WriteFile(tmp, []byte(content), 0o600) == nil {
		os.Rename(tmp, filepath.Join(dir, name))
	}
}
