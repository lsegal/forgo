// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package upgrade implements the "forgo upgrade" (and "forgo update" alias)
// commands.
package upgrade

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"cmd/go/internal/base"
	"cmd/go/internal/web"
)

// defaultRepo and defaultBranch mirror the URLs documented in README.md
// and printed by 'forgo version' -- the same install scripts a user would
// otherwise fetch by hand via curl/irm.
const (
	defaultRepo   = "lsegal/forgo"
	defaultBranch = "release-branch.go1.27"
)

var CmdUpgrade = &base.Command{
	UsageLine: "forgo upgrade [version]",
	Short:     "upgrade forgo by re-running the platform install script",
	Long: `Upgrade fetches and runs forgo's platform install script -- the same
install.sh/install.ps1 documented in README.md and printed by the
installer -- to install a forgo release over the current installation.

With no argument, it installs the latest release. An explicit argument
installs that version instead:

	forgo upgrade v0.4.0

It honors the same environment variables as the install scripts:

	FORGO_REPO         "owner/repo" to install from (default: lsegal/forgo)
	FORGO_INSTALL_DIR  install directory (default: $HOME/.forgo, or
	                    %USERPROFILE%\.forgo on Windows)

'forgo update' is an alias for this command.
`,
}

var CmdUpdate = &base.Command{
	UsageLine: "forgo update [version]",
	Short:     "alias for 'forgo upgrade'",
	Long:      "Update is an alias for 'forgo upgrade'; see 'forgo help upgrade'.",
}

func init() {
	CmdUpgrade.Run = runUpgrade
	CmdUpdate.Run = runUpgrade
}

func runUpgrade(ctx context.Context, cmd *base.Command, args []string) {
	if len(args) > 1 {
		fmt.Fprintf(os.Stderr, "usage: %s\n", cmd.UsageLine)
		base.SetExitStatus(2)
		return
	}
	version := "latest"
	if len(args) == 1 {
		version = args[0]
	}

	repo := os.Getenv("FORGO_REPO")
	if repo == "" {
		repo = defaultRepo
	}

	scriptName := "install.sh"
	if runtime.GOOS == "windows" {
		scriptName = "install.ps1"
	}
	scriptURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/install/%s", repo, defaultBranch, scriptName)

	fmt.Printf("forgo upgrade: fetching %s\n", scriptURL)
	script, err := fetchScript(scriptURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forgo upgrade: %v\n", err)
		base.SetExitStatus(1)
		return
	}

	tmp, err := os.CreateTemp("", "forgo-upgrade-*"+filepath.Ext(scriptName))
	if err != nil {
		fmt.Fprintf(os.Stderr, "forgo upgrade: %v\n", err)
		base.SetExitStatus(1)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	_, writeErr := tmp.Write(script)
	closeErr := tmp.Close()
	if writeErr != nil {
		fmt.Fprintf(os.Stderr, "forgo upgrade: %v\n", writeErr)
		base.SetExitStatus(1)
		return
	}
	if closeErr != nil {
		fmt.Fprintf(os.Stderr, "forgo upgrade: %v\n", closeErr)
		base.SetExitStatus(1)
		return
	}

	var run *exec.Cmd
	if runtime.GOOS == "windows" {
		run = exec.CommandContext(ctx, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", tmpName, "-Version", version)
	} else {
		run = exec.CommandContext(ctx, "sh", tmpName, version)
	}
	run.Stdin = os.Stdin
	run.Stdout = os.Stdout
	run.Stderr = os.Stderr

	if err := run.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "forgo upgrade: install script failed: %v\n", err)
		base.SetExitStatus(1)
	}
}

func fetchScript(rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	return web.GetBytes(u)
}
