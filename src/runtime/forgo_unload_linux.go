// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

type forgoSigaction = sigactiont

//go:nosplit
//go:nowritebarrierrec
func forgoGetSigaction(sig uint32, sa *sigactiont) {
	sigaction(sig, nil, sa)
}

// forgoSigactionHandler returns the handler sa installs.
func forgoSigactionHandler(sa *sigactiont) uintptr {
	return sa.sa_handler
}

func forgoSetSigaction(sig uint32, sa *sigactiont) {
	sigaction(sig, sa, nil)
}

func forgoClosePollerOS() {
	closefd(epfd)
	closefd(int32(netpollEventFd))
}

func forgoTLSKey() uintptr { return 0 }
