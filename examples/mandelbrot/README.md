# Mandelbrot SIMD benchmark

This example compares a scalar Mandelbrot escape-time kernel with an
eight-pixel-wide AVX2 kernel built with Go's experimental `simd/archsimd`
package. Both kernels perform the same `float32` operations and produce the
same checksum before timing begins.

Set `GOEXPERIMENT=simd`, then run either version from the example directory:

```text
cd examples/mandelbrot
forgo run ./fgo
forgo run go/main.go
```

`fgo/main.fgo` uses forgo's language features. `go/main.go` is the equivalent
implementation in regular Go syntax. When using the toolchain directly from
this source checkout, set `GOROOT` to the repository root and invoke the
`forgo` executable under its `bin` directory.

Optional positional arguments set width, height, and maximum iterations:

```text
forgo run ./fgo 1920 1080 512
forgo run go/main.go 1920 1080 512
```

The width must be a multiple of eight. The benchmark requires an AMD64 CPU
with AVX2 and reports calibrated time per frame, throughput, allocations, and
the SIMD speedup over the scalar kernel.

The two implementations produce the same checksum and benchmark the same SIMD
operations.
