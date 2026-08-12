# Forgo for VS Code

Language support for [forgo](https://github.com/lsegal/forgo) `.fgo` files:
syntax highlighting for `?`, `throw`, `//fgo:comptime`, and `//fgo:macro`
on top of standard Go syntax, plus a language client backed by
**forgopls** — [gopls](https://pkg.go.dev/golang.org/x/tools/gopls) built
with the forgo toolchain (see [scripts/build-forgopls.sh](../../scripts/build-forgopls.sh)),
so it understands `?` and `throw` — parses, prints, and type-checks them —
instead of just tolerating or choking on them.

## Install

1. Install forgo itself (see the [main README](../../README.md#installing-a-prebuilt-release))
   — its release tarball includes `forgopls` alongside `forgo` in `bin/`, and
   the install script already puts that on `PATH`.
2. Download the extension's `.vsix` from the
   [latest release](https://github.com/lsegal/forgo/releases), then either:

   ```bash
   code --install-extension forgo-<version>.vsix
   ```

   or in VS Code: Extensions view → `...` menu → **Install from VSIX...**.

## Settings

- `forgo.enableLanguageServer` (default `true`) — start a language server for `.fgo` files.
- `forgo.languageServerPath` (default `forgopls`) — path to the `forgopls` binary.
  If it can't be started, the extension falls back to a plain `gopls` on
  `PATH`, which will misreport `?`/`throw` (it wasn't built against forgo's
  `go/parser`/`go/types`, and doesn't have the `golang.org/x/tools` patches
  described in [build-forgopls.sh](../../scripts/build-forgopls.sh)), not
  just `//fgo:comptime`/`//fgo:macro`. `//fgo:comptime` and
  `//fgo:macro` are compiler-only either way and will still show
  false-positive diagnostics from either binary; disable
  `forgo.enableLanguageServer` if that's noisier than it's worth.
- `forgo.languageServerEnv` (default `{}`) — extra environment variables for
  the forgopls/gopls process, merged over VS Code's own environment.
  forgopls resolves packages by shelling out to `go list`/`go build` using
  its process environment, so if `GOROOT`/`PATH` there don't already point
  at a forgo toolchain (e.g. VS Code wasn't launched from a shell with them
  exported), set them here, e.g.
  `{"GOROOT": "/path/to/forgo", "GOTOOLCHAIN": "local", "PATH": "/path/to/forgo/bin:${env:PATH}"}`.

## Building from source

```bash
cd editors/vscode
npm install
npx @vscode/vsce package
```

To build `forgopls` itself (see [scripts/build-forgopls.sh](../../scripts/build-forgopls.sh)
for what it does and why, and [release.yml](../../.github/workflows/release.yml)
for the exact `gopls` version pinned):

```bash
export GOROOT=/path/to/forgo
export GOTOOLCHAIN=local
scripts/build-forgopls.sh v0.23.0 "$GOROOT/bin/forgopls"
```
