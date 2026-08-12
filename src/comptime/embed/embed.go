// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package embed provides filesystem helpers that forgo's compiler
// evaluates natively at compile time when called directly as a const
// initializer (see AGENTS.md rule 2), in addition to working normally at
// runtime like any other function.
//
// At compile time, a relative path is resolved against the directory of
// the source file containing the call, so `embed.ReadFile("data.txt")`
// reads the file next to the .go/.fgo source it's written in. At runtime,
// relative paths are resolved the normal way, against the process's
// current working directory.
//
//	const banner = embed.ReadFile("banner.txt")
//	const hasConfig = embed.Exists("config.json")
//	const assetNames = embed.ReadDir("assets") // newline-joined names
//
// Every scalar-returning function here panics on error rather than
// returning one, since a (string, error) result can't be folded into a
// single compile-time constant. Callers that need to handle a
// missing/unreadable file gracefully at runtime should use the os package
// directly instead.
//
// # Filesystems
//
// [Load] embeds a whole tree of files into an [FS], the way the standard
// library's `//go:embed` directive populates a `embed.FS` -- but folded
// through a const initializer like everything else in this package,
// instead of a directive:
//
//	const content = embed.Load("image", "template", "html/index.html")
//	data := content.ReadFile("image/hello.jpg")
//
//	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(content))))
//
// A pattern naming a directory embeds every file in that directory's
// subtree, skipping names beginning with '.' or '_'; a plain path embeds
// exactly that one file. Embedded file paths always use '/', even on
// Windows. FS implements [io/fs.FS], [io/fs.ReadFileFS], and
// [io/fs.ReadDirFS], so it works anywhere those are accepted.
package embed

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// ReadFile returns the contents of the file at path as a string.
func ReadFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// ReadFileRange returns the length bytes of the file at path starting at
// offset, as a string. It's the "seek" of this package: a way to pull a
// slice out of a file without reading (or folding into the binary) the
// whole thing.
func ReadFileRange(path string, offset, length int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if offset < 0 || length < 0 || offset+length > len(data) {
		panic("embed: ReadFileRange: range out of bounds")
	}
	return string(data[offset : offset+length])
}

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsDir reports whether path exists and is a directory.
func IsDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		panic(err)
	}
	return info.IsDir()
}

// ReadDir returns the names of path's directory entries, one per line,
// sorted by filename.
func ReadDir(path string) string {
	entries, err := os.ReadDir(path)
	if err != nil {
		panic(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return strings.Join(names, "\n")
}

// Getwd returns the process's current working directory.
func Getwd() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return wd
}

// file is a single embedded file.
type file struct {
	name string
	data string
}

func (f *file) Name() string               { _, elem, _ := split(f.name); return elem }
func (f *file) Size() int64                { return int64(len(f.data)) }
func (f *file) ModTime() time.Time         { return time.Time{} }
func (f *file) IsDir() bool                { _, _, isDir := split(f.name); return isDir }
func (f *file) Sys() any                   { return nil }
func (f *file) Type() fs.FileMode          { return f.Mode().Type() }
func (f *file) Info() (fs.FileInfo, error) { return f, nil }

func (f *file) Mode() fs.FileMode {
	if f.IsDir() {
		return fs.ModeDir | 0555
	}
	return 0444
}

func (f *file) String() string { return fs.FormatFileInfo(f) }

var (
	_ fs.FileInfo = (*file)(nil)
	_ fs.DirEntry = (*file)(nil)
)

// split splits name into dir and elem the way [FS]'s files list is
// sorted: dir is everything before the final path element (or "." at the
// root), elem is the final element, and isDir reports whether name ended
// in a trailing slash (marking a directory entry).
func split(name string) (dir, elem string, isDir bool) {
	name, isDir = strings.CutSuffix(name, "/")
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return ".", name, isDir
	}
	return name[:i], name[i+1:], isDir
}

