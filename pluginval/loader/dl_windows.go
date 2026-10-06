// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"syscall"
	"unsafe"
)

const libExt = ".dll"

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleHandleEx = kernel32.NewProc("GetModuleHandleExW")
	procGetModuleFileName = kernel32.NewProc("GetModuleFileNameW")
)

const (
	getModuleHandleExFlagPin               = 0x1
	getModuleHandleExFlagUnchangedRefcount = 0x2
	getModuleHandleExFlagFromAddress       = 0x4
)

func dlopen(path string) (unsafe.Pointer, error) {
	h, err := syscall.LoadLibrary(path)
	if err != nil {
		return nil, err
	}
	return unsafe.Pointer(h), nil
}

func dlsym(h unsafe.Pointer, name string) (unsafe.Pointer, error) {
	p, err := syscall.GetProcAddress(syscall.Handle(h), name)
	if err != nil {
		return nil, err
	}
	return unsafe.Pointer(p), nil
}

// pin keeps the already loaded library at path loaded until the process
// exits.
func pin(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var h syscall.Handle
	r, _, err := procGetModuleHandleEx.Call(getModuleHandleExFlagPin, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&h)))
	if r == 0 {
		return err
	}
	return nil
}

// modulePath returns the path of the library that holds address pc.
func modulePath(pc uintptr) (string, error) {
	var h syscall.Handle
	r, _, err := procGetModuleHandleEx.Call(
		getModuleHandleExFlagFromAddress|getModuleHandleExFlagUnchangedRefcount,
		pc, uintptr(unsafe.Pointer(&h)))
	if r == 0 {
		return "", err
	}
	buf := make([]uint16, syscall.MAX_PATH)
	for {
		n, _, err := procGetModuleFileName.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 {
			return "", err
		}
		if int(n) < len(buf) {
			return syscall.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, 2*len(buf))
	}
}
