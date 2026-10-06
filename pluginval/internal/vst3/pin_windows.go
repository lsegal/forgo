// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

import (
	"reflect"
	"syscall"
	"unsafe"
)

// A host may unload a VST3 module once it has listed its classes and load
// it again later, as pluginval does between scanning a module and opening
// it. A Go c-shared library cannot be unloaded: its runtime's threads keep
// running in the unmapped code. ELF libraries are linked with -z nodelete
// and macOS keeps them loaded, but Windows unloads them, so the module
// pins itself in the process here.

const (
	getModuleHandleExFlagPin         = 0x1
	getModuleHandleExFlagFromAddress = 0x4
)

func init() {
	var h syscall.Handle
	r, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleExW").Call(
		getModuleHandleExFlagPin|getModuleHandleExFlagFromAddress,
		reflect.ValueOf(GetPluginFactory).Pointer(), uintptr(unsafe.Pointer(&h)))
	if r == 0 {
		panic("vst3: cannot pin the module: " + err.Error())
	}
}
