# forgo — agent instructions

This repository is **forgo**, a real git fork of [golang/go](https://github.com/golang/go)
(currently synced to the 1.26.0 release — see [README.md](README.md) for
the full design writeup and how the daily upstream sync works). Code in
this repo is **not plain Go** — it compiles with a custom `forgo`/`compile`
binary (built under `bin/`, `pkg/tool/`) that understands four extra
language features. When you write or edit `.go` files in a forgo project,
use these features where they apply. Writing plain-Go idioms where a forgo
feature fits is a style regression, not a neutral choice.

Quick identification: a repo is a forgo project if it has a `bin/forgo`
(or `bin/forgo.exe`) built from this source tree, or if its code already
uses `?`, `throw`, `//forgo:comptime`, or `//forgo:macro`. If unsure, check
for a forgo `GOROOT` (a directory with `src/cmd/compile/internal/forgo`).

## Rule 1: propagate errors with `?`, not manual `if err != nil` chains

**Default to this.** Any time you write a function that calls other
fallible functions and needs to bubble up their errors, give it a **named**
error result and use `?` instead of hand-written `if err != nil { return
..., err }` blocks.

```go
// Write this:
func loadConfig(path string) (cfg *Config, err error) {
	f := open(path)?
	data := parse(f)?
	cfg = data.normalize()?
	return
}

// Not this:
func loadConfig(path string) (*Config, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	data, err := parse(f)
	if err != nil {
		return nil, err
	}
	cfg, err := data.normalize()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
```

Both compile to the same thing — `?` is lowered to exactly the manual form
before type-checking — but the `?` version is what a forgo codebase expects
to see. Only fall back to a manual `if err != nil` when you need to do
something with the error other than propagate it unchanged (wrap it with
extra context via `fmt.Errorf("...: %w", err)`, log it, retry, etc.).

### The rules `?` requires

1. **The enclosing function (or func literal) must declare its last result
   as a named `error`.** `func f() (T, err error)` or `func f() (err
   error)`. If you're adding `?` to a function that currently returns
   `(T, error)` with an unnamed result, **name the error result first**
   (e.g. `error` → `err error`) — this is a required, mechanical edit, not
   optional cleanup.
2. `expr?` used where a value is expected (`x := f()?`, `f()?.g()`, `if
   f()? { ... }`) assumes `expr` evaluates to exactly `(value, error)`.
3. `expr?` used as a whole statement (`f()?` alone on a line) assumes
   `expr` evaluates to a bare `error`. To discard a value from a
   `(value, error)` call in statement position, write `_ = f()?`, not bare
   `f()?`.
4. It chains: `?` is a postfix operator at the same precedence as `.` and
   `(...)`, so `foo()?.bar()?` parses as `(foo()?).bar()?`.
5. It works inside `if`/`for` init clauses (`if f()?; cond { ... }`) and
   inside a `for` loop's `Cond`/`Post` clauses — not just the loop body.
6. It does **not** work: in a package-level `var`/`const` initializer;
   directly inside a labeled statement when that would require hoisting
   code before the label (put the fallible call above the label instead);
   or in a `for` loop's `Cond`/`Post` when that clause is a channel send.

If a function's error result isn't named and can't be renamed (e.g. it's
part of a public API you can't change), keep using manual `if err != nil`
there — don't contort the signature just to use `?`.

## Rule 1b: bail out early with `throw`, not `return zero..., err`

**Default to this** for guard clauses that fail a function outright,
instead of spelling out every leading zero value by hand.

```go
// Write this:
func makeThing(s string) (*Thing, error) {
	if s == "" {
		throw errors.New("empty")
	}
	return &Thing{name: s}, nil
}

// Or, equivalently, for a plain message:
func makeThing(s string) (*Thing, error) {
	if s == "" {
		throw "empty"
	}
	return &Thing{name: s}, nil
}

// Not this:
func makeThing(s string) (*Thing, error) {
	if s == "" {
		return nil, errors.New("empty")
	}
	return &Thing{name: s}, nil
}
```

`throw EXPR` lowers to a `return` that passes `EXPR` through as the last
(error) result and fills every other result with its zero value — `throw
errors.New("empty")` and `throw "empty"` are exactly equivalent; the
second is sugar for the first. Unlike `?`, `throw` does **not** require a
named error result — it works in any function whose last result's type is
spelled `error`.

Constraints:
- Every result before the last must have a **syntactically obvious** zero
  value: pointer, slice, map, chan, func, interface (including `any` and
  `error`), or a basic type (`string`, `bool`, the numeric types). A named
  struct/array/defined type isn't nil-able and can't be zeroed without a
  type checker, so `throw` there is a compile error: `no default value for
  <result>`. Use a manual `return` in that case.
- `throw "literal text"` requires the file to already `import "errors"`
  (however it's named, or via `.`) — the compiler can't add an import for
  you after the fact, since `go build` computes a package's dependencies
  from the literal source text before the compiler ever runs.
- `throw` is a **contextual** keyword, not a reserved word: `throw(x)`,
  `throw.field`, `throw = x`, and a bare `throw` used as an identifier
  (e.g. `runtime.throw`) are unaffected and parse exactly as they did
  before. Only `throw` immediately followed by a new operand (another
  name or a literal) is read as the `throw` statement.

**Never hand-write `return nil, errors.New(...)` (or any other
`return zero..., err`-shaped statement that only exists to fail the
function out) when the last result is `error`** — that's exactly what
`throw` replaces. The only time a manual `return ..., err` belongs in a
forgo codebase is when `err` isn't a *new* error being introduced right
there (i.e. it's a value already bound to a variable from earlier in the
function) — that's not what `throw` is for; see Rule 1 for that case
(`?`), or write the manual `return` if `?` doesn't apply either (e.g. the
error result isn't named).

## Rule 2: use `//forgo:comptime` for values that can be computed once, at compile time

If you're computing a `const` from a pure function of literal/constant
inputs — a table, a hash, a formatted string, a size — write the function
as `//forgo:comptime` and call it directly in the `const` declaration,
instead of computing it by hand or leaving it as a runtime `var`.

```go
//forgo:comptime
func crc(s string) uint32 {
	var c uint32 = 0xFFFFFFFF
	for i := 0; i < len(s); i++ {
		c ^= uint32(s[i])
		for b := 0; b < 8; b++ {
			if c&1 != 0 {
				c = (c >> 1) ^ 0xEDB88320
			} else {
				c >>= 1
			}
		}
	}
	return ^c
}

const magicHash = crc("forgo") // computed by the compiler, not at runtime
```

Constraints:
- The function is ordinary Go and also compiles/runs normally at runtime —
  `//forgo:comptime` only adds compile-time-callability, it doesn't remove
  the function from the binary.
- Folding only triggers when the comptime function is called **directly as
  a `const` initializer** (`const x = f(...)`), not in general
  constant-expression positions like an inline array length
  (`var a [f(3)]int` won't fold — assign to a `const` first, then use that
  `const`).
- The comptime interpreter supports a deliberately small subset of Go:
  `int`/`float`/`string`/`bool` locals and arithmetic, `if`, 3-clause `for`,
  compound assignment, calls to other `//forgo:comptime` functions, and
  `fmt.Sprintf`/`strconv.Itoa`. **No closures, generics, maps, slices,
  method calls, or `range` loops** inside a comptime function body — if you
  need those, compute the value at runtime (`var`, `init()`) instead.

## Rule 3: use `//forgo:macro` for AST-level code generation, not for things a normal function/generic can do

Reach for a macro only when you need to generate or transform *syntax* at
the call site — e.g. duplicating an argument expression, building
boilerplate control flow around a caller-supplied expression. If a regular
function, method, or generic would work, use that instead; macros are a
last resort because they have no hygiene and are harder to read/debug than
ordinary Go.

```go
//forgo:macro
func double(x Node) Node {
	return Quote(func() {
		Splice(x) + Splice(x)
	})
}

double(compute()) // expands, at compile time, to: compute() + compute()
```

- `Quote(func(){ ... })` captures the function literal's body as a syntax
  template. A single-expression body unwraps so the macro can be used in
  expression position; anything else makes it a statement-position macro.
- `Splice(x)` substitutes the syntax tree bound to macro parameter `x` (or
  a value derived from it) into the template.
- Macro parameter/return types (`Node` above) are placeholders — macro
  signatures are never type-checked, only their expansion is. Don't try to
  give macro parameters real Go types.
- Macro calls are matched by plain function name (no overloading/qualified
  calls), expanded before type-checking, and only within function bodies
  — not in a package-level `var`/`const` initializer.

## Decision quick-reference

| You need to...                                                     | Use                        |
|----------------------------------------------------------------------|-----------------------------|
| Bubble up an error from a call unchanged                           | `?`                        |
| Bubble up an error with added context (`fmt.Errorf("...: %w", err)`) | manual `if err != nil`     |
| Fail a function outright with a new error                          | `throw`                    |
| Compute a constant from literal inputs (hash, table, formatted str) | `//forgo:comptime` + `const`|
| Generate/duplicate syntax at a call site                           | `//forgo:macro`              |
| Anything a plain function or Go generic already does well          | plain Go — don't reach for a forgo feature just because it exists |

## Building and running

This directory *is* the forgo `GOROOT` (`src/`, `bin/`, `pkg/`, `lib/`, etc.
all present). To build or run forgo code, point `GOROOT` at this repo's
root and use its `bin/forgo` (a copy of the real, unmodified `bin/go` under
a different name — see README.md's "Least invasive by design"):

```bash
GOROOT=/path/to/forgo /path/to/forgo/bin/forgo run ./yourpackage
GOROOT=/path/to/forgo /path/to/forgo/bin/forgo build ./...
```

Using the system's regular `go` (not this repo's `bin/forgo`) on forgo
source will fail to parse `?`, `//forgo:comptime`, and `//forgo:macro` —
always use the `bin/forgo` built from `src/` in this repo.

If you change the compiler itself (anything under
`src/cmd/compile/internal/{syntax,noder,types2,forgo}`), rebuild before
testing code changes, then re-copy the binary:

```bash
cd src && GOROOT_BOOTSTRAP=/path/to/a/plain/go1.24+/install ./make.bat   # or make.bash / make.rc
cp ../bin/go ../bin/forgo   # bin/go.exe -> bin/forgo.exe on Windows
```

## Version, CI, releases, upstream sync

forgo has its own version (independent of the golang/go release it's synced
against), tracked in `FORGO_VERSION` at the repo root. Don't hand-edit that
file — use `./scripts/version.sh {show|patch|minor|major}`, or trigger the
"Release" GitHub Actions workflow (`workflow_dispatch`, pick
`patch`/`minor`/`major`), which bumps it, tags `vX.Y.Z`, builds the
toolchain for linux/amd64, darwin/arm64, and windows/amd64, and publishes a
GitHub release with prebuilt archives. The "CI" workflow builds and
smoke-tests the toolchain on Linux/macOS/Windows on every push/PR. The
"Upstream Sync" workflow runs daily, merging golang/go's default branch in
via a PR (auto-merged if CI passes, left open for manual resolution if
there are conflicts) — don't "fix" a stale-looking source file without
checking whether it's actually a few days behind upstream and about to sync.
Users install a release via `install/install.sh` (Linux/macOS) or
`install/install.ps1` (Windows) — see README.md.

## Where the implementation lives (only relevant if you're editing the compiler, not writing forgo code)

- `src/cmd/compile/internal/forgo/` — the comptime/macro interpreter.
- `src/cmd/compile/internal/noder/forgo_try.go` — lowers `?` to plain
  `:=`/`if`/`return` statements.
- `src/cmd/compile/internal/noder/forgo_pragma.go` — parses
  `//forgo:comptime`/`//forgo:macro`.
- `src/cmd/compile/internal/noder/forgo_macro.go` — macro expansion pass.
- `src/cmd/compile/internal/types2/forgo.go` — folds `//forgo:comptime`
  calls in `const` initializers.
- `src/cmd/compile/internal/syntax/{tokens,scanner,parser,nodes}.go` — the
  `?` token/operator and `//forgo:` pragma parsing.

These are all either new files (untouched by upstream merges) or single-line
hooks into existing files — see README.md's "Least invasive by design" if
you're adding a new forgo feature and want to follow the same pattern.

See [README.md](README.md) for full design rationale, limitations, and
runnable examples under `examples/`.
