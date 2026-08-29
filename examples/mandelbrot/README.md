# Mandelbrot SIMD benchmark

This example compares a scalar binary32 Mandelbrot escape-time kernel with a
kernel built with forgo's `simd/archsimd` package. It processes eight pixels
per vector with AVX2 on AMD64 and four with Neon on ARM64.

Both timed kernels consume one precomputed array of `{float32 re, float32 im}`
points and write one `{int32 iterations, uint32 escaped}` record per pixel.
Grid generation, checksums and allocation remain outside the timed boundary.
Every binary32 operation has the same order in both bodies; before timing, the
benchmark requires every scalar and SIMD result record to agree exactly.

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

Set `MANDELBROT_RESULTS` to write the SIMD result records in little-endian
binary form. Nupp's `bench/kernel-subset-spike/forgo.sh` uses that output to
compare all 786,432 records byte-for-byte against its automatic `f32x8` AOT
lowering.

Three runs of each matched implementation on Apple arm64 with Forgo 0.6.1 measured a median 153.73
MPix/s for Forgo's explicit `f32x4` Neon body and 174.81 MPix/s for Nupp's
automatic `f32x8` body. The scalar controls measured 59.72 and 61.02 MPix/s,
respectively. Every run produced checksum `46372998` and the same 6,291,456
result bytes.
