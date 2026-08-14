// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !darwin || !arm64

package fgohot

func patchText(pairs []uintptr) string {
	lo, hi := patchBounds(pairs)
	beginPatch()
	defer endPatch()
	if err := protect(lo, hi-lo, protRWX); err != nil {
		return "cannot unprotect text: " + err.Error()
	}
	msg := patchFuncs(pairs)
	if err := protect(lo, hi-lo, protRX); err != nil && msg == "" {
		msg = "cannot reprotect text: " + err.Error()
	}
	return msg
}
