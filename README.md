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

### VS Code extension

Forgo source lives in `.fgo` files (alongside plain `.go` files, which still
compile as before). The [Forgo VS Code extension](editors/vscode) adds `.fgo`
syntax highlighting for `?`, `//fgo:comptime`, and `//fgo:macro`, plus a
language client backed by **forgopls** — [gopls](https://pkg.go.dev/golang.org/x/tools/gopls)
built with the forgo toolchain (see [release.yml](.github/workflows/release.yml)),
so it links against this repo's patched `go/parser`/`go/ast`/`go/types`
(see [go/types/expr.go](src/go/types/expr.go)) instead of vanilla Go's —
enough to type-check `?` correctly. `//fgo:comptime` and `//fgo:macro`
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

### `//fgo:comptime` functions

A function marked `//fgo:comptime` is ordinary Go — it type-checks and
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
assignment, `++`/`--`, calls to other `//fgo:comptime` functions, and a
small allow-list of real stdlib calls (`fmt.Sprintf`, `strconv.Itoa`, and
the [`comptime/embed`](#comptimeembed--compile-time-file-reads) helpers
below).
Anything else (closures, goroutines, maps, slices, method calls, `range`
loops, ...) is not supported in v1 and reports a compile error if reached.

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
- Lowered to a plain `if COND { STMT }` by
  [`cmd/compile/internal/noder/forgo_postfixif.go`](src/cmd/compile/internal/noder/forgo_postfixif.go),
  before `throw`/`?` lowering (so those two passes still see the wrapped
  statement as an ordinary one inside a regular `if`) and before type
  checking.

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
walking their Go source (real file I/O, `os.Stat`, slices, and `panic` are
all outside what [`cmd/compile/internal/forgo`](src/cmd/compile/internal/forgo)'s
tree-walking interpreter can run) — the compiler recognizes calls to
`comptime/embed` by name and executes them natively, the same way it
special-cases `fmt.Sprintf`/`strconv.Itoa` calls inside comptime function
bodies. The package's Go source is still real, working code that also runs
normally outside of a `const` initializer.

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

To evaluate a struct/map/slice composite literal (for `Marshal`) or build
the result of `Unmarshal`, the comptime interpreter represents structs and
maps as an ordered `field name -> value` mapping and slices/arrays as an
element list — see "Non-scalar (struct/slice/map) consts" below for how
that folds into a real `const`. This still isn't available as general
local variables inside a `//fgo:comptime` function body (no closures,
generics, or method calls there either). `Unmarshal` is generic
(`Unmarshal[T any](s string) T`) purely so real Go type-checking accepts
`.Field` on its result with a concrete field to point at; the interpreter
itself ignores the type argument and matches JSON object keys against
field names verbatim, without applying Go's export-name capitalization or
`json:"..."` struct tags — keep a struct's field names identical to the
JSON keys you read them by (as `Schema` above already does, since it's
also what the real `encoding/json` used at runtime expects for unadorned
exported fields).

See [examples/schemajson](examples/schemajson/main.fgo) for a runnable
version that loads and unmarshals a `schema.json` file entirely at compile
time.

### Non-scalar (struct/slice/map) consts

Ordinary Go restricts `const` to bool/numeric/string values — a struct,
slice, or map can never be a constant, in any Go compiler, because
`go/constant.Value` (the type every constant is represented as, all the
way through IR generation and export data) has no case for one. forgo
changes that: `go/constant` gets a new `Composite` kind alongside
Bool/String/Int/Float/Complex, representing a struct/map (an ordered field
name → value mapping) or a slice/array (an ordered element list), and the
compiler's own constant machinery — type-checking, export data encoding,
and static-data code generation — has been taught to carry it through. In
practice, this is what lets `comptime/json`'s `Unmarshal[T]` fold into a
real, named constant instead of only a one-shot expression:

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

`schema.Field` and `schema.Tags[0]` are themselves constants — the field
name/index access is folded by the type checker the moment it sees a
selector or index applied to a Composite-kind constant, before the result
ever reaches IR — so they compose with everything an ordinary scalar
`const` already does: sizing an array, seeding another `const`, a `case`
label, and so on.

**Current limits**, both bounded and enforced with a real compiler
diagnostic rather than a crash:

- A struct-, array-, or slice-shaped composite const (including nested
  combinations, like `embed.FS`'s struct-holding-a-slice-of-structs) can
  be used directly as an ordinary runtime value (`fmt.Println(schema)`,
  `content.ReadFile(...)`, passed to a function, etc.). The compiler
  materializes it once into a read-only static global — a slice field's
  elements go into a freshly allocated backing array, exactly like a
  `[]byte(someStringConst)` conversion's backing data — and every
  reference to that same `const` reads from the same global, memoized
  the first time it's needed (see `CompositeConstExpr` in
  [`staticdata/data.go`](src/cmd/compile/internal/staticdata/data.go)).
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
  `forgo_throw.go`, `forgo_postfixif.go`, `forgo_pragma.go`, `forgo_macro.go`,
  and `cmd/compile/internal/types2/forgo.go` are all new files upstream will
  never touch.
- **Existing files get single-line hooks, not inline logic.** E.g.
  `types2/decl.go`'s `constDecl` gains exactly one `if` calling
  `forgoEvalConstCall` (defined in `forgo.go`); `noder.go`'s pragma switch
  gains one `if forgoPragma(...) { return pragma }` before it, with the
  actual `//fgo:comptime`/`//fgo:macro` handling living in
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
postfix `if` parsing (same five `syntax/` files, plus `noder/forgo_postfixif.go`
for lowering), the `//fgo:` pragma prefix (`syntax/scanner.go`,
`syntax/parser.go`), the `ForgoPragma` interface (`syntax/syntax.go`), and
the hooks described above. Run `git log --oneline` to see exactly which
commits these are.

## Implementation notes / where to look

- [`src/cmd/compile/internal/syntax/scanner.go`](src/cmd/compile/internal/syntax/scanner.go)
  and [`parser.go`](src/cmd/compile/internal/syntax/parser.go): recognize
  `//fgo:` directive comments (previously only `//go:` and `//line` were
  allowed through as compiler directives), and the `?` token.
- [`src/cmd/compile/internal/syntax/syntax.go`](src/cmd/compile/internal/syntax/syntax.go):
  the `ForgoPragma` interface, letting `types2` (which can't import `noder`)
  recognize forgo pragmas on a `FuncDecl` without a shared concrete type.
- [`src/cmd/compile/internal/noder/forgo_pragma.go`](src/cmd/compile/internal/noder/forgo_pragma.go):
  parses `//fgo:comptime` / `//fgo:macro`, called from one line in
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
- [`src/cmd/compile/internal/noder/forgo_postfixif.go`](src/cmd/compile/internal/noder/forgo_postfixif.go):
  lowers `PostfixIfStmt` (`STMT if COND`) into an ordinary `if COND { STMT
  }`, run before `forgo_throw.go`/`forgo_try.go` (the wrapped statement may
  itself be a `throw` or contain `?`) and before type-checking.
- [`src/cmd/compile/internal/forgo`](src/cmd/compile/internal/forgo): the
  interpreter shared by comptime evaluation and macro expansion. Also hosts
  the native dispatch table (`nativeCall`) for `fmt.Sprintf`,
  `strconv.Itoa`, and the `comptime/embed`/`comptime/json` helpers — real
  functions special-cased by name rather than interpreted from source —
  the `objectVal`/`arrayVal` composite-value representation used to
  evaluate a struct/map/slice literal or a `json.Unmarshal` result, and
  `ToConstant`, which converts one into a real `go/constant.Value`.
- [`src/go/constant/value.go`](src/go/constant/value.go): the `Composite`
  `Kind` and `compositeVal` type themselves — a struct/map (an ordered
  field name → value mapping) or slice/array (an ordered element list)
  represented as a real constant, alongside the ordinary
  Bool/String/Int/Float/Complex kinds. See "Non-scalar (struct/slice/map)
  consts" above.
- [`src/internal/pkgbits/encoder.go`](src/internal/pkgbits/encoder.go) and
  [`decoder.go`](src/internal/pkgbits/decoder.go): read/write a
  `Composite` constant into the compiler's unified-IR bitstream (used even
  for a single, non-cross-package build), recursively encoding each
  field/element as its own `Value`.
- [`src/cmd/compile/internal/types2/forgo.go`](src/cmd/compile/internal/types2/forgo.go):
  folds a `const` initializer that's a call (bare, package-qualified like
  `embed.ReadFile(...)`, or generic-instantiated like
  `json.Unmarshal[Config](...)`) or a field/index chain on top of one
  (`json.Unmarshal[Config](...).Name`), called from one line in `decl.go`'s
  `constDecl`; `assignments.go`/`recording.go` accept a Composite-kind
  constant where the type checker otherwise requires an ordinary
  const-eligible (basic) type.
- [`src/cmd/compile/internal/types2/call.go`](src/cmd/compile/internal/types2/call.go)
  (`selector`) and [`index.go`](src/cmd/compile/internal/types2/index.go)
  (`indexExpr`): fold `x.Field`/`x[i]` into a further constant when `x` is
  a Composite-kind constant operand — this is what makes `schema.Port`
  usable anywhere a constant is required, not just inside the expression
  that produced `schema`.
- [`src/cmd/compile/internal/staticdata/data.go`](src/cmd/compile/internal/staticdata/data.go)
  (`InitConst`/`CompositeConstExpr`): writes a struct-, array-, or
  slice-shaped Composite constant into static memory
  field-by-field/element-by-element (a slice field's elements go into a
  freshly allocated backing-array symbol), so a composite const can be
  used directly as an ordinary runtime value, not just folded further.
  `CompositeConstExpr` (called from `walk/expr.go`'s `OLITERAL` case)
  handles a composite const referenced from expression position — a
  function argument, an assignment RHS — by materializing it once into a
  memoized static global and rewriting the reference to an
  `OLINKSYMOFFSET` read out of it.
- [`src/comptime/embed`](src/comptime/embed/embed.go): the `comptime/embed`
  package itself — `ReadFile`, `ReadDir`, `Exists`, `Load`/`FS`, etc.
- [`src/comptime/json`](src/comptime/json/json.go): the `comptime/json`
  package itself — `Marshal` and generic `Unmarshal[T]`.
- [`src/cmd/compile/internal/syntax/nodes.go`](src/cmd/compile/internal/syntax/nodes.go),
  [`tokens.go`](src/cmd/compile/internal/syntax/tokens.go): the `?` token
  and `*syntax.TryExpr` node, parsed as a postfix operator alongside
  `.`/`(...)`.

## Known limitations (v1)

- Comptime folding only triggers for `const` initializers, not general
  constant-expression contexts (array lengths written directly as a call,
  `case` labels of a call, etc.) — go through an intermediate `const` if
  you need that.
- A `//fgo:comptime` function body supports a deliberately small subset
  of Go (see above); no closures, generics, maps/slices, or method calls.
  Building a composite literal, or indexing into one, isn't supported
  inside an ordinary comptime function body — only directly within a
  `const` initializer's expression tree (see "Non-scalar
  (struct/slice/map) consts" above for what that constant can then be
  used for elsewhere).
- `comptime/embed` and `comptime/json`'s helpers sidestep the small-subset
  limitation above by being executed natively rather than interpreted (see
  above), which is also why they're a fixed, hardcoded set rather than
  something you can add to yourself by tagging an arbitrary function
  `//fgo:comptime` in another package — cross-package comptime calls
  aren't supported in v1.
- A struct literal's keys and a map literal's keys are told apart by a
  heuristic (an unbound identifier is a field name, anything else is
  evaluated as an expression), since the interpreter has no real type
  information — see `compositeLitKey` in
  [`interp.go`](src/cmd/compile/internal/forgo/interp.go).
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
