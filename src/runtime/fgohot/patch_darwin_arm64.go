// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && arm64

package fgohot

import (
	"strconv"
	"syscall"
	"unsafe"
)

func rtmmap(size uintptr, prot int32) (uintptr, int32)
func rtmunmap(addr, size uintptr)
func rtremap(target, source, size uintptr) int32

type preparedPage struct {
	target uintptr
	copy   uintptr
}

// patchText prepares private copies of the affected text pages, writes the
// jumps there, seals them RX, and atomically remaps them over the originals.
// At no point is a live text page writable or non-executable.
func patchText(pairs []uintptr) string {
	beginPatch()
	defer endPatch()

	if msg := checkPatchFuncs(pairs); msg != "" {
		return msg
	}

	ps := uintptr(pageSize())
	pages := make([]preparedPage, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		page := pairs[i] &^ (ps - 1)
		if hasPreparedPage(pages, page) {
			continue
		}
		copyPage, errno := rtmmap(ps, int32(protRW))
		if errno != 0 {
			releasePreparedPages(pages)
			return "cannot prepare text patch: " + syscall.Errno(errno).Error()
		}
		copyIn(copyPage, unsafe.Slice((*byte)(unsafe.Pointer(page)), int(ps)))
		pages = append(pages, preparedPage{page, copyPage})
	}

	adjusted := make([]uintptr, len(pairs))
	copy(adjusted, pairs)
	for i := 0; i < len(adjusted); i += 2 {
		page := adjusted[i] &^ (ps - 1)
		for _, p := range pages {
			if p.target == page {
				adjusted[i] = p.copy + adjusted[i] - page
				break
			}
		}
	}
	if msg := patchFuncs(adjusted); msg != "" {
		releasePreparedPages(pages)
		return msg
	}
	for _, p := range pages {
		if err := protect(p.copy, ps, protRX); err != nil {
			releasePreparedPages(pages)
			return "cannot seal text patch: " + err.Error()
		}
	}
	for i, p := range pages {
		if kr := rtremap(p.target, p.copy, ps); kr != 0 {
			releasePreparedPages(pages[i:])
			return "cannot install text patch: mach_vm_remap failed with kernel code " + strconv.Itoa(int(kr))
		}
		rtmunmap(p.copy, ps)
		flushICache(p.target, ps)
	}
	return ""
}

func hasPreparedPage(pages []preparedPage, target uintptr) bool {
	for _, p := range pages {
		if p.target == target {
			return true
		}
	}
	return false
}

func releasePreparedPages(pages []preparedPage) {
	for _, p := range pages {
		rtmunmap(p.copy, uintptr(pageSize()))
	}
}
