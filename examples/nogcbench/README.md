# nogc vs GC benchmarks

Seven allocation-heavy workloads, each written once with ordinary types
and compiled both ways into one binary: `runNogc` is marked `//fgo:nogc`,
so the workloads it calls run as nogc copies whose structs are reference
counted, while `runGC` calls the original, garbage collected functions.
Both must produce the same checksum.

```bash
forgo run .                 # every workload once, nogc
forgo run . -gc             # the same, garbage collected
./bench.sh [scale [runs]]   # both side by side (default: scale 4, 5 runs)
```

`bench.sh` runs each workload in its own process, so peak RSS is per
workload, and reports the median run. `trie`'s 50,000 words are made at
compile time (a `//fgo:comptime` function returning a `[]string`), so
they are data in the binary and neither mode allocates them.

| workload | what it does |
|---|---|
| `binarytrees` | The Benchmarks Game's GC benchmark: a long-lived tree, and many short-lived trees built, walked and dropped. |
| `rbtree` | Inserts keys into a persistent red-black tree map and then overwrites them all; every insert rebuilds its path. |
| `deriv` | Differentiates large random expressions four times each, simplifying as it goes, like a compiler rewriting syntax trees. |
| `wordfreq` | Counts words with a skewed distribution in a search tree keyed by string fields, then lists the top ten. |
| `chunks` | Keeps a sorted multiset as a persistent tree of chunks whose leaves hold up to 64 keys in a slice field; inserts split full leaves, range counts scan them, and a periodic pass shifts every key in place. |
| `trie` | Counts words in a persistent trie whose nodes keep their children in a slice of pointers; each insert splices a rebuilt child into a copy of its parent's children. |
| `mandeltree` | Renders a zoom into the Mandelbrot set adaptively: each frame builds a quadtree that subdivides only where the image has detail, then drops it. |

The plain [Mandelbrot example](../mandelbrot) is not here: it allocates
nothing per frame, so the GC never runs and nogc changes nothing.

## Results

Apple M-series laptop (Darwin arm64), scale 4. "cpu" is user plus system
time across all threads, which includes the work the GC does on other
cores. The machine was busy with other work during this run (its
one-core `mandeltree` row was rerun on its own, median of 3), so absolute
times vary a little between runs; compare the columns.

All cores:

| workload | gc wall | nogc wall | gc cpu | nogc cpu | gc GCs | gc max pause | gc peak RSS | nogc peak RSS | nogc reuses |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| binarytrees | 189.2 ms | 250.7 ms | 458.1 ms | 250.1 ms | 92 | 0.08 ms | 15.3 MB | 13.1 MB | 0 |
| rbtree | 614.0 ms | 626.6 ms | 2121.8 ms | 625.2 ms | 83 | 0.10 ms | 55.4 MB | 29.5 MB | 15271488 |
| deriv | 2799.5 ms | 2930.6 ms | 6107.0 ms | 2909.8 ms | 101 | 0.16 ms | 120.7 MB | 103.3 MB | 12950131 |
| wordfreq | 217.5 ms | 313.8 ms | 227.8 ms | 313.0 ms | 6 | 0.02 ms | 9.9 MB | 9.0 MB | 0 |
| mandeltree | 6200.1 ms | 6188.2 ms | 6338.7 ms | 6163.6 ms | 60 | 0.17 ms | 31.7 MB | 23.0 MB | 0 |
| chunks | 619.1 ms | 540.6 ms | 1088.1 ms | 539.4 ms | 254 | 0.11 ms | 22.9 MB | 12.2 MB | 13133068 |
| trie | 697.7 ms | 528.6 ms | 2062.9 ms | 528.9 ms | 129 | 0.13 ms | 22.2 MB | 17.2 MB | 5386526 |

One core (`GOMAXPROCS=1 ./bench.sh 4 5`), as in a one-CPU container, where
the GC cannot hide its work on idle cores:

| workload | gc wall | nogc wall | gc cpu | nogc cpu | gc GCs | gc max pause | gc peak RSS | nogc peak RSS | nogc reuses |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| binarytrees | 335.4 ms | 251.4 ms | 333.3 ms | 251.0 ms | 67 | 0.05 ms | 18.4 MB | 12.5 MB | 0 |
| rbtree | 1305.0 ms | 622.4 ms | 1302.6 ms | 621.3 ms | 66 | 0.03 ms | 59.3 MB | 28.9 MB | 15271488 |
| deriv | 4509.0 ms | 2899.0 ms | 4464.8 ms | 2891.1 ms | 76 | 0.06 ms | 137.6 MB | 102.3 MB | 12950131 |
| wordfreq | 225.7 ms | 303.7 ms | 225.4 ms | 303.4 ms | 6 | 0.01 ms | 9.4 MB | 8.6 MB | 0 |
| mandeltree | 6255.3 ms | 6122.0 ms | 6235.2 ms | 6108.7 ms | 71 | 0.06 ms | 26.7 MB | 22.1 MB | 0 |
| chunks | 842.0 ms | 519.5 ms | 840.3 ms | 518.5 ms | 205 | 0.02 ms | 27.4 MB | 11.7 MB | 13133068 |
| trie | 1245.4 ms | 524.8 ms | 1239.9 ms | 524.0 ms | 87 | 0.06 ms | 27.7 MB | 17.6 MB | 5386526 |

## Reading the numbers

- **CPU.** nogc does all its memory management inline, on the thread that
  allocates. The GC does much of its work on other cores, so with cores
  to spare it often finishes sooner in wall time while burning far more
  CPU: 3.9x as much on `trie`, 3.4x on `rbtree`, 2.1x on `deriv`, 2.0x on
  `chunks`, 1.8x on `binarytrees`. On a server whose cores are all busy,
  or with one core, that work comes out of the program's own time, and
  nogc is 1.3x to 2.4x faster.
- **Memory.** nogc frees each object right after its last use, so its
  peak is close to the live data: about half the GC build's peak RSS on
  `rbtree` and `chunks`.
- **Pauses.** nogc never stops the world. Go's GC pauses are already
  short (well under a millisecond here), so this matters less than the
  CPU and memory.
- **Reuse.** `rbtree` and `deriv` rebuild their trees, and nogc hands most
  dying nodes' memory straight to their replacements (15 and 13 million
  reuses).
- **Slices.** `chunks` keeps its keys in slice fields, which a counted
  object owns outside the GC heap. nogc wins even with spare cores (541
  vs 619 ms) and uses about half the memory: each insert copies one leaf
  into a buffer whose memory comes straight from the leaf it replaces,
  while the GC build allocates a new array per insert and runs 254
  collections. `trie` keeps its children in a slice of pointers: nogc is
  1.3x faster with spare cores and 2.4x with one. Each insert passes the
  child it descends into to the recursive call while the parent is still
  alive, so the compiler moves that child out of the parent's slice when
  the parent is unique: the child arrives unique too, and its memory is
  reused (5.4 million reuses).
- **Where nogc loses.** `binarytrees` with spare cores: Go's allocator is
  inlined and its GC runs elsewhere. `wordfreq`: rebuilding a path copies
  each node's string field, where the GC build just copies the pointer.
- **No difference.** `mandeltree` spends nearly all its time computing
  escape counts, so the memory manager hardly shows.
