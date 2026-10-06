// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows

package vst3

const comCompatible = false

const (
	ResultOK        Result = 0
	ResultTrue      Result = 0
	ResultFalse     Result = 1
	NoInterface     Result = -1
	InvalidArgument Result = 2
	NotImplemented  Result = 3
	InternalError   Result = 4
	NotInitialized  Result = 5
)
