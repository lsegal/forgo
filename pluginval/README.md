# Validating multi-runtime Go plugins with pluginval

[pluginval](https://github.com/Tracktion/pluginval) drives an audio plugin
the way a DAW does. This directory uses it to check that several forgo
`c-shared` runtimes can share one real plugin host process (see "Several Go
runtimes in one process" in the top-level README). `TestMultiRuntime` in
`src/cmd/cgo/internal/testcshared` covers the same cases with a synthetic C
host.

## What is tested

`run.sh` builds two Go plugins with forgo as `-buildmode=c-shared`
libraries:

- `gain/`: scales the signal by its parameter.
- `drive/`: a tanh soft clipper.

It copies the gain library to a second path, then builds one VST3 module,
`forgo-go-plugins.vst3`, around all three libraries. The module is a thin
C++ shim on the VST3 SDK (`shim/goplugin.cpp`). Its factory exposes one
plugin class per library:

| Class                         | Library           | Case                                |
| ----------------------------- | ----------------- | ----------------------------------- |
| `Forgo Go Gain`               | `forgo-gain`      | first Go plugin                     |
| `Forgo Go Drive`              | `forgo-drive`     | a different Go plugin               |
| `Forgo Go Gain (second load)` | `forgo-gain-copy` | the same Go binary, loaded twice    |

pluginval validates one plugin file per invocation, but it tests every class
in that file one after another in the same process. The pluginval 1.0.4
command line always validates in process. The first instance of any class
loads all three libraries, as a DAW loads a project's plugins before it
plays. So each class is validated while three Go runtimes are resident in
the process.

For each class, pluginval opens the plugin cold and warm. It then runs its
whole test suite on one instance, which includes processing at several
sample rates and block sizes, state save and restore, parameter fuzzing,
automation, and parameter changes from other threads. Several of those
tests open, close, and reopen more instances of their own. Under all of this
the Go code keeps its runtime busy:

- Every processed block is copied into a fresh Go slice, and every 32nd
  block calls `runtime.GC()` on the host's audio thread.
- Every instance has a worker goroutine. The audio thread hands it each
  block through an atomic, and the worker spin-waits for the next block
  without blocking, as real-time audio code often does. It then allocates a
  peak envelope of the block. The spin loop makes no calls, so a collection
  can only stop it with an asynchronous preemption signal. That needs the
  signal routing from #5, which lets a runtime loaded before another one
  still preempt its own goroutines.

`run.sh` fails if pluginval fails, times out, or tests fewer than three
classes.

## Running it

Requirements: a forgo toolchain built in this checkout (`src/make.bash`),
a C compiler that cgo can use, a C++ compiler, CMake 3.25 or newer, git
(CMake fetches VST3 SDK 3.8.0, which is MIT licensed), and curl. Then:

```bash
./pluginval/run.sh
```

It downloads pluginval 1.0.4 unless `PLUGINVAL` names a binary. Other
settings are environment variables documented at the top of `run.sh`:
`FORGO` (a different forgo binary, used as `GOROOT` as well), `STRICTNESS`,
`REPEAT`, `ROUNDS`, `TIMEOUT_MS`, `BUILD_DIR`, and `VST3_SDK_DIR`. On
Windows, run it from Git Bash.

Each round runs:

```bash
pluginval --strictness-level 10 --repeat 3 --randomise --skip-gui-tests \
  --timeout-ms 300000 --validate forgo-go-plugins.vst3
```

The plugins have no editor, so `--skip-gui-tests` leaves nothing out, and
it lets the run work on headless CI machines. CI runs one round of this on
every push and pull request, as the "pluginval multi-runtime VST3
validation" step of the build-and-test job.

## Results

pluginval 1.0.4 (JUCE 8.0.3), strictness 10, `--repeat 3 --randomise`:

| Platform      | Toolchain                     | Result                                       |
| ------------- | ----------------------------- | -------------------------------------------- |
| darwin/arm64  | this branch                   | pass, 10 of 10 rounds                        |
| darwin/arm64  | before #5 (`7eaed83ed5`)      | hangs in the first class; pluginval times out |
| macOS (CI)    | this branch                   | pass                                         |
| Windows (CI)  | this branch                   | pass                                         |

Before #5, the runtime loaded last owned the process's signal handler and
dropped the preemption signals that the earlier runtimes sent to their own
threads. In the run against `7eaed83ed5`, pluginval starts testing
`Forgo Go Gain`, whose runtime was loaded first. Its next `runtime.GC()`
needs to stop the instance's spinning worker, but the preemption signal
never arrives. A sample of the hung process shows the worker still spinning
in `goplugin.(*instance).analyze` and the gain runtime's `sysmon` calling
`preemptone` again and again. pluginval prints `*** FAILED: Timeout after
30 secs` (run with `TIMEOUT_MS=30000`) and exits with status 1. To repeat
this, build the toolchain at `7eaed83ed5` in another checkout and point
`FORGO` at its `bin/forgo`.

On darwin/arm64 that hang is the only way the pre-#5 runtime fails here: the
`g` register already had a per-runtime TLS slot there. darwin/amd64, where
golang/go#65050 crashes with `bad sweepgen` and `unexpected return pc`, is not
covered because forgo does not build for it yet.
