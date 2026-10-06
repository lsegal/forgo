# forgo

`forgo` is a fork of Go (currently synced to the 1.27 release branch) that adds
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
curl -fsSL https://github.com/lsegal/forgo/releases/latest/download/install.sh | sh
```

```powershell
# Windows
irm https://github.com/lsegal/forgo/releases/latest/download/install.ps1 | iex
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

### `forgo run --watch` — hot reload

```bash
forgo run --watch ./examples/hotreload
```

Edit a function, save, and the running program picks up the new code
without restarting. Its goroutines keep running, its heap is untouched, and
every package-level variable keeps the value it had — counters keep
counting, caches stay warm, open files and connections stay open. This is
the same thing Dart and Flutter do, done for an ahead-of-time compiled
language.

```
$ forgo run --watch ./examples/hotreload
pid 44081 — edit render() in examples/hotreload/main.fgo and save
tick 1
tick 2
...                                    # edit render(), save
forgo: reloaded 1 function in 11.9s
GEN1 tick 104 (worker beat 51, 104 in history, up 51s)
GEN1 tick 105
```

Note the tick count and the uptime: same process, new code.

#### How it works

A Go binary is fully linked ahead of time, so "inject new code" has to mean
something concrete. It means this:

1. The first build is an ordinary one, except the linker also writes down a
   **record** of everything it linked: for each symbol, its address in the
   binary and a digest of its contents together with the *names* its
   relocations point at.
2. The program starts with a small agent (`runtime/fgohot`) linked in. The
   agent reserves half a gigabyte of address space within reach of the
   program's own code — close enough that ordinary PC-relative references
   from new code can still reach pinned code and data — and tells the watcher
   where it is.
3. On save, the whole program is linked again, against that record. Any
   symbol whose digest is unchanged is **pinned**: it keeps the address it
   already occupies in the running process, and is left out of the output
   entirely. Only what changed, plus anything genuinely new, is laid out —
   into the reserved region.

   This is the step that preserves state. A changed function's references
   to package-level variables, to the runtime, to type descriptors and
   itabs are all resolved by the linker to the addresses those things
   already live at. Nothing is copied, migrated, or re-initialized, because
   nothing moved.
4. The agent maps the resulting image, registers its `moduledata` with the
   runtime — so the garbage collector, the stack unwinder, profiles and
   panics all understand the new code — runs the initializers of packages
   that are new, and finally overwrites each changed function's aligned entry
   slot with a full-address indirect jump to its new body, with the world
   stopped. On amd64 that is a register-preserving 14-byte trampoline, so
   entry-point redirection is not limited to the `JMP rel32` ±2GB range.

A function already executing finishes its current call in the old code; the
next call runs the new code. Dart's hot reload makes the same bargain.

This has a sharp edge worth calling out explicitly: **a change inside a
function that is already running and never returns doesn't take effect,**
because the patch only redirects future calls, and there is no future call
— the function is still inside the one call it's already in. The most
common way to hit this is editing code written directly in `main()`'s own
loop body (as opposed to a function `main()` *calls* each iteration, like
`render()` in the example below) — `main()` never returns to be re-entered,
so a patch to its entry point never matters for the activation already
running. Move logic you want to be reloadable into an ordinary function
that gets called repeatedly, and edit that instead. (Flutter's hot reload
has the identical rule for a `State.build()` that's already mid-execution;
this isn't a corner cut, it's inherent to reloading via call redirection
rather than modifying a suspended stack frame in place.)

#### What requires a restart

Some changes cannot be applied to a process that is already running, and
forgo refuses them rather than corrupting it. The program keeps running the
old code and the watcher says why:

```
forgo: cannot reload without restarting — the type main.Point changed shape
```

- **A struct's layout changed.** Objects already on the heap were laid out
  by the old descriptor, and code that is not being replaced still uses it.
- **A function's signature changed.** Callers that are not being replaced
  would still pass arguments the old way.
- **A package's initialization changed.** Package `init` runs once per
  process; re-running it would trample the state it already built, and
  skipping it would leave the new code without state it expects.

#### Limits

