# forgo

`forgo` is a fork of Go (currently synced to the 1.26.0 release) that adds
Nim-style compile-time execution and AST macros, plus Rust-style `?`
error-propagation, `throw` for failing a function out with a new error, and
a Ruby/Perl-style postfix `if` for one-line guard clauses.

Using a coding agent on a forgo codebase? Point it at [AGENTS.md](AGENTS.md)
— it tells the agent when to reach for `?`, `throw`, postfix `if`,
`//fgo:comptime`, and `//fgo:macro` instead of plain-Go idioms,
including the rule that a forgo codebase should never hand-write `return
..., err` just to propagate or introduce an error — that's what `?` and
`throw` are for.

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

Once installed, `forgo upgrade` (alias `forgo update`) re-runs the same
install script to pull the latest release over the current installation —
no need to re-fetch and re-run the one-liner above by hand. Pass a version
to install that instead of latest (`forgo upgrade v0.4.0`); it honors the
same `FORGO_INSTALL_DIR`/`FORGO_REPO` env vars as the scripts.

### VS Code extension

Forgo source lives in `.fgo` files (alongside plain `.go` files, which still
compile as before). The [Forgo VS Code extension](editors/vscode) adds `.fgo`
syntax highlighting for `?`, `//fgo:comptime`, and `//fgo:macro`, plus a
language client backed by **forgopls** — a build of
[gopls](https://pkg.go.dev/golang.org/x/tools/gopls) that understands `?`
well enough to type-check it correctly. `//fgo:comptime` and `//fgo:macro`
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
//fgo:comptime
func calculateFactorial(n int) int {
	result := 1
	for i := 1; i <= n; i++ {
		result *= i
	}
	return result
}

//fgo:comptime
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

## What's new

### `//fgo:comptime` functions

A function marked `//fgo:comptime` is ordinary Go — it type-checks and
compiles normally, and can be called at runtime like any other function.
Additionally, when it (or a chain of comptime functions calling each other)
is invoked directly as the initializer of a `const` declaration with
constant-foldable arguments, the compiler evaluates it during type-checking
and folds the result into a real Go constant — so it can be used anywhere
the language requires a constant expression (array lengths, other `const`
declarations, etc.).

Supported inside a comptime function body: `int`/`float`/`string`/`bool`
locals and arithmetic, `if`, 3-clause `for` loops, `+=`/`-=`/... compound
assignment, `++`/`--`, calls to other `//fgo:comptime` functions, and a
small allow-list of real stdlib calls (`fmt.Sprintf`, `strconv.Itoa`, and
the [`comptime/embed`](#comptimeembed--compile-time-file-reads) helpers
below).
Anything else (closures, goroutines, maps, slices, method calls, `range`
loops, ...) is not supported in v1 and reports a compile error if reached.
Building or indexing a composite literal isn't supported inside a comptime
function body either — only directly within a `const` initializer's
expression tree (see "Non-scalar (struct/slice/map) consts" below).

Folding only triggers for `const` initializers, not other
constant-expression contexts written directly as a call (an array length,
a `case` label, etc.) — go through an intermediate `const` if you need
that. Comptime calls also can't cross package boundaries in v1: a function
must be tagged `//fgo:comptime` in the same package where it's folded.

### `//fgo:macro` functions — AST macros

A function marked `//fgo:macro` receives the *unevaluated* syntax tree of
each of its call-site arguments and returns a syntax tree that is spliced
into the caller's code in place of the macro call — before type checking
runs. Macro functions are never type-checked or compiled themselves; they
exist purely at compile time and are removed from the AST once expanded.

```go
//fgo:macro
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
in a package-level `var`/`const` initializer isn't supported in v1. Macros
have no hygiene/renaming and only cover the AST node kinds needed for
straightforward expression/statement templates.

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
- `?` works in an `if`/`for` init clause (including a bare, value-discarding
  `if f()?; cond { ... }`), and in a `for` loop's `Cond`/`Post` clauses. A
  loop using `?` in `Cond`/`Post` is rewritten to an equivalent form with the
  condition checked (and `break` on failure) at the top of the body and the
  post statement moved to the bottom; `continue` targeting that loop
  (bare or labeled) is redirected to run the post statement first, so nested
  loops are unaffected.
- `?` is lowered before type-checking, so it can't verify its assumption
  that `expr` returns `(value, error)` or bare `error` — a mismatched call
  surfaces as an ordinary type-checking error rather than a `?`-specific
  one. It also can't be used directly inside a labeled statement (`L: for
  f()? { }`) if that would require hoisting code before the label — move
  the fallible call above the label instead.

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
  "errors"` (under any name, or `.`) — a package's import graph is computed
  from the literal source text before the compiler runs, so an import added
  only during compilation wouldn't be visible to it. If `errors` isn't
  imported, `throw "..."` is a compile error telling you to add the import;
  `throw errors.New(...)` has no such requirement since you're already
  spelling out the import yourself.
- `throw` is a **contextual** keyword, not a reserved word — plain Go code
  that uses `throw` as an ordinary identifier or function name (like
  `runtime.throw` in the standard library) is completely unaffected.
  `throw` is only read as the statement when it's immediately followed by
  a new operand (another name or a literal, e.g. `throw errors.New(...)`
  or `throw "text"`); `throw(x)`, `throw.field`, `throw = x`, and a bare
  `throw` all still parse as the identifier `throw`.

See [examples/tryop/chain.fgo](examples/tryop/chain.fgo) for a runnable
version using both `throw` forms.

### Postfix `if` — one-line guard clauses

`STMT if COND` is shorthand for `if COND { STMT }`, Ruby/Perl-style — useful
for the kind of one-line guard clause `throw` is often used in:

```go
func check(s string) (n int, err error) {
	throw "empty" if s == ""     // same as: if s == "" { throw "empty" }
	return len(s), nil
}
```

It isn't limited to `throw` — it works as a modifier on any statement kind
that doesn't introduce a new binding into the surrounding scope:

```go
continue if i%2 == 0
total += i if want(i)
return errors.New("bad") if x < 0
```

- Eligible statement kinds: an expression statement, a channel send, a
  plain (non-`:=`) assignment (including `++`/`--`), `return`, `throw`, and
  `break`/`continue`/`goto`.
- `x := f() if cond` is **not** allowed — wrapping a short variable
  declaration in an implicit block would silently shrink its scope to just
  that block, which is exactly the kind of subtle bug postfix `if` should
  never introduce. Use the ordinary block form (`if cond { x := f() }`)
  when you need to declare inside the guard.
- `fallthrough if cond` is not allowed either, since `fallthrough` must
  remain the last statement of its `switch` case, not the body of a
  synthesized `if`.
- Unambiguous by construction: a statement must always be followed by `;`
  (explicit, or automatically inserted at a newline) before the next
  statement can begin, so `if` appearing immediately after a just-completed
  statement on the same line was always a syntax error before — there's no
  existing program this could misparse.

See [examples/tryop/postfixif.fgo](examples/tryop/postfixif.fgo) for a
runnable version.

### `comptime/embed` — compile-time file reads

`comptime/embed` is a small stdlib package for reading files and inspecting
the filesystem at compile time, so a file's contents (or a directory
listing, or an existence check) can be folded into a `const` the same way a
`//fgo:comptime` function's result can:

```go
import "comptime/embed"

const banner = embed.ReadFile("banner.txt")   // read entirely by the compiler
const hasConfig = embed.Exists("config.json")
const assetNames = embed.ReadDir("assets")    // newline-joined entry names
```

It provides `ReadFile`, `ReadFileRange(path, offset, length)` (a "seek" for
pulling a slice out of a file without reading the whole thing), `Exists`,
`IsDir`, `ReadDir`, and `Getwd`. A relative path is resolved against the
directory of the source file containing the call (like Zig's
`@embedFile`), not the compiler's working directory. Every function panics
on error rather than returning one, since a `(string, error)` result can't
be folded into a single compile-time constant — a `comptime.ReadFile` on a
missing file surfaces as a compile error pointing at the `const` line, not
a runtime panic.

Unlike an ordinary `//fgo:comptime` function, these aren't interpreted by
walking their Go source — the compiler recognizes calls to `comptime/embed`
by name and executes them natively, the same way it special-cases
`fmt.Sprintf`/`strconv.Itoa` calls inside comptime function bodies. The
package's Go source is still real, working code that also runs normally
outside of a `const` initializer.

See [examples/embedfile](examples/embedfile/main.fgo) for a runnable
version.

`Load(patterns ...string) FS` goes further and embeds a whole tree of
files into an `FS` value, the way the standard library's `//go:embed`
directive populates an `embed.FS` — but folded through a `const`
initializer instead of a directive:

```go
const content = embed.Load("image", "template", "html/index.html")

data, _ := content.ReadFile("image/hello.jpg")
http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(content))))
```

A pattern naming a directory embeds every file in that directory's
subtree (skipping names beginning with `.` or `_`); a plain path embeds
exactly that one file. `FS` implements `io/fs.FS`, `io/fs.ReadFileFS`,
and `io/fs.ReadDirFS`, so it works anywhere those are accepted — `content`
above is a genuine compile-time constant (see "Non-scalar (struct/slice/map)
consts" below for how a slice-shaped value like `FS`'s file list
materializes), not a value rebuilt from disk at every reference.

See [examples/embedfs](examples/embedfs/main.fgo) for a runnable version
that embeds a small file tree and serves it over HTTP.

### `comptime/json` — compile-time JSON marshal/unmarshal

`comptime/json` marshals and unmarshals JSON at compile time, so a value
built from a struct/slice/map composite literal can be folded into a
`const` string, and (combined with `comptime/embed`) a JSON file's fields
can be folded into `const`s of their own:

```go
import (
	"comptime/embed"
	"comptime/json"
)

type Schema struct {
	Name string
	Port int
}

const cfgJSON = json.Marshal(Schema{Name: "svc", Port: 8080})
const schema = json.Unmarshal[Schema](embed.ReadFile("schema.json"))

// schema is a real compile-time constant of type Schema -- see "Non-scalar
// (struct/slice/map) consts" below -- so schema.Name, schema.Port, etc.
// are themselves constants, usable anywhere Go requires one.
const port = schema.Port
```

`Unmarshal` is generic (`Unmarshal[T any](s string) T`) purely so real Go
type-checking accepts `.Field` on its result with a concrete field to point
at; matching against a struct's fields ignores Go's export-name
capitalization or `json:"..."` struct tags — keep a struct's field names
identical to the JSON keys you read them by (as `Schema` above already
does, since it's also what the real `encoding/json` used at runtime expects
for unadorned exported fields). This isn't available as general local
variables inside a `//fgo:comptime` function body (no closures, generics,
or method calls there either).

See [examples/schemajson](examples/schemajson/main.fgo) for a runnable
version that loads and unmarshals a `schema.json` file entirely at compile
time.

### Non-scalar (struct/slice/map) consts

Ordinary Go restricts `const` to bool/numeric/string values — a struct,
slice, or map can never be a constant, in any Go compiler. forgo changes
that: a struct/map (an ordered field name → value mapping) or a
slice/array (an ordered element list) can be a real constant, alongside
the ordinary bool/string/int/float/complex kinds. In practice, this is what
lets `comptime/json`'s `Unmarshal[T]` fold into a real, named constant
instead of only a one-shot expression:

```go
type Schema struct {
	Name string
	Port int
	Tags []string
}

const schema = json.Unmarshal[Schema](embed.ReadFile("schema.json"))

// schema is a genuine compile-time constant, referenceable anywhere in the
// package, not just inside the expression that produced it.
const name = schema.Name
var proof [schema.Port % 16]byte // usable as an array length, like any const
```

`schema.Field` and `schema.Tags[0]` are themselves constants — so they
compose with everything an ordinary scalar `const` already does: sizing an
array, seeding another `const`, a `case` label, and so on.

**Current limits**, both bounded and enforced with a real compiler
diagnostic rather than a crash:

- A struct-, array-, or slice-shaped composite const (including nested
  combinations, like `embed.FS`'s struct-holding-a-slice-of-structs) can
  be used directly as an ordinary runtime value (`fmt.Println(schema)`,
  `content.ReadFile(...)`, passed to a function, etc.). Every reference to
  the same `const` reads from the same underlying data.
- A composite const with a *map* field still can't be used bare this
  way — only through field/index access down to a scalar or a further
  struct, as in `schema.Tags[0]` or `const name = schema.Name` above.
  Go doesn't statically lay out map data at all, so materializing one as
  a plain value at an arbitrary reference site is unsupported; using one
  where it isn't supported reports a specific compile error rather than
  crashing.

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
new GitHub release. (Intel Mac/darwin-amd64 isn't built; it runs fine under
Rosetta 2 on Apple Silicon, or build from source.) [CI](.github/workflows/ci.yml)
builds and smoke-tests the toolchain on every push/PR (Linux, macOS,
Windows) so a release build is never the first time a change gets built end
to end.

## Building

This repo's root *is* a Go distribution checkout (`src/`, plus `bin/`,
`pkg/`, `lib/`, `api/`, `go.env` needed to bootstrap-build it) — it's what
you get by forking golang/go. Build it like upstream Go:

```bash
cd src
GOROOT_BOOTSTRAP=/path/to/a/go1.24+/install ./make.bash   # Linux/macOS
# or ./make.bat on Windows, ./make.rc on Plan 9
```

This produces `bin/forgo`, the name you actually invoke:

```bash
GOROOT=/path/to/forgo /path/to/forgo/bin/forgo run ./examples/factorial
```
