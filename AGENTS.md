# forgo — agent instructions

This repository is **forgo**, a real git fork of [golang/go](https://github.com/golang/go)
(currently synced to the Go 1.27 release branch — see [README.md](README.md) for
the full design writeup and how the daily upstream sync works). Code in
this repo is **not plain Go** — it compiles with a custom `forgo`/`compile`
binary (built under `bin/`, `pkg/tool/`) that understands six extra
language features. When you write or edit `.go` files in a forgo project,
use these features where they apply. Writing plain-Go idioms where a forgo
feature fits is a style regression, not a neutral choice.

Quick identification: a repo is a forgo project if it has a `bin/forgo`
(or `bin/forgo.exe`) built from this source tree, or if its code already
uses `?`, `throw`, postfix `if`, `//fgo:comptime`, `//fgo:macro`, or
`//fgo:nogc`. If unsure, check for a forgo `GOROOT` (a directory with
`src/cmd/compile/internal/forgo`).

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

## Rule 1c: collapse a one-statement guard into postfix `if`

**Default to this** when a guard clause's body is a single eligible
statement — most often `throw`, `return`, or `continue`/`break` — instead
of writing out the block form.

```go
// Write this:
func check(s string) (n int, err error) {
	throw "empty" if s == ""
	n = len(s)
	return
}

// Not this:
func check(s string) (n int, err error) {
	if s == "" {
		throw "empty"
	}
	n = len(s)
	return
}
```

`STMT if COND` is exactly equivalent to `if COND { STMT }` — it's parsed
and lowered to that form before type-checking, so there's no behavioral
difference, only a more compact guard clause.

Eligible statement kinds: an expression statement, a channel send, a plain
(non-`:=`) assignment (including `++`/`--`), `return`, `throw`, and
`break`/`continue`/`goto`.

Constraints:
- **Never** use it for a short variable declaration (`x := f() if cond`) —
  that's a compile error, and intentionally so: wrapping `:=` in an
  implicit block would silently shrink the new variable's scope to just
  that block. Use the block form (`if cond { x := f() }`) instead, or, if
  the variable needs to outlive the guard, declare it above the guard and
  assign inside it (`var x T; x = f() if cond`).
- **Never** use it for `fallthrough` — it must remain the literal last
  statement of its `switch` case, not the body of a synthesized `if`.
- Only reach for it when the guard body really is one statement. Don't
  contort two-or-more-statement bodies to fit — use the ordinary block
  form there; postfix `if` is a compaction, not a control-flow rewrite you
  should chase for its own sake.
- It composes with `throw` and `?`: `throw "empty" if s == ""` and
  `x := f()? if cond` (the `?` inside the guarded statement) both lower
  correctly, in that order (postfix `if` unwraps first, so `throw`/`?`
  lowering see an ordinary statement inside a regular `if` afterward).

## Rule 2: use `//fgo:comptime` for values that can be computed once, at compile time

If you're computing a `const` from a pure function of literal/constant
inputs — a table, a hash, a formatted string, a size — write the function
as `//fgo:comptime` and call it directly in the `const` declaration,
instead of computing it by hand or leaving it as a runtime `var`.

```go
//fgo:comptime
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
  `//fgo:comptime` only adds compile-time-callability, it doesn't remove
  the function from the binary.
- Folding only triggers when the comptime function is called **directly as
  a `const` initializer** (`const x = f(...)`), not in general
  constant-expression positions like an inline array length
  (`var a [f(3)]int` won't fold — assign to a `const` first, then use that
  `const`).
- The comptime interpreter supports a subset of Go: basic-type locals with
  Go's integer semantics (wraparound, truncating division, shifts,
  conversions), `if`, `for` (including `range`), `break`/`continue`,
  slices (`make`, `append`, `len`, indexing, slicing), struct and map
  literals, calls to other `//fgo:comptime` functions, and
  `fmt.Sprintf`/`strconv.Itoa`. A comptime function may return a slice
  (or struct or map): `const table = build(n)` puts the table in the
  binary's data, so prefer it over building lookup tables in `init()`.
  **No closures, generics, pointers or method calls** inside a comptime
  function body — if you need those, compute the value at runtime (`var`,
  `init()`) instead.
