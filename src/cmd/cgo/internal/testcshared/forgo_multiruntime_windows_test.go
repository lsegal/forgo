// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cshared_test

import (
	"os/exec"
	"syscall"
)

// setNewProcessGroup starts cmd in a process group of its own, which is
// where GenerateConsoleCtrlEvent sends a Ctrl+Break the process sends
// itself.
func setNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
