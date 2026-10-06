# Validating multi-runtime Go plugins with pluginval

[pluginval](https://github.com/Tracktion/pluginval) drives an audio plugin
the way a DAW does. This directory uses it to check that several forgo
`c-shared` runtimes can share one real plugin host process (see "Several Go
runtimes in one process" in the top-level README). `TestMultiRuntime` in
`src/cmd/cgo/internal/testcshared` covers the same cases with a synthetic C
host.

## Pure Go VST3

The plugins implement VST3 in Go. There is no VST3 SDK, no C or C++ source,
no header, and no `#include` anywhere under this directory.
`internal/vst3/pure_test.go` checks that.

VST3 is a COM-style ABI: every object is a pointer to a table of C function
pointers. `internal/vst3` implements the parts these plugins need:

- `abi.go`: the structs, interface IDs (TUIDs), and constants, defined in
  Go. Steinberg's [C API](https://github.com/steinbergmedia/vst3_c_api) was
  only a reference for the layouts. `layout_test.go` checks sizes and
  offsets, and `result_*.go` holds the result codes, which are HRESULTs on
  Windows.
- `cabi.go`: the interface tables for `IPluginFactory`/`IPluginFactory2`,
  `IComponent`, `IAudioProcessor`, and `IEditController`. They are C arrays
  of pointers to the `//export` functions in `exports.go`. The cgo
  preamble declares those functions, `free`, and a few trampolines for
  calling other modules' interfaces, all by hand.
- `com.go`: `queryInterface`/`addRef`/`release`. Each interface the host
  holds is a `malloc`ed view: its table and an integer handle into a
  Go-side table of objects. No Go pointer is handed to the host.
- `plugin.go`: one stereo or mono bus each way, one automatable parameter,
  `setupProcessing`, `process` with parameter changes, and
  `getState`/`setState`. The component is also its own edit controller.
- `factory.go`: the module's `GetPluginFactory`, and `ModuleFactory` for
  calling another module's factory.

## What is tested

`run.sh` builds three VST3 modules with forgo, each a `-buildmode=c-shared`
library that exports `GetPluginFactory`:

- `gain/`: scales the signal by its parameter.
- `drive/`: a tanh soft clipper.
- `loader/`: a module whose classes live in other modules.

It lays out the `.vst3` bundles itself (`Contents/MacOS` with an
`Info.plist` on macOS, `Contents/x86_64-win` on Windows,
`Contents/x86_64-linux` on Linux, and the arm64 equivalents), then
validates each one:

| Module                   | Classes | Go runtimes in the process      |
| ------------------------ | ------- | ------------------------------- |
| `forgo-go-gain.vst3`     | 1       | gain                            |
| `forgo-go-drive.vst3`    | 1       | drive                           |
| `forgo-go-plugins.vst3`  | 3       | loader, gain, drive, gain-copy  |

The `forgo-go-plugins.vst3` bundle holds the loader and the gain and drive
libraries, plus a byte-identical copy of gain at a second path. Its factory
lists one class per library:

| Class                         | Library           | Case                                |
| ----------------------------- | ----------------- | ----------------------------------- |
| `Forgo Go Gain`               | `forgo-gain`      | first Go plugin                     |
| `Forgo Go Drive`              | `forgo-drive`     | a different Go plugin               |
| `Forgo Go Gain (second load)` | `forgo-gain-copy` | the same Go binary, loaded twice    |

The loader opens each library with `dlopen(RTLD_LOCAL)`, or `LoadLibrary`
on Windows through `syscall`, before it lists its classes, as a DAW loads a
project's plugins before it plays. Each class forwards to its library's own
`GetPluginFactory`, so the host calls straight into that library's Go code
for every VST3 method. pluginval validates one plugin file per invocation,
but it tests every class in that file one after another in the same
process. The pluginval 1.0.4 command line always validates in process. So
each class is validated while four Go runtimes are resident in the
process.

For each class, pluginval opens the plugin cold and warm. It then runs its
whole test suite on one instance, which includes processing at several
sample rates and block sizes, state save and restore, parameter fuzzing,
automation, and parameter changes from other threads. Several of those
tests open, close, and reopen more instances of their own. Under all of this
the Go code keeps its runtime busy (`internal/goplugin`):

- Every processed block is copied into a fresh Go slice, and every 32nd
  block calls `runtime.GC()` on the host's audio thread.
- Every instance has a worker goroutine. The audio thread hands it each
  block through an atomic, and the worker spin-waits for the next block
  without blocking, as real-time audio code often does. It then allocates a
  peak envelope of the block. The spin loop makes no calls, so a collection
  can only stop it with an asynchronous preemption signal. That needs the
  signal routing from #5, which lets a runtime loaded before another one
  still preempt its own goroutines.

`run.sh` fails if pluginval fails or times out for any module, or tests a
different number of classes than the table above.

## Running it

Requirements: a forgo toolchain built in this checkout (`src/make.bash`),
a C compiler that cgo can use, and curl. Then:

```bash
./pluginval/run.sh
```

It downloads pluginval 1.0.4 unless `PLUGINVAL` names a binary. Other
settings are environment variables documented at the top of `run.sh`:
`FORGO` (a different forgo binary, used as `GOROOT` as well), `STRICTNESS`,
`REPEAT`, `ROUNDS`, `TIMEOUT_MS`, and `BUILD_DIR`. On Windows, run it from
Git Bash.

It runs `forgo test -vet=off ./...` first (vet cannot parse forgo's syntax
yet), then, for each module and round:

```bash
pluginval --strictness-level 10 --repeat 3 --randomise --skip-gui-tests \
  --timeout-ms 300000 --validate <module>.vst3
```

The plugins have no editor, so `--skip-gui-tests` leaves nothing out, and
it lets the run work on headless CI machines. CI runs one round of this on
every push and pull request, as the "pluginval multi-runtime VST3
validation" step of the build-and-test job.

## Results

pluginval 1.0.4 (JUCE 8.0.3), strictness 10, `--repeat 3 --randomise`:

| Platform      | Toolchain                     | Result                                                   |
| ------------- | ----------------------------- | -------------------------------------------------------- |
| darwin/arm64  | this branch                   | pass, every module                                       |
| darwin/arm64  | before #5 (`7eaed83ed5`)      | gain and drive pass alone; the loader hangs in its first class and pluginval times out |

Before #5, the runtime loaded last owned the process's signal handler and
dropped the preemption signals that the earlier runtimes sent to their own
threads. Alone, gain and drive each have the process to themselves and
pass. In the loader's run against `7eaed83ed5`, pluginval starts testing
`Forgo Go Gain`, whose library was loaded before drive and gain-copy. Its
next `runtime.GC()` needs to stop the instance's spinning worker, but the
preemption signal never arrives. pluginval prints `*** FAILED: Timeout after
30 secs` (run with `TIMEOUT_MS=30000`) and exits with status 1. To repeat
this, build the toolchain at `7eaed83ed5` in another checkout and point
`FORGO` at its `bin/forgo`.

On darwin/arm64 that hang is the only way the pre-#5 runtime fails here: the
`g` register already had a per-runtime TLS slot there. darwin/amd64, where
golang/go#65050 crashes with `bad sweepgen` and `unexpected return pc`, is
covered by `TestMultiRuntime` in CI's `darwin-amd64-multiruntime` job.
