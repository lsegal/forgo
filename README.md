# forgo

`forgo` is a fork of Go (currently synced to the 1.26.0 release) that adds
Nim-style compile-time execution and AST macros, plus Rust-style `?`
error-propagation and `throw` for failing a function out with a new error.

Using a coding agent on a forgo codebase? Point it at [AGENTS.md](AGENTS.md)
— it tells the agent when to reach for `?`, `throw`, `//forgo:comptime`, and
`//forgo:macro` instead of plain-Go idioms, including the rule that a
forgo codebase should never hand-write `return ..., err` just to propagate
or introduce an error — that's what `?` and `throw` are for.

## Installing a prebuilt release

```bash
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/lsegal/forgo/release-branch.go1.26/install/install.sh | sh
```

```powershell
# Windows
irm https://raw.githubusercontent.com/lsegal/forgo/release-branch.go1.26/install/install.ps1 | iex
```

Both scripts install the latest [GitHub release](https://github.com/lsegal/forgo/releases)
to `~/.forgo` by default. Pass a specific version (`sh install.sh v0.2.0`, or
`-Version v0.2.0` on Windows) to install that release instead of latest; set
`FORGO_INSTALL_DIR`/`FORGO_REPO` env vars to change the install location or
fork. After installing, point `GOROOT` at the install directory and add its
`bin/` to `PATH` (the script prints the exact commands), then run `forgo`
(e.g. `forgo build`, `forgo run`). The release tarball also includes
`forgopls` in `bin/` — see the VS Code extension below.

### VS Code extension

Forgo source lives in `.fgo` files (alongside plain `.go` files, which still
compile as before). The [Forgo VS Code extension](editors/vscode) adds `.fgo`
syntax highlighting for `?`, `//forgo:comptime`, and `//forgo:macro`, plus a
language client backed by **forgopls** — [gopls](https://pkg.go.dev/golang.org/x/tools/gopls)
built with the forgo toolchain (see [release.yml](.github/workflows/release.yml)),
so it links against this repo's patched `go/parser`/`go/ast`/`go/types`
(see [go/types/expr.go](src/go/types/expr.go)) instead of vanilla Go's —
enough to type-check `?` correctly. `//forgo:comptime` and `//forgo:macro`
are still compiler-only and will show as false-positive diagnostics.
`forgopls` ships in the toolchain install above; download the extension's
`.vsix` from the [latest release](https://github.com/lsegal/forgo/releases)
and install it with:

```bash
code --install-extension forgo-<version>.vsix
```

or in VS Code: Extensions view → `...` menu → **Install from VSIX...**. See
[editors/vscode/README.md](editors/vscode/README.md) for settings and
building from source.

```go
//forgo:comptime
func calculateFactorial(n int) int {
	result := 1
	for i := 1; i <= n; i++ {
		result *= i
	}
	return result
}

//forgo:comptime
func factorialMessage(n int) string {
	return fmt.Sprintf("Factorial of %d is %d", n, calculateFactorial(n))
}

// Evaluated entirely by the compiler.
const factFive = calculateFactorial(5)
const msg = factorialMessage(5)
```

See [examples/factorial](examples/factorial/main.fgo) for a runnable version,
including proof that `factFive` is a real constant (it sizes an array).

Files using forgo-specific syntax use the `.fgo` extension instead of `.go`
— the toolchain treats the two identically everywhere (`forgo build`,
`forgo run`, `forgo test`, module resolution), so a package can freely mix
both. Files that are plain Go keep the `.go` extension.

## This is a real fork, not a copy

This repository is a git fork of [golang/go](https://github.com/golang/go)
— same history, same tags, `upstream` remote pointed at the real thing —
not a one-time export of the source. It's grafted onto the `go1.26.0` tag,
which lives on golang/go's `release-branch.go1.26` (not `master`, where
golang/go's ongoing development happens), so this repo's default branch is
named `release-branch.go1.26` to match. The [Upstream Sync
workflow](.github/workflows/upstream-sync.yml) runs daily, merges
`upstream/release-branch.go1.26` into this repo's default branch, and opens
a PR (which must pass [CI](.github/workflows/ci.yml) and auto-merges if it
does, or sits for manual conflict resolution if it doesn't). Every
forgo-specific change is designed to keep that merge boring — see "Least
invasive by design" below.

## What's new

### `//forgo:comptime` functions

A function marked `//forgo:comptime` is ordinary Go — it type-checks and
compiles normally, and can be called at runtime like any other function.
Additionally, when it (or a chain of comptime functions calling each other)
is invoked directly as the initializer of a `const` declaration with
constant-foldable arguments, the compiler evaluates it during type-checking
via a small tree-walking interpreter
([`cmd/compile/internal/forgo`](src/cmd/compile/internal/forgo)) and folds
the result into a real Go constant — so it can be used anywhere the
language requires a constant expression (array lengths, other `const`
declarations, etc.).

Supported inside a comptime function body: `int`/`float`/`string`/`bool`
locals and arithmetic, `if`, 3-clause `for` loops, `+=`/`-=`/... compound
assignment, `++`/`--`, calls to other `//forgo:comptime` functions, and a
small allow-list of real stdlib calls (`fmt.Sprintf`, `strconv.Itoa`).
Anything else (closures, goroutines, maps, slices, method calls, `range`
loops, ...) is not supported in v1 and reports a compile error if reached.

### `//forgo:macro` functions — AST macros

A function marked `//forgo:macro` receives the *unevaluated* syntax tree of
each of its call-site arguments and returns a syntax tree that is spliced
into the caller's code in place of the macro call — before type checking
runs. Macro functions are never type-checked or compiled themselves; they
exist purely at compile time and are removed from the AST once expanded.

```go
//forgo:macro
func double(x Node) Node {
	return Quote(func() {
		Splice(x) + Splice(x)
	})
}

double(compute()) // expands to: compute() + compute()
```

- `Quote(func(){ ... })` captures the function literal's body as an AST
  template without evaluating it. A single-expression template unwraps so
  the macro can be used in expression position; anything else is treated as
  a statement-position macro.
- `Splice(x)` marks a point in a quoted template where the tree bound to
  local variable `x` (a macro parameter, or a value derived from one) is
  substituted in.
- Macro parameter/return types (`Node` above) are placeholders — v1 does not
  define a real `Node` type, since macro signatures are never type-checked.

Macro call recognition is purely syntactic (unqualified function-name
match), and expansion happens once per call site, recursively into the
result. Macros are expanded within function bodies; using a macro directly
in a package-level `var`/`const` initializer isn't supported in v1.

### `?` — Rust-style error propagation

`expr?` evaluates `expr`, and if it produced a non-nil error, returns
immediately from the enclosing function with that error; otherwise it
evaluates to the non-error value, so it chains:

```go
func loadConfig(path string) (name string, err error) {
	f := open(path)?      // returns early if open fails
	cfg := parse(f)?.normalize()?
	name = cfg.Name
	return
}
```

- `?` only works inside a function (or func literal) whose **last result is
  a named `error`**, e.g. `func f() (T, err error)` or `func g() (err
  error)`. On error it assigns to that named result and does a naked
  `return`, relying on Go's automatic zero-initialization of the other named
  results.
- `expr?` used where a value is expected (`x := f()?`, `f()?.g()`, `if f()?
  { ... }`, ...) assumes `expr` returns exactly `(value, error)`.
- `expr?` used as a whole statement (`f()?` alone on a line) assumes `expr`
  returns only `error`. To discard a value explicitly instead, write `_ =
  f()?`.
- Chaining works because `?` is parsed as a postfix operator at the same
  precedence as `.`/`(...)`, so `foo()?.bar()?` parses as `(foo()?).bar()?`:
  the first `?` unwraps to `foo`'s value, `.bar()` is then called on it, and
  the second `?` unwraps that call's result.
- `?` is lowered to plain `:=`/`if`/`return` statements by
  [`cmd/compile/internal/noder/forgo_try.go`](src/cmd/compile/internal/noder/forgo_try.go)
  before type checking, so the rest of the compiler never sees it.
- `?` works in an `if`/`for` init clause (including a bare, value-discarding
  `if f()?; cond { ... }`), and in a `for` loop's `Cond`/`Post` clauses. A
  loop using `?` in `Cond`/`Post` is rewritten to an equivalent form with the
  condition checked (and `break` on failure) at the top of the body and the
  post statement moved to the bottom; `continue` targeting that loop
  (bare or labeled) is redirected to run the post statement first, using the
  parser's own branch resolution (`syntax.BranchStmt.Target`) so nested
  loops are unaffected.

See [examples/tryop](examples/tryop/main.fgo) for a runnable version,
including the literal `foo()?.bar()?` chained form, and
[examples/tryop/loops.fgo](examples/tryop/loops.fgo) for `?` in loop headers,
`continue`/`break`, and labeled loops.

### `throw` — fail a function out with a new error

`throw EXPR` returns immediately from the enclosing function, passing
`EXPR` through as the last (`error`) result and a zero value for every
other result — it's shorthand for the `return nil, ..., err`-shaped guard
clause you'd otherwise write by hand:

```go
func makeThing(s string) (*Thing, error) {
	if s == "" {
		throw errors.New("empty")     // same as: return nil, errors.New("empty")
	}
	return &Thing{name: s}, nil
}
```

`throw "some text"` (a bare string literal) is sugar for `throw
errors.New("some text")` — the two forms are exactly equivalent:

```go
func makeThing(s string) (*Thing, error) {
	if s == "" {
		throw "empty"                 // same as: throw errors.New("empty")
	}
	return &Thing{name: s}, nil
}
```

- `throw` works in any function (or func literal) whose **last result's
  type is spelled `error`** — unlike `?`, it doesn't need that result to
  be *named*, since it builds the `return`'s value list directly instead
  of relying on Go's naked-return zero-initialization.
- Every result before the last needs a zero value the compiler can work
  out from its syntax alone: pointer, slice, map, chan, func, interface
  (including `any`/`error`), or a basic type. A named struct, array, or
  other defined type isn't nil-able and can't be zeroed without a type
  checker, so `throw` there is a compile error — `no default value for
  <result>` — naming exactly which result couldn't be zeroed; fall back to
  a manual `return` for that function.
- `throw "literal text"` requires the file to already have `import
  "errors"` (under any name, or `.`) — `go build` computes a package's
  import graph from the literal source text before the compiler runs, so
  an import added only inside the compiler's own lowering pass wouldn't be
  visible to it. If `errors` isn't imported, `throw "..."` is a compile
  error telling you to add the import; `throw errors.New(...)` has no such
  requirement since you're already spelling out the import yourself.
- `throw` is a **contextual** keyword, not a reserved word — plain Go code
  that uses `throw` as an ordinary identifier or function name (like
  `runtime.throw` in the standard library) is completely unaffected.
  `throw` is only read as the statement when it's immediately followed by
  a new operand (another name or a literal, e.g. `throw errors.New(...)`
  or `throw "text"`); `throw(x)`, `throw.field`, `throw = x`, and a bare
  `throw` all still parse as the identifier `throw`.
- `throw` is lowered to a plain `return` by
  [`cmd/compile/internal/noder/forgo_throw.go`](src/cmd/compile/internal/noder/forgo_throw.go)
  before type checking, so the rest of the compiler never sees it — same
  strategy as `?`.

See [examples/tryop/chain.fgo](examples/tryop/chain.fgo) for a runnable
version using both `throw` forms.

## Versioning

forgo has its own version, independent of the golang/go release it's synced
against — tracked in [`FORGO_VERSION`](FORGO_VERSION) at the repo root and
tagged as `vX.Y.Z`. Bump it locally with:

```bash
./scripts/version.sh show    # print the current version
./scripts/version.sh patch   # or minor / major — bumps and writes FORGO_VERSION
```

In practice this is normally driven by the [Release workflow](.github/workflows/release.yml)
(Actions → Release → Run workflow → pick `patch`/`minor`/`major`), which
bumps `FORGO_VERSION`, commits and tags it, builds the toolchain for
linux/amd64, darwin/arm64, and windows/amd64, and publishes them all to a
new GitHub release. (Intel Mac/darwin-amd64 isn't built — see the comment in
release.yml; it runs fine under Rosetta 2 on Apple Silicon, or build from
source.) [CI](.github/workflows/ci.yml) builds and smoke-tests the
toolchain on every push/PR (Linux, macOS, Windows) so a release build is
never the first time a change gets built end to end.

## Building

This repo's root *is* a Go distribution checkout (`src/`, plus `bin/`,
`pkg/`, `lib/`, `api/`, `go.env` needed to bootstrap-build it) — it's what
you get by forking golang/go. Build it like upstream Go:

```bash
cd src
GOROOT_BOOTSTRAP=/path/to/a/go1.24+/install ./make.bash   # Linux/macOS
# or ./make.bat on Windows, ./make.rc on Plan 9
```

This produces `bin/go` and `pkg/tool/<os>_<arch>/compile` (etc.) — unchanged
from upstream. forgo doesn't rename `cmd/go` internally (see "Least invasive
by design" below); instead, copy `bin/go` to `bin/forgo` as the name you
actually invoke:

```bash
cp bin/go bin/forgo   # bin/go.exe -> bin/forgo.exe on Windows
GOROOT=/path/to/forgo /path/to/forgo/bin/forgo run ./examples/factorial
```

(CI and the release workflow do this same copy step; prebuilt releases
already include `bin/forgo`.)

## Least invasive by design

Because this is a real fork that merges from upstream daily, every
forgo-specific change is deliberately structured to touch as little of the
existing Go source as possible, so those merges stay conflict-free:

- **New files carry the logic.** [`cmd/compile/internal/forgo`](src/cmd/compile/internal/forgo)
  (the comptime/macro interpreter), `cmd/compile/internal/noder/forgo_try.go`,
  `forgo_throw.go`, `forgo_pragma.go`, `forgo_macro.go`, and
  `cmd/compile/internal/types2/forgo.go` are all new files upstream will
  never touch.
- **Existing files get single-line hooks, not inline logic.** E.g.
  `types2/decl.go`'s `constDecl` gains exactly one `if` calling
  `forgoEvalConstCall` (defined in `forgo.go`); `noder.go`'s pragma switch
  gains one `if forgoPragma(...) { return pragma }` before it, with the
  actual `//forgo:comptime`/`//forgo:macro` handling living in
  `forgo_pragma.go`.
- **No new fields on hot structs.** `types2.Checker` (a large, frequently
  touched struct) carries zero forgo-specific fields — the comptime
  interpreter is cached in a package-level table keyed by `*Checker`
  instead, so `check.go` has a zero-line diff from upstream.
- **`cmd/go` is untouched.** Renaming the actual `go` binary to `forgo`
  would mean renaming/patching `cmd/go` — a huge, constantly-changing
  package with internal self-references to its own binary name — which is
  exactly the kind of change that fights every future upstream merge. So
  the build stays 100% stock, and `forgo` ships as a copy of the real `go`
  binary under a different name (see "Building" above). One consequence:
  `forgo version` still prints `go version ...` — `FORGO_VERSION` (via
  `./scripts/version.sh`) is the source of truth for forgo's own version,
  not `forgo version`'s output.

The full forgo-specific diff against upstream is genuinely small: the `?`
token/parsing (`syntax/{tokens,scanner,parser,nodes,printer}.go`), the
`throw` statement parsing (`syntax/{nodes,parser,positions,printer,walk}.go`),
the `//forgo:` pragma prefix (`syntax/scanner.go`, `syntax/parser.go`), the
`ForgoPragma` interface (`syntax/syntax.go`), and the hooks described above.
Run `git log --oneline` to see exactly which commits these are.

## Implementation notes / where to look

- [`src/cmd/compile/internal/syntax/scanner.go`](src/cmd/compile/internal/syntax/scanner.go)
  and [`parser.go`](src/cmd/compile/internal/syntax/parser.go): recognize
  `//forgo:` directive comments (previously only `//go:` and `//line` were
  allowed through as compiler directives), and the `?` token.
- [`src/cmd/compile/internal/syntax/syntax.go`](src/cmd/compile/internal/syntax/syntax.go):
  the `ForgoPragma` interface, letting `types2` (which can't import `noder`)
  recognize forgo pragmas on a `FuncDecl` without a shared concrete type.
- [`src/cmd/compile/internal/noder/forgo_pragma.go`](src/cmd/compile/internal/noder/forgo_pragma.go):
  parses `//forgo:comptime` / `//forgo:macro`, called from one line in
  `noder.go`.
- [`src/cmd/compile/internal/noder/forgo_macro.go`](src/cmd/compile/internal/noder/forgo_macro.go):
  the macro expansion pass, run after parsing and before type-checking.
- [`src/cmd/compile/internal/noder/forgo_try.go`](src/cmd/compile/internal/noder/forgo_try.go):
  lowers `TryExpr` (the `?` operator) into ordinary `:=`/`if`/`return`
  statements, also run after parsing and before type-checking.
- [`src/cmd/compile/internal/noder/forgo_throw.go`](src/cmd/compile/internal/noder/forgo_throw.go):
  lowers `ThrowStmt` (the `throw` statement) into an ordinary `return`,
  run before `forgo_try.go` (a thrown expression may itself contain `?`)
  and before type-checking.
- [`src/cmd/compile/internal/forgo`](src/cmd/compile/internal/forgo): the
  interpreter shared by comptime evaluation and macro expansion.
- [`src/cmd/compile/internal/types2/forgo.go`](src/cmd/compile/internal/types2/forgo.go):
  folds `//forgo:comptime` calls in `const` initializers, called from one
  line in `decl.go`'s `constDecl`.
- [`src/cmd/compile/internal/syntax/nodes.go`](src/cmd/compile/internal/syntax/nodes.go),
  [`tokens.go`](src/cmd/compile/internal/syntax/tokens.go): the `?` token
  and `*syntax.TryExpr` node, parsed as a postfix operator alongside
  `.`/`(...)`.

## Known limitations (v1)

- Comptime folding only triggers for `const` initializers, not general
  constant-expression contexts (array lengths written directly as a call,
  `case` labels of a call, etc.) — go through an intermediate `const` if
  you need that.
- The comptime interpreter supports a deliberately small subset of Go
  (see above); no closures, generics, maps/slices, or method calls.
- Macros have no hygiene/renaming and only cover the AST node kinds needed
  for straightforward expression/statement templates.
- Macro compile errors are reported without a precise source position.
- `?` assumes the expression it's applied to returns `(value, error)` in
  value position or bare `error` in statement position — it can't check
  this itself (lowering runs before type-checking), so a mismatched call
  surfaces as an ordinary "assignment mismatch" error instead of a `?`-
  specific one.
- `?` can't be used directly inside a labeled statement (`L: for f()? { }`)
  if that would require hoisting statements before the label — move the
  fallible call above the label instead. `?` also isn't lowered inside a
  `for` loop's `Cond`/`Post` when written as (or containing) a channel send
  (`for ...; ...; ch <- f()? { }`) — an obscure combination not handled in
  v1.
- `forgo version` prints Go's own version string, not `FORGO_VERSION` (see
  "Least invasive by design" above).
