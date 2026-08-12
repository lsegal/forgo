# Forgo for VS Code

Language support for [forgo](https://github.com/lsegal/forgo) `.fgo` files:
syntax highlighting for `?`, `//forgo:comptime`, and `//forgo:macro` on top
of standard Go syntax, plus an optional `gopls`-backed language client for
the features `gopls` can still provide on forgo source.

## Install

Download the `.vsix` from the [latest release](https://github.com/lsegal/forgo/releases),
then either:

```bash
code --install-extension forgo-<version>.vsix
```

or in VS Code: Extensions view → `...` menu → **Install from VSIX...**.

## Settings

- `forgo.enableLanguageServer` (default `true`) — start a language server for `.fgo` files.
- `forgo.languageServerPath` (default `gopls`) — path to the `gopls` binary.
  `gopls` doesn't understand forgo-specific syntax, so expect false-positive
  diagnostics on `?`, `//forgo:comptime`, and `//forgo:macro` until forgo
  has its own language server; disable `forgo.enableLanguageServer` if
  that's noisier than it's worth.

## Building from source

```bash
cd editors/vscode
npm install
npx @vscode/vsce package
```
