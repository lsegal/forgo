// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fgohot

import (
	"bytes"
	"debug/pe"
	"unsafe"
)

// resolveImports fixes up the Import Address Table of a manually mapped PE
// image, the way the OS loader would for one it loaded normally.
//
// runtime/fgohot maps an image itself — VirtualAlloc, copy the bytes,
// VirtualProtect — and never hands it to the OS loader, so nothing else ever
// performs this step. Skipping it would leave any import the image makes
// pointing at zeroed memory.
//
// In practice this rarely does real work. A reload only introduces a new
// import if the changed code calls a Windows API that the running program
// didn't already call — ordinary application code reaches Win32 through
// package syscall/os, and those packages essentially never change, so they
// stay pinned to the host's own copy, already resolved when the host process
// started. This function exists for the rare case that isn't true.
func resolveImports(base uintptr, image []byte) string {
	f, err := pe.NewFile(bytes.NewReader(image))
	if err != nil {
		return "cannot read image for import resolution: " + err.Error()
	}
	oh, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok {
		return "hot reload requires a PE32+ (64-bit) image"
	}
	dir := oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_IMPORT]
	if dir.Size == 0 {
		return "" // nothing imported that wasn't already resolved in the running process
	}

	for off := uint32(0); off < dir.Size; off += peImportDescriptorSize {
		desc := (*peImportDescriptor)(unsafe.Pointer(base + uintptr(dir.VirtualAddress) + uintptr(off)))
		if desc.name == 0 && desc.firstThunk == 0 {
			break // terminating all-zero entry
		}
		h, _, err := procLoadLibraryA.Call(base + uintptr(desc.name))
		if h == 0 {
			return "cannot load " + cString(base+uintptr(desc.name)) + ": " + err.Error()
		}

		thunkRVA := desc.originalFirstThunk
		if thunkRVA == 0 {
			thunkRVA = desc.firstThunk
		}
		for i := uintptr(0); ; i += 8 {
			thunk := (*uint64)(unsafe.Pointer(base + uintptr(thunkRVA) + i))
			iat := (*uint64)(unsafe.Pointer(base + uintptr(desc.firstThunk) + i))
			if *thunk == 0 {
				break
			}
			var proc uintptr
			if *thunk&0x8000000000000000 != 0 {
				proc, _, err = procGetProcAddress.Call(h, uintptr(*thunk&0xffff))
			} else {
				// IMAGE_IMPORT_BY_NAME{Hint uint16, Name [...]byte}; the name
				// starts two bytes past the RVA this thunk stores.
				nameAddr := base + uintptr(uint32(*thunk)) + 2
				proc, _, err = procGetProcAddress.Call(h, nameAddr)
			}
			if proc == 0 {
				return "cannot resolve an import from " + cString(base+uintptr(desc.name)) + ": " + err.Error()
			}
			*iat = uint64(proc)
		}
	}
	return ""
}

// peImportDescriptor mirrors IMAGE_IMPORT_DESCRIPTOR.
type peImportDescriptor struct {
	originalFirstThunk uint32
	timeDateStamp      uint32
	forwarderChain     uint32
	name               uint32
	firstThunk         uint32
}

const peImportDescriptorSize = 20

// cString reads a NUL-terminated ASCII string directly out of the mapped
// image — PE import names are always ASCII.
func cString(addr uintptr) string {
	var b []byte
	for i := uintptr(0); i < 512; i++ {
		c := *(*byte)(unsafe.Pointer(addr + i))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}
