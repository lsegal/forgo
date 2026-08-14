// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && arm64

package fgohot

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"strings"
	"unsafe"
)

const (
	machoNonLazySymbolPointers = 0x6
	machoLazySymbolPointers    = 0x7
	machoSectionTypeMask       = 0xff
	machoIndirectSymbolLocal   = 0x80000000
	machoIndirectSymbolAbs     = 0x40000000
	rtldLazy                   = 0x1
	rtldGlobal                 = 0x8
)

func rtdlopen(name *byte, flags int32) uintptr
func rtdlsym(handle uintptr, name *byte) uintptr

// resolveMachOImports performs the part of dyld that a manually mapped Go
// image needs: populate its indirect symbol pointers. Internal linking emits
// imported calls and data references through these slots.
func resolveMachOImports(image []byte) string {
	f, err := macho.NewFile(bytes.NewReader(image))
	if err != nil {
		return "cannot parse Mach-O imports: " + err.Error()
	}
	defer f.Close()
	if f.Symtab == nil || f.Dysymtab == nil {
		return ""
	}

	handles := make([]uintptr, 0, len(f.Loads))
	for _, load := range f.Loads {
		dylib, ok := load.(*macho.Dylib)
		if !ok {
			continue
		}
		name := cBytes(dylib.Name)
		handle := rtdlopen(&name[0], rtldLazy|rtldGlobal)
		if handle == 0 {
			return "cannot load Mach-O dependency " + dylib.Name
		}
		handles = append(handles, handle)
	}

	reserved, msg := machoReserved1(f)
	if msg != "" {
		return msg
	}
	for _, section := range f.Sections {
		typ := section.Flags & machoSectionTypeMask
		if typ != machoNonLazySymbolPointers && typ != machoLazySymbolPointers {
			continue
		}
		first, ok := reserved[section.Addr]
		if !ok {
			return "cannot locate Mach-O indirect symbol range for " + section.Seg + "," + section.Name
		}
		count := section.Size / 8
		if uint64(first)+count > uint64(len(f.Dysymtab.IndirectSyms)) {
			return "Mach-O indirect symbol range is out of bounds"
		}
		for i := uint64(0); i < count; i++ {
			index := f.Dysymtab.IndirectSyms[uint64(first)+i]
			if index&(machoIndirectSymbolLocal|machoIndirectSymbolAbs) != 0 {
				continue
			}
			if int(index) >= len(f.Symtab.Syms) {
				return "Mach-O indirect symbol index is out of bounds"
			}
			name := strings.TrimPrefix(f.Symtab.Syms[index].Name, "_")
			address := lookupMachOSymbol(handles, name)
			if address == 0 {
				return "cannot resolve Mach-O import " + name
			}
			*(*uintptr)(unsafe.Pointer(uintptr(section.Addr) + uintptr(i*8))) = address
		}
	}
	return ""
}

func lookupMachOSymbol(handles []uintptr, name string) uintptr {
	cname := cBytes(name)
	for _, handle := range handles {
		if address := rtdlsym(handle, &cname[0]); address != 0 {
			return address
		}
	}
	return 0
}

func cBytes(s string) []byte {
	return append([]byte(s), 0)
}

// debug/macho intentionally exposes section flags but not reserved1, the
// indirect-symbol-table start index. Recover that one field from each raw
// LC_SEGMENT_64 command while still using debug/macho for the rest.
func machoReserved1(f *macho.File) (map[uint64]uint32, string) {
	out := make(map[uint64]uint32)
	for _, load := range f.Loads {
		segment, ok := load.(*macho.Segment)
		if !ok || segment.Cmd != macho.LoadCmdSegment64 {
			continue
		}
		r := bytes.NewReader(segment.LoadBytes)
		var header macho.Segment64
		if err := binary.Read(r, f.ByteOrder, &header); err != nil {
			return nil, "cannot parse Mach-O segment: " + err.Error()
		}
		for range header.Nsect {
			var section macho.Section64
			if err := binary.Read(r, f.ByteOrder, &section); err != nil {
				return nil, "cannot parse Mach-O section: " + err.Error()
			}
			out[section.Addr] = section.Reserve1
		}
	}
	return out, ""
}