- linux/amd64, windows/amd64, and darwin/arm64 (Apple Silicon) work end to
  end. darwin/amd64 (Intel Mac, or Rosetta on Apple Silicon) shares the same
  non-PIE, RWX-capable model as Linux and Windows and should work the same
  way, but hasn't been run end to end on real darwin/amd64 hardware.
- Apple Silicon enforces W^X, so a live text page can never be writable and
  executable at the same time. The agent copies each affected 16KB page to
  private writable memory, writes the entry-point jumps into the copy, seals
  it executable, then atomically overlays the original mapping with
  `mach_vm_remap` while the world is stopped. The original page remains
  executable until the replacement is installed; there is no RWX or
  non-executable window.
- Watch mode currently forces the Go internal linker. Ordinary cgo packages
  supported by that linker hot-reload, including edits to C function bodies.
  On Darwin the agent resolves each mapped Mach-O image's `__got` and lazy
  symbol pointers with `dlopen`/`dlsym`, since dyld never sees these manually
  mapped images. Packages that specifically require external linking — for
  example C++ or unsupported static libraries — still require a restart.
- On Windows, the watched build always uses `-buildmode=exe`, overriding
  the toolchain's PIE-by-default on windows/amd64. PIE's load address is
  chosen by the OS at every launch, which would make the addresses a hot
  link records meaningless from one run to the next; `-buildmode=exe`
  keeps the fixed load address every other platform already gets by
  default, so a hot link's `-T` reliably lands where the running
  program's pinned symbols actually are. The linker refuses to hot-link a
  PIE binary outright rather than silently producing bad relocations.
- Windows images are never handed to the OS's PE loader — the agent maps
  them itself — so it also performs the one step that requires: resolving
  the image's own Import Address Table (the DLL functions the runtime
  depends on, e.g. `kernel32.dll`) by walking the PE import directory and
  calling `GetProcAddress` directly, the way the OS loader would.
- The watched build compiles with inlining disabled (`-gcflags=all=-l`), so
  that changing a function cannot leave stale copies of its body inside
  callers that are not being replaced. Watch mode is therefore slower than
  a normal build — it is a development mode, not a production one.
- Each reload consumes a slice of the reserved region; after a few hundred
  reloads the program must be restarted. It says so when that happens.
- A reload links the whole program, so it costs about as long as a link,
  not as long as a build from scratch — under a second to a couple of
  seconds for a small program on a native filesystem. (If you're
  developing forgo itself from WSL with `GOROOT` on a Windows-mounted
  drive, expect that link step to be much slower — the 9p filesystem
  bridge WSL uses for `/mnt/*` paths adds real overhead to the many small
  file reads a link does; this doesn't apply to programs whose `GOROOT`
  and module live entirely on a native filesystem, WSL's own or Windows'.)
- `--watch` runs the package with default build flags; other build flags
  passed alongside it are not yet forwarded to the watched build.

### Several Go runtimes in one process — c-shared plugins

