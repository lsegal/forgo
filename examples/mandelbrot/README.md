# Mandelbrot SIMD benchmark

This example compares a scalar Mandelbrot escape-time kernel with a kernel
built with forgo's `simd/archsimd` package. It processes eight pixels per
vector with AVX2 on AMD64 and four with Neon on ARM64. Both kernels perform
the same `float32` operations and produce the same checksum before timing
begins.

Run with forgo:

```text
cd examples/mandelbrot
forgo run .
```

Optional positional arguments set width, height, and maximum iterations:

```text
forgo run . 1920 1080 512
```

SIMD is enabled by default in forgo. The width must be a multiple of eight on
AMD64 or four on ARM64. The benchmark reports calibrated time per frame,
throughput, allocations, and the SIMD speedup over the scalar kernel. On
other architectures, the example prints a skip message.
