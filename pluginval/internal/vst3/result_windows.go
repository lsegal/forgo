// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

// On Windows VST3 is COM compatible: its results are HRESULTs and its
// TUIDs are GUIDs.
const comCompatible = true

const (
	ResultOK        Result = 0
	ResultTrue      Result = 0
	ResultFalse     Result = 1
	NoInterface     Result = -0x7FFFBFFE // 0x80004002
	InvalidArgument Result = -0x7FF8FFA9 // 0x80070057
	NotImplemented  Result = -0x7FFFBFFF // 0x80004001
	InternalError   Result = -0x7FFFBFFB // 0x80004005
	NotInitialized  Result = -0x7FFF0001 // 0x8000FFFF
)