- Called at run time, a comptime function is nogc code (a `//fgo:nogc`
  root, see Rule 4).

## Rule 3: use `//fgo:macro` for AST-level code generation, not for things a normal function/generic can do

Reach for a macro only when you need to generate or transform *syntax* at
the call site — e.g. duplicating an argument expression, building
boilerplate control flow around a caller-supplied expression. If a regular
function, method, or generic would work, use that instead; macros are a
last resort because they have no hygiene and are harder to read/debug than
ordinary Go.

```go
//fgo:macro
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

## Rule 4: mark allocation-heavy code `//fgo:nogc`

Reach for `//fgo:nogc` when code allocates a lot and either (a) profiling
or benchmarking shows GC pressure from it, or (b) it builds persistent,
tree-/DAG-shaped structures (trees, persistent maps, ASTs, tries). Put the
directive on the function that does the work (the root); it and every
function of the same package it calls allocate their structs outside the
GC heap, reference-counted and freed immediately on last use. No `free`
call, no new syntax, no change to the types: plain `*T` code, and the same
types and functions stay garbage collected for other callers.

```go
type Node struct {
	Val         int
	Left, Right *Node
}

func build(d int) *Node {
	if d == 0 {
		return &Node{}
	}
	return &Node{Val: d, Left: build(d - 1), Right: build(d - 1)}
}

//fgo:nogc
func checksum(d int) int {
	return sum(build(d)) // the tree is freed right after sum
}
```

- **Keep the hot path inside the root's package.** Calls to other
  packages' functions (unless inlined), func values, interface methods,
  generic functions and goroutines leave nogc code: an object passed to
  one that may keep it or change its pointers is handed to the GC, with
  everything it reaches. So is one stored in a map, interface, slice,
  global, closure or channel, one whose field address escapes, and one
  given a pointer to itself outside a composite literal (a cycle). That's
  always correct, but those objects are then freed by the GC, later.
- **Rebuild instead of mutating linked fields.** To "change" a tree,
  rebuild the path (`&Node{Val: t.Val, Left: insert(t.Left, v), Right:
  t.Right}`), which reuses the dying nodes' memory for the new ones when
  the old tree isn't shared.
- **Don't reach for this by default.** It's a targeted optimization, not a
  style preference: plain GC-backed code remains the default for anything
  that isn't on a hot, allocation-heavy path.
- Ownership of parameters, release timing, and reuse are all inferred by
  the compiler — don't try to manage this by hand. See
  [README.md](README.md)'s "`//fgo:nogc` — reference counted memory, no
  GC" section for the full rules and limits.
  [examples/nogc](examples/nogc/main.fgo) and
  [examples/rbtree](examples/rbtree/main.fgo) are runnable references.

## Decision quick-reference

| You need to...                                                     | Use                        |
|----------------------------------------------------------------------|-----------------------------|
| Bubble up an error from a call unchanged                           | `?`                        |
| Bubble up an error with added context (`fmt.Errorf("...: %w", err)`) | manual `if err != nil`     |
| Fail a function outright with a new error                          | `throw`                    |
| Guard clause whose body is one statement (`throw`, `return`, `continue`, ...) | postfix `if` (`STMT if COND`) |
| Compute a constant from literal inputs (hash, table, formatted str) | `//fgo:comptime` + `const`|
| Generate/duplicate syntax at a call site                           | `//fgo:macro`              |
| A hot, allocation-heavy tree/DAG that's causing measurable GC pressure | `//fgo:nogc` on the function building it |
| Anything a plain function or Go generic already does well          | plain Go — don't reach for a forgo feature just because it exists |

## Building and running

