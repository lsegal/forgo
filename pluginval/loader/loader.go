// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Loader is a VST3 module whose classes live in other Go VST3 modules,
// for validating several Go runtimes in one host process with pluginval.
//
// Its factory exposes one plugin class per Go plugin library in its
// bundle. pluginval tests every class in a module one after another in a
// single process. The factory loads every library before it lists a
// class, the way a DAW loads all of a project's plugins before playing it,
// so each class is tested while all of the Go runtimes are resident: the
// loader's own and one per library.
//
// The libraries sit next to the loader's binary inside the bundle and are
// loaded with RTLD_LOCAL (LoadLibrary on Windows), in the order of the
// libraries array. Each class forwards to its library's own
// GetPluginFactory, so the host calls straight into that library's Go
// code.
//
// The libraries are never unloaded, so the loader must not be either. A
// host may unload a module once it has listed its classes, as pluginval
// does between scanning a module and testing it. The libraries' signal
// handlers sit on top of the loader's and forward to it, and forgo
// libraries can only be unloaded in reverse load order. So before it loads
// them, the loader pins itself in the process.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"unsafe"

	"forgo.dev/pluginval/internal/vst3"
)

// libraries are the base names of the Go plugin libraries, without the
// platform extension. gain and gain-copy are byte-identical, so the gain
// plugin's runtime is loaded twice from two paths.
var libraries = []struct{ file, suffix string }{
	{"forgo-gain", ""},
	{"forgo-drive", ""},
	{"forgo-gain-copy", " (second load)"},
}

// A class is one of a library's classes under an ID of the loader's own,
// since gain and gain-copy share their class IDs.
type class struct {
	info    vst3.ClassInfo2
	cid     vst3.TUID // the class's ID in its library
	factory *vst3.ModuleFactory
}

type loader struct {
	once    sync.Once
	classes []class
}

func (l *loader) load() {
	l.once.Do(func() {
		self, err := modulePath(reflect.ValueOf(openModule).Pointer())
		if err == nil {
			err = pin(self)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "loader: %v\n", err)
			return
		}
		dir := filepath.Dir(self)
		for _, lib := range libraries {
			path := filepath.Join(dir, lib.file+libExt)
			f, err := openModule(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "loader: %s: %v\n", path, err)
				continue
			}
			for _, info := range f.Classes() {
				c := class{info: info, cid: info.CID, factory: f}
				c.info.CID = vst3.UID(0x6F72676F, 0x4C6F6164, 0x00000001, uint32(len(l.classes)+1))
				c.info.SetName(info.NameString() + lib.suffix)
				l.classes = append(l.classes, c)
			}
		}
	})
}

func (l *loader) Info() vst3.FactoryInfo {
	return vst3.NewFactory("forgo", "https://github.com/lsegal/forgo").Info()
}

func (l *loader) Classes() []vst3.ClassInfo2 {
	l.load()
	infos := make([]vst3.ClassInfo2, len(l.classes))
	for i, c := range l.classes {
		infos[i] = c.info
	}
	return infos
}

func (l *loader) CreateInstance(cid, iid *vst3.TUID) (unsafe.Pointer, vst3.Result) {
	l.load()
	for _, c := range l.classes {
		if c.info.CID == *cid {
			return c.factory.CreateInstance(&c.cid, iid)
		}
	}
	return nil, vst3.NoInterface
}

// openModule loads a VST3 module's library and opens its factory.
func openModule(path string) (f *vst3.ModuleFactory, err error) {
	h := dlopen(path)?
	sym := dlsym(h, "GetPluginFactory")?
	f = vst3.OpenFactory(sym)?
	return
}

func init() { vst3.SetFactory(&loader{}) }
