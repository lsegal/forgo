// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package cgo

import _ "unsafe" // for go:linkname

// C functions for unloading a c-shared library; see
// gcc_forgo_unload_unix.c and runtime/forgo_unload.go.

//go:cgo_import_static x_cgo_forgo_lib_init
//go:linkname x_cgo_forgo_lib_init x_cgo_forgo_lib_init
//go:linkname _cgo_forgo_lib_init _cgo_forgo_lib_init
var x_cgo_forgo_lib_init byte
var _cgo_forgo_lib_init = &x_cgo_forgo_lib_init

//go:cgo_import_static x_cgo_forgo_join
//go:linkname x_cgo_forgo_join x_cgo_forgo_join
//go:linkname _cgo_forgo_join _cgo_forgo_join
var x_cgo_forgo_join byte
var _cgo_forgo_join = &x_cgo_forgo_join

//go:cgo_import_static x_cgo_forgo_m0_exit
//go:linkname x_cgo_forgo_m0_exit x_cgo_forgo_m0_exit
//go:linkname _cgo_forgo_m0_exit _cgo_forgo_m0_exit
var x_cgo_forgo_m0_exit byte
var _cgo_forgo_m0_exit = &x_cgo_forgo_m0_exit

//go:cgo_import_static x_cgo_forgo_sigstub_init
//go:linkname x_cgo_forgo_sigstub_init x_cgo_forgo_sigstub_init
//go:linkname _cgo_forgo_sigstub_init _cgo_forgo_sigstub_init
var x_cgo_forgo_sigstub_init byte
var _cgo_forgo_sigstub_init = &x_cgo_forgo_sigstub_init
