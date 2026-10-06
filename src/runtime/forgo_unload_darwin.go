// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

type forgoSigaction = usigactiont

//go:nosplit
//go:nowritebarrierrec
func forgoGetSigaction(sig uint32, sa *usigactiont) {
	sigaction(sig, nil, sa)
}

func forgoSetSigaction(sig uint32, sa *usigactiont) {
	sigaction(sig, sa, nil)
}

func forgoClosePollerOS() {
	closefd(kq)
}

// forgoDarwinTLSKey is the pthread key that holds g, plus one; set by
// tlsinit on darwin/arm64. The destructor deletes it once Go code is done.
var forgoDarwinTLSKey uintptr

func forgoTLSKey() uintptr { return forgoDarwinTLSKey }
