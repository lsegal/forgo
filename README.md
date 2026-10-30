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
well enough to type-check it correctly, and folds `//fgo:comptime`
constants the way the compiler does. `//fgo:macro` is still
compiler-only: macro calls show as false-positive diagnostics.
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

Supported inside a comptime function body:

- locals of the basic types, with Go's semantics for them: integers wrap
  around at their type's size, integer division truncates, shifts and
  bitwise operators work, and conversions like `uint64(x)`, `byte(x)`,
  `string(r)` and `[]byte(s)` convert;
- `if`, `for` loops (3-clause, condition-only, and `range` over an int, a
  string, a slice or a map), `break` and `continue`, compound assignment,
  `++`/`--`;
- slices: literals, `make`, `append`, `len`, `cap`, indexing, element
  assignment, and slicing of slices and strings; struct and map literals,
  field selection and map indexing;
- calls to other `//fgo:comptime` functions, and a small allow-list of
  real stdlib calls (`fmt.Sprintf`, `strconv.Itoa`, and the
  [`comptime/embed`](#comptimeembed--compile-time-file-reads) helpers
  below).

Anything else (closures, goroutines, pointers, method calls, labeled
`break`, ...) reports a compile error if reached.

A comptime function can return a slice, struct or map, which folds into a
constant laid out in the binary's data (see "Non-scalar (struct/slice/map)
consts" below), so building a table at compile time allocates nothing at
run time:

```go
//fgo:comptime
func squares(n int) []int {
	s := make([]int, n)
	for i := range n {
		s[i] = i * i
	}
	return s
}

const sq = squares(1000)
```

Called at run time, a comptime function is nogc code: it is a
[`//fgo:nogc`](#fgonogc--reference-counted-memory-no-gc) root.

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

### `//fgo:nogc` — reference counted memory, no GC

Mark a function `//fgo:nogc` and the structs it allocates live outside the
GC heap: the compiler counts references Perceus-style and frees each
object the moment its last reference dies. There's no `free` call, no new
syntax and no marker in your types: you write plain `*T` code with
ordinary structs, including types from other packages.

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

func sum(n *Node) int { // no count operations: see below
	if n == nil {
		return 0
	}
	return n.Val + sum(n.Left) + sum(n.Right)
}

//fgo:nogc
func run() int {
	t := build(20)
	return sum(t)
} // t's last use was sum(t): the whole tree is freed right after it
```

**Roots and callees.** A `//fgo:nogc` function is a *root*. The root and
every function of the same package it calls directly (and the ones those
call, and so on) run as nogc code: their `&T{...}` and `new(T)` of
structs are reference counted, and they count the pointers they hold.
A `//fgo:nogc` directive on a type makes its methods roots, and every
`//fgo:comptime` function is a root.

**The same code, both ways.** Being nogc belongs to where an object is
allocated, not to its type. The compiler keeps every function a root
reaches as it is, garbage collected, for other callers, and builds a
second, nogc copy of it that nogc code calls instead. So one binary can
run the same code and the same types both ways: `build` above allocates
counted nodes when `run` calls it and garbage collected ones when `main`
does. The count lives in a header in front of each counted object.

**Handing objects to the GC.** Nogc code can give its objects to any other
code. Code that doesn't count can't be trusted to keep the counts right,
so when a nogc object reaches it, the object is *handed off* to the
garbage collector first, along with every nogc object it can reach. From
then on its fields hold ordinary GC values, and the GC frees it once
nothing points to it (and nogc code no longer counts it). That happens
when nogc code:

- stores the pointer in an interface, a map, a slice, a global, a
  closure, a channel, or a field of a garbage collected object;
- passes it to a function that isn't nogc code (another package's, a
  func value, an interface method, a generic function, a goroutine) whose
  escape analysis says it may keep the pointer, return it or something
  inside it, or change pointers in what it points to. A callee that only
  reads, or only changes numbers in a struct with no pointers, leaves the
  object counted, and calls the compiler inlines are nogc code anyway;
- takes the address of one of the object's fields, other than to call a
  method that only changes numbers (`n.mu.Lock()` is fine);
- assigns a field through which the object can reach itself again
  (`l.Next = m`) outside a composite literal, which could form a cycle:
  the GC collects those;
- stores a garbage collected pointer into it: a counted object can only
  count nogc objects.

```go
//fgo:nogc
func index(words []string) map[string]*Entry {
	m := map[string]*Entry{}
	for _, w := range words {
		e := &Entry{Word: w} // counted
		m[w] = e             // into a map: handed to the GC
	}
	return m
}
```

To change a structure without handing it off, rebuild it:

```go
func insert(t *Node, v int) *Node {
	if t == nil {
		return &Node{Val: v}
	}
	if v < t.Val {
		return &Node{Val: t.Val, Left: insert(t.Left, v), Right: t.Right}
	}
	return &Node{Val: t.Val, Left: t.Left, Right: insert(t.Right, v)}
}
```

When `t` isn't shared, this updates in place: each node on the path dies as
it's rebuilt, and its memory goes straight to its replacement (Koka's
"functional but in-place").