Plugin hosts load every plugin into one process: a DAW loading VST3/CLAP
plugins, Python loading extension modules. A `-buildmode=c-shared` library
carries its own Go runtime, so two Go plugins mean two Go runtimes side by
side, and upstream Go does not support that (golang/go#65050). forgo
libraries can share a process with each other, with several instances of
the same plugin, and with copies of one plugin loaded from two paths (a
VST2 and a VST3 build of the same effect).

Each library keeps a fully separate runtime: its own heap, garbage
collector, scheduler, and goroutines. The libraries only meet through the
C ABI, the same way the host talks to them:

- The `g` register of each runtime lives in its own thread-local slot
  (per-module ELF TLS on Linux, a `pthread_key` on macOS, `TlsAlloc` on
  Windows), so on any thread each runtime only sees its own goroutine,
  and callbacks can nest host → A → C → B → C → A on one thread.
- No Go symbol is exported from the library. ELF output is linked with
  `-Bsymbolic`, Mach-O uses two-level namespaces, and PE exports only the
  `//export` set, so neither runtime's references can bind to the other's
  copy, even when the host loads with `RTLD_GLOBAL`.
- Signal handlers chain: the library loaded last handles a signal first and
  passes it on when the thread is not running its own Go code. Upstream Go
  only does that for faults. forgo also forwards the preemption (`SIGURG`)
  and profiling (`SIGPROF`) signals each runtime sends to its own threads.
  Without that, the library loaded last swallows the earlier library's
  preemption requests. The earlier runtime then can never stop a goroutine
  in a tight loop, and its next garbage collection hangs the process.

`TestMultiRuntime` in `src/cmd/cgo/internal/testcshared` checks all of this.
It runs two different libraries and two copies of one library from 8 host
threads with forced GCs, many instances of one plugin, nested cross-library
callbacks with recovered panics, recovered nil dereferences while the other
runtime is busy, preemption of a spinning goroutine, a host `SIGSEGV`
handler installed before the libraries, and the golang/go#65050 reproducer.
[`pluginval/`](pluginval/README.md) repeats the multi-runtime cases in a
real plugin host: it validates pure-Go VST3 plugins with Tracktion's pluginval
at its highest strictness, with four Go runtimes in one process.

Process-wide state. Some things a Go runtime sets belong to the whole
process, so several runtimes share them:

- `os/signal.Notify`: every forgo runtime that calls `Notify` for a signal
  gets it. A runtime whose handler sits on top of another forgo runtime's
  handles the signal and also hands it down, and a runtime that has called
  `Reset` or `Stop` passes what it is handed on to the next forgo runtime
  only. A C handler installed before the libraries loses the signal while
  any runtime is listening, as upstream documents for `Notify` in a library,
  and gets it back once they have all called `Reset`. On Linux, macOS, and
  FreeBSD the runtimes tell each other's handlers from C ones through the
  dynamic loader (`dladdr`, `dlopen`, `dlsym`), so this works between forgo
  c-shared libraries, but a c-archive runtime, an upstream Go library, and a
  statically linked program fall back to upstream's rule: the runtime that
  asked last gets the signal. Upstream Go also restored the handler it had
  found on `Reset` even when another runtime had installed one on top since,
  which took the signal away from that runtime. A forgo runtime leaves its
  handler in place and passes signals through instead. `signal.Ignore` still
  ignores the signal for the whole process.
- `SIGPIPE`: a Go write to a closed pipe or socket fails with `EPIPE` in
  whichever runtime made it. The handler chain passes the signal down to
  the runtime running on that thread.
- Open-file limit: a Go program raises its soft `RLIMIT_NOFILE` at startup
  and puts the original back in the processes it starts. A forgo c-shared or
  c-archive library leaves the limit to the host. A runtime loaded after
  another one could not tell the raised limit from the host's own, and
  would hand the raised limit to its child processes. A library that needs
  many open files should raise the limit itself with `syscall.Setrlimit`.
- Environment: each runtime copies the environment when it loads.
  `os.Setenv` updates the C environment and the calling runtime's copy, but
  not the copies of runtimes that are already loaded, just as when a C host
  calls `setenv`. A `GODEBUG` change applies only to the runtime that made
  it.
- Exit: `os.Exit` in any runtime ends the process at once, running only that
  runtime's exit hooks. The Go runtime registers no C `atexit` handlers.
- Crash reports: an unrecovered panic or fault in one library prints that
  library's goroutines only, whatever `GOTRACEBACK` says, while the other
  runtimes keep running until the process exits.
- Windows timer resolution: the runtime sleeps on high-resolution waitable
  timers and leaves the system timer alone. On Windows versions without
  them it falls back to `timeBeginPeriod`/`timeEndPeriod`, which Windows
  counts per call, so one runtime cannot cancel another's request.
- Windows console control events: every runtime registers a handler when it
  loads, and Windows calls the newest first. The runtimes also register in a
  per-process list (a named file mapping keyed by the process ID), and the
  first handler Windows calls delivers `os.Interrupt` or `SIGTERM` to every
  runtime that called `Notify` for it. If any of them did, the event stops
  there, so C handlers below lose it, as with one runtime. For a close,
  logoff, or shutdown event that handler then blocks until Windows ends the
  process, while every runtime that asked cleans up.

`TestMultiRuntime` covers `Notify` delivery to both runtimes and the
hand-back on `Reset`, Ctrl+Break on Windows, `SIGPIPE`, the open-file limit,
and the crash reports.

Rules for plugin authors:

- Pass only C data between libraries. Never hand a Go pointer, func value,
  channel, or interface to another library. The other runtime cannot scan
  or unwind it.
- All instances of a plugin loaded from one file share one library image and
  one runtime, however many times the host opens it. Package-level
  variables are therefore shared by every instance. Keep per-instance state
  in an object the host holds a handle to, such as a `runtime/cgo.Handle`
  or an index into a table guarded by a mutex.
- Expect several host threads to call into the library at once, and expect
  an instance to move between threads.
- A library can be unloaded and loaded again; see "Unloading and
  reloading" below for the goroutine rules.

Limits:

- On darwin/amd64 (Intel Macs and Rosetta), upstream Go keeps `g` in the
  Apple-reserved TLS slot `%gs:0x30` that every runtime shares. forgo uses a
  per-runtime `pthread_key` instead, whose offset from `%gs` is in
  `runtime.tls_g`. Debuggers that read `g` from `%gs:0x30` won't find it.
- Unloading is supported on linux/amd64, linux/arm64, darwin/amd64,
  darwin/arm64, windows/amd64 and windows/arm64. On other platforms a
  library stays loaded after `dlclose`, as with upstream Go.
- A library built by upstream Go does not forward preemption signals. If
  one is loaded after a forgo library, it can still swallow the forgo
  library's preemption requests. Load upstream-built libraries first when
  you control the order.

#### Unloading and reloading

Hosts unload a plugin (`dlclose`, `FreeLibrary`) when its last instance is
removed, and may load it again later. Upstream Go libraries cannot be
unloaded (golang/go#11100): on Linux they are linked with `-z nodelete`, and
elsewhere the runtime's threads keep running code that has just been
unmapped. forgo libraries shut their runtime down when they are unloaded,
and the next load starts a fresh runtime with fresh package variables.

When the host drops the last reference to the library, its destructor
stops the world, ends every thread the runtime created and waits until they
are gone, restores the signal handlers it installed (Unix) or removes its
exception, console and power-event handlers (Windows), closes the network
poller, and unmaps all the memory the runtime mapped. When the process
exits instead, nothing changes: the runtime is left running, as before.

Goroutine lifetime rules:

- Goroutines still alive at unload are discarded where they stand. A
  goroutine blocked on a channel, a lock, a timer, `time.Sleep`, or network
  I/O is simply never resumed; its deferred calls do not run. Do any cleanup
  (flushing files, closing connections) from the host's deinit callback,
  through an `//export` function, before the host unloads the library.
- A goroutine inside a blocking system call or a C call must return before
  the unload. The runtime waits up to five seconds for it. If it is still
  there, the unload is refused with the fatal error `unloading a Go library
  while goroutines are blocked in system calls or C code`, because the
  library's code is about to disappear under it. Stop such goroutines from
  the deinit callback, for example by closing the file or pipe they read.
- No host thread may be inside a call into the library while it is being
  unloaded; that is a fatal error too.
- Unload libraries in the reverse order of loading when you can. Signal
  handlers form a chain, and a library can only take its handler out when
  it is the one installed last. If a later library (or a crash reporter)
  installed its handler on top, the unloaded library's handler is left in
  the chain and a signal forwarded to it crashes the process.

`TestMultiRuntime/*/unload` loads, uses and unloads one library 100 times
in one process while a second library stays loaded and busy. Each reload
must start with fresh package state, the library must really be gone after
every unload, the thread count and (on Linux) the number of memory mappings
must not grow, and the remaining library must still recover faults and
preempt goroutines afterwards. `UnloadBlocked` and `ExitBlocked` check the
refusal above and that exiting with such a goroutine still works.

### SIMD Mandelbrot benchmark

[`examples/mandelbrot`](examples/mandelbrot) is an allocation-free Mandelbrot
benchmark with matching scalar and SIMD kernels. It validates the two
checksums (with a tight ARM64 boundary-rounding tolerance), then reports
calibrated frame time, pixel throughput, and speedup.
Forgo enables Go 1.27's experimental `simd/archsimd` package by default as a
language feature: the example uses eight-lane AVX2 on AMD64 and four-lane Neon
on ARM64 without requiring callers to set `GOEXPERIMENT`.

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