// dotFile represents the root directory, which is never itself present
// in an FS's files list.
var dotFile = &file{name: "./"}

// FS is a read-only collection of files, usually built with [Load]. The
// zero FS is an empty filesystem.
//
// FS implements [io/fs.FS] (plus [io/fs.ReadFileFS] and
// [io/fs.ReadDirFS]), so it works with any package that understands
// filesystem interfaces, including net/http, text/template, and
// html/template.
type FS struct {
	// files is sorted by (dir, elem) as described by split, so a
	// directory's contents form one contiguous run findable by binary
	// search -- see lookup/readDir.
	files []file
}

var (
	_ fs.FS         = FS{}
	_ fs.ReadFileFS = FS{}
	_ fs.ReadDirFS  = FS{}
)

func (f FS) lookup(name string) *file {
	if !fs.ValidPath(name) {
		return nil
	}
	if name == "." {
		return dotFile
	}
	dir, elem, _ := split(name)
	i := sortSearch(len(f.files), func(i int) bool {
		idir, ielem, _ := split(f.files[i].name)
		return idir > dir || idir == dir && ielem >= elem
	})
	if i < len(f.files) && strings.TrimSuffix(f.files[i].name, "/") == name {
		return &f.files[i]
	}
	return nil
}

func (f FS) readDir(dir string) []file {
	i := sortSearch(len(f.files), func(i int) bool {
		idir, _, _ := split(f.files[i].name)
		return idir >= dir
	})
	j := sortSearch(len(f.files), func(j int) bool {
		jdir, _, _ := split(f.files[j].name)
		return jdir > dir
	})
	return f.files[i:j]
}

// Open opens the named file for reading and returns it as an [io/fs.File].
//
// The returned file implements [io.Seeker] and [io.ReaderAt] when the
// file is not a directory.
func (f FS) Open(name string) (fs.File, error) {
	fl := f.lookup(name)
	if fl == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if fl.IsDir() {
		return &openDir{fl, f.readDir(name), 0}, nil
	}
	return &openFile{fl, 0}, nil
}

// ReadDir reads and returns the entire named directory.
func (f FS) ReadDir(name string) ([]fs.DirEntry, error) {
	file, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	dir, ok := file.(*openDir)
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("not a directory")}
	}
	list := make([]fs.DirEntry, len(dir.files))
	for i := range list {
		list[i] = &dir.files[i]
	}
	return list, nil
}

// ReadFile reads and returns the content of the named file.
func (f FS) ReadFile(name string) ([]byte, error) {
	file, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	ofile, ok := file.(*openFile)
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("is a directory")}
	}
	return []byte(ofile.f.data), nil
}

type openFile struct {
	f      *file
	offset int64
}

var (
	_ io.Seeker   = (*openFile)(nil)
	_ io.ReaderAt = (*openFile)(nil)
)

func (f *openFile) Close() error               { return nil }
func (f *openFile) Stat() (fs.FileInfo, error) { return f.f, nil }