What the compiler does:

- **Escape analysis still applies.** A struct that doesn't outlive the
  function goes on the stack as usual, unless it has pointer, string or
  slice fields, which counted objects own.
- **Ownership of parameters is inferred** for the nogc copies of
  functions. A function that returns or stores a parameter, or allocates
  something of the same size, takes ownership of it: the caller hands
  over its reference (or adds one if it still needs the value). Other
  parameters are borrowed, which costs nothing.
- **Each owned value is released right after its last use**, not at the
  end of its scope.
- **A field read is counted only when it has to be.** A child read from a
  field that's never reassigned is kept alive by its parent, so walking a
  tree costs zero count operations. Other reads are counted, unless every
  use happens before any other memory operation.
- **Counting is inline.** Increments and decrements of counted objects are
  a few instructions in place; the runtime is called only for handed-off
  objects and for frees.
- **Reuse.** A release followed by an allocation of the same size, later in
  the same block or on every path after it, hands over the memory when it
  was the last reference. A release that has no such allocation is pushed
  into each branch that follows it, so each branch's own allocation can
  take the memory, as in `insert` above. That covers loops like
  `c = &Counter{N: c.N + 1}` and the recursive rebuilds above:
  `rev(l, acc)` in the example reverses a 100,000-element list without
  allocating. Fields read just before their parent's release aren't
  counted: if the parent goes, its references pass to the reads instead
  (Perceus's drop specialization), and a reused or new object that the
  literal writes in full isn't cleared first.
- **Panics release what unwound frames owned.** Each owned value sits in a
  stack slot listed in the function's metadata while it's owned. When
  `recover` discards frames, or `Goexit` ends a goroutine, the runtime
  releases what those slots hold.

**Strings.** A string field of a counted object owns its bytes, in a
buffer outside the GC heap that's freed with the object. Storing into it
copies the string. Reading it is free: you get an ordinary `string`
pointing into the buffer, and the compiler keeps the object alive for as
long as that string is used. Where the string outlives the function
(stored in a map or global, returned, or passed to something that keeps
it), escape analysis sees it and the read copies it onto the GC heap
instead.

**Slices.** A slice field of numbers, bools, or arrays or structs of them
works the same way: storing copies (capacity equals length, so `append`
to the field always copies into a new buffer), and reading gives a view
whose element writes change the object. A read that escapes is a copy.
In nogc code, storing into these fields copies into garbage collected
objects too.

**Slices of struct pointers.** In a counted object, a field like
`Kids []*Node` owns a reference to each element. Nogc code doesn't assign
its elements in place (that's a compile error): it stores a new slice,
built from a slice literal, `append`, or slicing of another such field.
When `t.Kids[j]` goes to an owned parameter and the code after the call
reads only `t.Kids[:j]` and `t.Kids[j+1:]` (the usual splice), the
compiler moves the child out of `t`'s slice when `t` is unique, so the
callee sees it unique too.

**Any other field.** Maps, interfaces, funcs, channels, pointers to
anything but structs, `[]string`: those values stay on the GC heap. While
an object holding them is alive, the GC scans it as a root.

**Current limits:**

- Generic functions aren't copied: calls to them leave nogc code.
- A cycle has to go through the GC: an object that can reach itself is
  handed off when the cycle is made.
- A handed-off object is freed after a GC cycle, not at its last use.
- Weak pointers to nogc objects hand them off; finalizers and cleanups
  on them aren't supported.

**Benchmarks.** [examples/nogcbench](examples/nogcbench) runs seven
allocation-heavy workloads (the Benchmarks Game's binary trees, a
persistent red-black tree map, symbolic differentiation, word counting,
an adaptive quadtree Mandelbrot zoom, a chunked sorted index whose leaves
hold slices, and a trie whose nodes hold slices of children), each
written once and compiled both ways into one binary. With all cores free,
the GC build often finishes sooner, because its collector runs on other
cores, but it uses up to 3.9x the CPU time. With one core, nogc is 1.3x
to 2.4x faster on the tree workloads. nogc's peak memory is about half the
GC build's on `rbtree` and `chunks`, and it never pauses. The two slice
workloads (`chunks`, `trie`) are faster with nogc even with all cores
free. It is slower where every allocation is tiny and short-lived
(`binarytrees`, 1.3x the GC build's wall time with cores to spare) and
where rebuilding copies string fields (`wordfreq`, 1.4x).

| workload (scale 4) | gc wall, all cores | nogc wall | gc cpu | nogc cpu | gc wall, 1 core | nogc wall, 1 core | gc peak RSS | nogc peak RSS |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| binarytrees | 189 ms | 251 ms | 458 ms | 250 ms | 335 ms | 251 ms | 15.3 MB | 13.1 MB |
| rbtree | 614 ms | 627 ms | 2122 ms | 625 ms | 1305 ms | 622 ms | 55.4 MB | 29.5 MB |
| deriv | 2800 ms | 2931 ms | 6107 ms | 2910 ms | 4509 ms | 2899 ms | 120.7 MB | 103.3 MB |
| wordfreq | 218 ms | 314 ms | 228 ms | 313 ms | 226 ms | 304 ms | 9.9 MB | 9.0 MB |
| mandeltree | 6200 ms | 6188 ms | 6339 ms | 6164 ms | 6255 ms | 6122 ms | 31.7 MB | 23.0 MB |
| chunks | 619 ms | 541 ms | 1088 ms | 539 ms | 842 ms | 520 ms | 22.9 MB | 12.2 MB |
| trie | 698 ms | 529 ms | 2063 ms | 529 ms | 1245 ms | 525 ms | 22.2 MB | 17.2 MB |

See [examples/nogc](examples/nogc/main.fgo) for a runnable version that
checks live object counts and reuse, and
[examples/rbtree](examples/rbtree/main.fgo) for a full program: a
persistent red-black tree map, the benchmark from the Perceus paper.

```
$ forgo run ./examples/rbtree
insert 1,000,000 keys           1042.2ms  1000000 live 21693947 reused   0 GCs
overwrite every key              664.4ms  1000000 live 19454319 reused   0 GCs
scale every value                 37.3ms  1000000 live  1000000 reused   0 GCs
scale, keeping the old version    42.7ms  2000000 live        0 reused   0 GCs
after old's last use                      1000000 live
after t's last use                              0 live
```

Every insert rebuilds the path from the root, and every rebuilt node
takes the memory of the node it replaces, so the tree is updated in place
and the garbage collector never runs. Scaling a version that is still held
elsewhere copies it instead, and the old version stays valid until its
last use. The same program without the directive, garbage collected,
takes about 1.2s to insert and 0.62s to overwrite, with 91 and 18
collections.

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
