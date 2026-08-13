// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux && !windows

package fgohot

import (
	"errors"
	"runtime"
	"syscall"
)

// Hot reload needs to place freshly linked code at an address it chooses,
// within reach of the program's own text. Only the platforms below expose
// that; everywhere else the agent reports why it is inactive and the program
// runs normally.
const (
	protR   = 0
	protRW  = 0
	protRX  = 0
	protRWX = 0
)

var errUnsupported = errors.New("hot reload is not implemented on " + runtime.GOOS)

func pageSize() int { return syscall.Getpagesize() }

func reserve(near, size uintptr) (uintptr, error) { return 0, errUnsupported }

func commit(addr, size uintptr) error { return errUnsupported }

func protect(addr, size uintptr, prot int) error { return errUnsupported }