func (f *openFile) Read(b []byte) (int, error) {
	if f.offset >= int64(len(f.f.data)) {
		return 0, io.EOF
	}
	if f.offset < 0 {
		return 0, &fs.PathError{Op: "read", Path: f.f.name, Err: fs.ErrInvalid}
	}
	n := copy(b, f.f.data[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *openFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.offset
	case io.SeekEnd:
		offset += int64(len(f.f.data))
	}
	if offset < 0 || offset > int64(len(f.f.data)) {
		return 0, &fs.PathError{Op: "seek", Path: f.f.name, Err: fs.ErrInvalid}
	}
	f.offset = offset
	return offset, nil
}

func (f *openFile) ReadAt(b []byte, offset int64) (int, error) {
	if offset < 0 || offset > int64(len(f.f.data)) {
		return 0, &fs.PathError{Op: "read", Path: f.f.name, Err: fs.ErrInvalid}
	}
	n := copy(b, f.f.data[offset:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

type openDir struct {
	f      *file
	files  []file
	offset int
}

func (d *openDir) Close() error               { return nil }
func (d *openDir) Stat() (fs.FileInfo, error) { return d.f, nil }

func (d *openDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.f.name, Err: errors.New("is a directory")}
}

func (d *openDir) ReadDir(count int) ([]fs.DirEntry, error) {
	n := len(d.files) - d.offset
	if n == 0 {
		if count <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	if count > 0 && n > count {
		n = count
	}
	list := make([]fs.DirEntry, n)
	for i := range list {
		list[i] = &d.files[d.offset+i]
	}
	d.offset += n
	return list, nil
}

// sortSearch is sort.Search, inlined to avoid importing "sort" here.
func sortSearch(n int, f func(int) bool) int {
	i, j := 0, n
	for i < j {
		h := int(uint(i+j) >> 1)
		if !f(h) {
			i = h + 1
		} else {
			j = h
		}
	}
	return i
}

// Load embeds the files matched by patterns into an [FS], following the
// same rules as the standard library's `//go:embed` directive: a pattern
// naming a directory embeds every file in that directory's subtree
// (skipping names beginning with '.' or '_'), and a plain path embeds
// exactly that one file. Load is evaluated natively by forgo's compiler
// when called directly as a const initializer (see AGENTS.md rule 2);
// this implementation is what runs when Load is instead called as an
// ordinary function, e.g. assigned to a var or called inside a function
// body. At compile time, relative patterns resolve against the source
// file's directory; at runtime they resolve against the process's
// current working directory, matching the rest of this package.
func Load(patterns ...string) FS {
	var files []file
	seen := map[string]bool{}
	for _, p := range patterns {
		info, err := os.Stat(p)
		if err != nil {
			panic(err)
		}
		if info.IsDir() {
			loadDir(p, p, &files, seen)
			continue
		}
		loadFile(p, p, &files, seen)
	}
	files = addDirEntries(files)
	insertionSortFiles(files)
	return FS{files: files}
}

// addDirEntries appends a directory marker entry (name ending in '/',
// no data) for every ancestor directory of every file in files, so
// FS.lookup/readDir can find and list "template" as a directory the way
// it finds "template/page.tmpl" as a file -- mirroring how the real
// `//go:embed` directive's compiler-generated FS always includes an
// explicit entry for each embedded directory, not just its leaf files.
func addDirEntries(files []file) []file {
	seen := map[string]bool{}
	var dirs []file
	for _, f := range files {
		for dir, _, _ := split(f.name); dir != "."; {
			if !seen[dir] {
				seen[dir] = true
				dirs = append(dirs, file{name: dir + "/"})
			}
			dir, _, _ = split(dir)
		}
	}
	return append(files, dirs...)
}

func loadDir(diskDir, fsDir string, files *[]file, seen map[string]bool) {
	entries, err := os.ReadDir(diskDir)
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		diskPath := diskDir + "/" + name
		fsPath := fsDir + "/" + name
		if e.IsDir() {
			loadDir(diskPath, fsPath, files, seen)
			continue
		}
		loadFile(fsPath, diskPath, files, seen)
	}
}

func loadFile(fsPath, diskPath string, files *[]file, seen map[string]bool) {
	if seen[fsPath] {
		return
	}
	seen[fsPath] = true
	data, err := os.ReadFile(diskPath)
	if err != nil {
		panic(err)
	}
	*files = append(*files, file{name: fsPath, data: string(data)})
}

// insertionSortFiles sorts files by (dir, elem) as required by
// lookup/readDir's binary search, without importing "sort".
func insertionSortFiles(files []file) {
	less := func(a, b file) bool {
		adir, aelem, _ := split(a.name)
		bdir, belem, _ := split(b.name)
		if adir != bdir {
			return adir < bdir
		}
		return aelem < belem
	}
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && less(files[j], files[j-1]); j-- {
			files[j], files[j-1] = files[j-1], files[j]
		}
	}
}
