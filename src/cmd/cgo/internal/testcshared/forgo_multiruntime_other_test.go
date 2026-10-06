// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows

package cshared_test

import "os/exec"

// setNewProcessGroup is only needed on Windows; see the Windows version.
func setNewProcessGroup(cmd *exec.Cmd) {}
