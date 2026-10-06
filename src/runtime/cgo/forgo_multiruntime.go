// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux || darwin || freebsd

package cgo

import _ "unsafe" // for go:linkname

// See gcc_forgo_multiruntime.c and runtime/forgo_multiruntime_unix.go.

//go:cgo_import_static x_cgo_forgo_setsighandler
//go:linkname x_cgo_forgo_setsighandler x_cgo_forgo_setsighandler
//go:linkname _cgo_forgo_setsighandler runtime._cgo_forgo_setsighandler
var x_cgo_forgo_setsighandler byte
var _cgo_forgo_setsighandler = &x_cgo_forgo_setsighandler

//go:cgo_import_static x_cgo_forgo_isgosighandler
//go:linkname x_cgo_forgo_isgosighandler x_cgo_forgo_isgosighandler
//go:linkname _cgo_forgo_isgosighandler runtime._cgo_forgo_isgosighandler
var x_cgo_forgo_isgosighandler byte
var _cgo_forgo_isgosighandler = &x_cgo_forgo_isgosighandler