This directory *is* the forgo `GOROOT` (`src/`, `bin/`, `pkg/`, `lib/`, etc.
all present). To build or run forgo code, point `GOROOT` at this repo's
root and use its `bin/forgo` (the real, unmodified `cmd/go` build, renamed
from `bin/go` by the wrapper build scripts — see README.md's "Least
invasive by design"):

```bash
GOROOT=/path/to/forgo /path/to/forgo/bin/forgo run ./yourpackage
GOROOT=/path/to/forgo /path/to/forgo/bin/forgo build ./...
```

Using the system's regular `go` (not this repo's `bin/forgo`) on forgo
source will fail to parse `?`, `//fgo:comptime`, and `//fgo:macro` —
always use the `bin/forgo` built from `src/` in this repo.

If you change the compiler itself (anything under
`src/cmd/compile/internal/{syntax,noder,types2,forgo}`), rebuild before
testing code changes:

```bash
cd src && GOROOT_BOOTSTRAP=/path/to/a/plain/go1.24+/install ./make.bat   # or make.bash / make.rc
```

The build script itself renames the freshly built `bin/go` to `bin/forgo`
as its last step — no separate copy needed.

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
- `src/cmd/compile/internal/noder/forgo_postfixif.go` — lowers postfix
  `if` (`STMT if COND`) to a plain `if COND { STMT }`; runs before
  `forgo_throw.go`/`forgo_try.go`.
- `src/cmd/compile/internal/noder/forgo_pragma.go` — parses
  `//fgo:comptime`/`//fgo:macro`.
- `src/cmd/compile/internal/noder/forgo_macro.go` — macro expansion pass.
- `src/cmd/compile/internal/types2/forgo.go` — folds `//fgo:comptime`
  calls in `const` initializers.
