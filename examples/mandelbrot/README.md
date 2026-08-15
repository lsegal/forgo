# Mandelbrot SIMD benchmark

This example compares a scalar Mandelbrot escape-time kernel with an
eight-pixel-wide AVX2 kernel built with Go's experimental `simd/archsimd`
package. Both kernels perform the same `float32` operations and produce the
same checksum before timing begins.

Run with forgo:

```text
cd examples/mandelbrot
forgo run .
```

Optional positional arguments set width, height, and maximum iterations:

```text
forgo run . 1920 1080 512
```

SIMD is enabled by default in forgo. The width must be a multiple of eight.
The benchmark requires an AMD64 CPU with AVX2 and reports calibrated time per
frame, throughput, allocations, and the SIMD speedup over the scalar kernel.
On other architectures the example prints a skip message.
