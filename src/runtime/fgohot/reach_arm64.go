// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64

package fgohot

// fgohotWriteJump's own indirect branch has no encoding-imposed reach limit
// (see runtime/forgo_hot_arm64_darwin.go). Calls the hot linker's ordinary
// (compiler-generated) code makes to pinned functions elsewhere in the
// program use direct BL, only +-128MB — but the linker already knows how to
// grow those into trampolines when a target is out of range (see
// cmd/link/internal/ld/data.go's fgohotLinking check, which forces that
// path unconditionally for a hot link, since a hot link's own tiny size
// would never trip the size heuristic that normally decides whether
// trampolines are worth the cost). What a trampoline itself can't get past
// is the +-4GB an ADRP-based load reaches (R_ADDRARM64 in
// cmd/link/internal/arm64/asm.go — both the trampoline's own addressing and
// any direct data reference, e.g. a global variable, use it), and nothing
// promotes that further. So maxReach has to stay safely inside 4GB, with
// margin for the program's own text size, the trampoline's own placement,
// and headroom.
//
// Within that budget, Go's own heap arena reservations (sysReserve's giant
// PROT_NONE placeholders), together with the dyld shared cache and its
// libraries, cover a lot of ground near a small program's text on
// darwin/arm64 (visible as VM_ALLOCATE and mapped-library regions in
// vmmap) — but multi-hundred-MB to multi-GB gaps exist further out, so
// there is room to search for one within a few GB even though there may be
// none within the first few hundred MB. mem_darwin.go's reserve also asks
// for less than it wants rather than fail outright — see minReserve — for
// whatever this doesn't find room for.
const maxReach = 3 << 30 // 3GB