- Comptime has two interpreters that must stay in step: the compiler's
  (`src/cmd/compile/internal/forgo/interp.go`, walking `syntax` trees, also
  used for macros) and `src/go/types/forgo.go` (walking `go/ast`, so
  forgopls folds the same constants; std can't import `cmd/`). Everything
  but the tree walking is shared in `src/internal/fgoeval` (values,
  `BinaryOp`/`Convert` for Go's integer semantics, the native
  fmt/strconv/embed/json calls, `ToConstant`), which is listed in
  `cmd/dist/buildtool.go`'s bootstrap packages. Each interpreter tracks
  variables' basic types from their declarations (`typeOf`). Tests:
  `cmd/compile/internal/forgo/interp_test.go` and
  `go/types/forgo_test.go`. Composite constants fold through field
  selection, indexing and `len` in both checkers (`types2`'s
  `call.go`/`index.go`/`builtins.go`; `go/types`' generated files come from
  `go test -run=Generate -write=all go/types`, the others are edited by
  hand). `types2/forgo.go` caches folded values by initializer node
  (`forgoConstCache`), since the noder type checks a package twice when it
  has nogc roots.
- After changing `go/types` or `internal/fgoeval`, rebuild forgopls with
  `scripts/build-forgopls.sh`.
- `src/cmd/compile/internal/syntax/{tokens,scanner,parser,nodes}.go` — the
  `?` token/operator and `//fgo:` pragma parsing.

Reference counted (`//fgo:nogc`) code lives in:

- `src/cmd/compile/internal/noder/forgo_nogc.go` — after a first type
  check, finds the roots (`//fgo:nogc` and `//fgo:comptime` functions,
  methods of `//fgo:nogc` types) and every same-package function they call directly (types2
  `Uses`/`Selections`; generic functions and calls in `go`/`defer` are
  left out), deep-copies each callee's syntax as `_fgonogc_<name>`
  (`ir.FgoNogcPrefix`) with its calls pointed at the copies, and has the
  package type checked again (the `didNogc` recheck in `irgen.go`, like
  the SIMD midway one). Copies and roots get `ir.FgoNogc`; closures
  inherit it in `reader.funcLit`; roots are `Noinline`. `ir.Func.FgoNogc`
  is "this is nogc code", `FgoNogcABI` "this is a copy, called only by nogc
  code" (owned results, maybe owned parameters).
- `src/fgo/nogc/` — `Count`, `HandedOff`, `Live`, `Reuses` diagnostics.
- `src/cmd/compile/internal/fgorc/` — checks nogc code for the remaining
  error (writing elements of a pointer-slice field) and records which
  fields nogc code reassigns.
- `src/cmd/compile/internal/ssa/forgo_rc.go` — the SSA pass (before call
  expansion) that inserts increments, releases and reuse; runs only in
  nogc code (`fgoRCNeeded`).
- `src/cmd/compile/internal/types/forgo_rc.go` — type predicates shared by
  the above (`FgoIsRCPtr`: any pointer to a non-compiler struct);
  `escape/forgo_rc.go` and `ssagen/forgo_rc.go` — keep nogc allocations
  with owned fields off the stack and route `ONEW` (only) to the runtime
  allocator in nogc code.
- `src/runtime/forgo_rc.go` — the allocator (per-M caches outside the GC
  heap, header in front of each object), inc/dec/release/reuse, hand-off
  to the GC.

- Owned-parameter inference is `fgoParamTag` in `escape/forgo_rc.go`, for
  copies only; it replaces the parameter's escape tag with
  `types.FgoOwnedTag`. Panic unwinding uses `FUNCDATA_FgoShadow` (emitted
  from `ssagen/forgo_rc.go`, declared in `gc/forgo_rc.go`, read by
  `runtime/forgo_rc.go`).
- Calls: results of copies and of the runtime's allocators are owned;
  results of anything else get an increment. Before a call to code that
  isn't nogc, `fgoRCPass.handsOff` decides from the callee's escape tags
  (recorded by `ssagen.fgoNoteKeeps`) whether to hand the argument's
  object off: heap, callee or result leaks, or a mutator leak into a type
  with pointers.
- Stores: `lowerStores` turns each store into a struct-pointer field into
  `runtime.fgoRCStore` (or `fgoRCStoreGC` for a field that can lead back
  to the type), except the stores filling in a new object, which stay
  plain (no write barrier: `Func.fgoNoWB`) and get an inline check
  (`vetIf`, applied after `materialize`) that every stored pointer is a
  nogc object, calling `runtime.fgoRCVet` otherwise. `inlineCounts`
  replaces the pass's `fgoRCInc`/`fgoRCDec` calls with inline code for
  counted objects. Both read `runtime.fgoArena` (`lo`, `size`, `chunks`)
  at fixed offsets.
- Anything the pass can't follow (a pointer into an object stored,
  returned, merged or passed on; whole-struct copies holding struct
  pointers; unknown uses) hands the object off (`handOff` in `use`)
  instead of failing to compile.
- Runtime states: a pointer in the arena points to a counted object unless
  its 64 KiB chunk is flagged in `fgoArena.chunks` (holds handed-off
  objects), where `fgoArena.state` marks handed-off objects' starts and
  `fgoStateSlow` tells their interiors apart (`fgoQuick` is the inlinable
  part). `fgoRCHandoff` sets `fgoHeld` and walks everything the object
  reaches, copying owned strings and slices onto the GC heap and handing
  the old buffers to the GC too (views may still point into them).
  Counts of handed-off objects change atomically. The GC scans handed-off
  objects with counted references as roots (`fgoMarkCounted`) and traces
  the others from `fgoEscMark` (called for every non-heap pointer it
  finds, with interior pointers resolved by `fgoHandedObj`), queuing them
  per M (`fgoEscReach`); `fgoEscSweeper` frees the unreached ones with no
  counted references. Weak pointers to nogc objects hand them off and use
  `fgoWeakHandle`/`fgoMakeStrong`. `GODEBUG=fgoesc=1` traces sweeps, `=2`
  hand-offs and frees, `=3` stops at the first hand-off with a traceback.
- String and scalar slice fields ("buffer fields", `types.FgoBufType`):
  `ir.FgoBufField` (`ir/forgo_rc.go`) recognizes them. The set/put
  runtime calls take the object as their last argument and copy onto the
  GC heap when it isn't a counted object.
