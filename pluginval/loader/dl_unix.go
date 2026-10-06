// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package main

// The dynamic loader's functions, declared by hand instead of including
// dlfcn.h. Their constants are in dl_*.go. cmd/cgo cannot parse forgo's
// syntax, so this file is plain Go.

/*
#cgo linux LDFLAGS: -ldl
extern void* dlopen(const char*, int);
extern void* dlsym(void*, const char*);
extern char* dlerror(void);
extern int dladdr(const void*, void*);
extern void free(void*);
*/
import "C"

import (
	"errors"
	"unsafe"
)

func dlError() error {
	if msg := C.dlerror(); msg != nil {
		return errors.New(C.GoString(msg))
	}
	return errors.New("unknown error")
}

func dlopen(path string) (unsafe.Pointer, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	h := C.dlopen(cpath, rtldNow|rtldLocal)
	if h == nil {
		return nil, dlError()
	}
	return h, nil
}

func dlsym(h unsafe.Pointer, name string) (unsafe.Pointer, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	p := C.dlsym(h, cname)
	if p == nil {
		return nil, dlError()
	}
	return p, nil
}

// dlInfo is Dl_info.
type dlInfo struct {
	fname *C.char
	fbase unsafe.Pointer
	sname *C.char
	saddr unsafe.Pointer
}

// modulePath returns the path of the library that holds address pc.
func modulePath(pc uintptr) (string, error) {
	var info dlInfo
	if C.dladdr(unsafe.Pointer(pc), unsafe.Pointer(&info)) == 0 || info.fname == nil {
		return "", errors.New("dladdr failed")
	}
	return C.GoString(info.fname), nil
}