- Slices of struct pointers (`types.FgoPtrSliceType`,
  `ir.FgoPtrSliceField`): `walk/forgo_rc.go` rewrites each stored value
  into `fgoPtrsMake/Append1/AppendN` calls (declared in
  `typecheck/_builtin/runtime.go`; regenerate `builtin.go` with
  `forgo run mkbuiltin.go`), which flag a buffer holding a pointer no
  counted object can count (`fgoPtrsTaint`), and ssagen's store hook
  hands the result to `runtime.fgoPtrsPut`. `takeElems` replaces an
  element passed to an owned parameter with `runtime.fgoPtrsTake`.
- Mixed types (`types.FgoMixed`: fields holding GC values) allocate with
  `runtime.fgoRCNewMixed`: two link words before the header put each live
  one on `fgoMixed`, which the GC scans as a root (`fixedRootFgoMixed` in
  `mgcmark.go`).
- `planKeep` drops the increments of loads of an object's struct pointer
  fields that reach its release with no memory write in between, and the
  release becomes `runtime.fgoRCDecKeep`/`fgoRCDropReuseKeep` with a word
  mask. `fullInits` finds allocations whose fields are all stored right
  after and calls `fgoRCNewInit`/`fgoRCNewAtInit`, which don't clear.
- After compiler changes, build with `-a` (or `forgo clean -cache` both
  before and after installing the compiler): the build cache has served
  objects built by the previous compiler.

Their hooks into upstream files are single lines in `gc/main.go`,
`gc/obj.go`, `escape/utils.go`, `escape/escape.go`, `escape/expr.go`,
`escape/assign.go`, `ssagen/ssa.go` (including one that turns off the
in-place `x = append(x, ...)` path for buffer fields, which would bypass
the copying store), `walk/walk.go`, `typecheck/_builtin/runtime.go`,
`ssa/compile.go`, `ssa/writebarrier.go`, `ssa/func.go` (`fgoNoWB`),
`noder/irgen.go` (the recheck), `noder/reader.go` (closures),
`noder/noder.go` and `noder/lex.go` (the pragma), `ir/node.go` (the
`FgoNogc` flag), `runtime/runtime2.go` (`m` fields), `runtime/panic.go`
(recovery and Goexit), `runtime/mheap.go` (weak handles),
`runtime/mgcmark*.go`, `internal/abi/symtab.go` and `runtime/funcdata.h`
(the funcdata index), and `go/build/deps_test.go`.

Hot reload (`forgo run --watch`) lives in four places:

- `src/cmd/link/internal/ld/forgo_hot.go` — pins symbols that did not
  change to the address they already have in the running program, so only
  changed code is laid out; writes the symbol record and the reload
  manifest.
- `src/runtime/forgo_hot.go` — registers a reloaded module with the
  runtime and patches function entry points with the world stopped.
- `src/runtime/fgohot/` — the in-process agent the watcher talks to. The
  Windows build additionally resolves the hot-linked image's own PE
  import table (`pe_windows.go`) since the agent maps the image itself
  rather than handing it to the OS loader, which is the only thing that
  would otherwise do that.
- `src/cmd/go/internal/run/forgo_watch.go` — the `--watch` driver. Reads
  ELF program headers on Linux, PE section headers on Windows
  (`loadSegments`/`loadSegmentsPE`), and forces `-buildmode=exe` on
  Windows so the toolchain's PIE-by-default doesn't randomize the load
  address the linker relied on when it pinned symbols.

The hooks these need in upstream files are all single lines, marked by a
call to a `fgohot*` function (`main.go`, `data.go`, `lib.go`, `symtab.go`,
`typelink.go`, `fips140.go`, `pe.go`).

These are all either new files (untouched by upstream merges) or single-line
hooks into existing files. Follow that same pattern if you're adding a new
forgo feature: put the logic in a new file, and touch existing upstream
files with only a single-line hook, so daily upstream merges stay
conflict-free.

See [README.md](README.md) for full design rationale, limitations, and
runnable examples under `examples/`.
