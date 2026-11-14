# The order of the kernel functions

`kernel-order.txt` lists kernel functions, one name per line. The linker
puts them first in the `.text` of `vmlinux`, in this order. The order makes
pods start faster in v86. It does not change what the kernel does, only
where its code is.

The order belongs to one kernel. A new kernel version (Renovate bumps it),
a new kernel option or a new clang can move code to other functions. Then
the order still works, but less well. This file explains why the order
helps, how the build uses it, how to check it after a kernel upgrade and
how to make a new one.

## Why the order matters

v86 runs guest code in two ways: an interpreter, and a JIT that makes a
WebAssembly module out of guest code. The JIT works per page of 4 KiB:

- A page gets a module when its code ran 750k instructions (the JIT
  threshold of the fork, `JIT_THRESHOLD` in `src/rust/jit.rs` of
  jkroepke/v86). Until then, the interpreter runs it, which is about 2-3x
  slower for kernel code.
- A module covers one page. A jump or call into another page leaves the
  module, goes back to the main loop of v86 and enters the module of the
  other page (or the interpreter). Each change costs time: kernel code
  left its module every ~30 instructions.
- The kernel JIT modules that tabernetes ships (`jit.bin`, see AGENTS.md)
  are the 50 pages with the most module entries in a recorded pod start.
  Code on other pages is not in it.

In the normal link order, the functions are sorted by source file. The
hot code of a pod start (page faults, `execve`, `mmap`, 9p reads, the
scheduler, freeing memory) is in hundreds of files, so it was spread over
many pages: 90 % of the kernel instructions of an nginx start were on ~96
pages, 99 % on ~250. Many of these pages ran a lot, but not enough for the
JIT, and the 50 modules of `jit.bin` covered only part of them.

With the order, the same instructions are on far fewer pages, and a
function often sits on the page of the function that calls it:

| nginx start, kernel of v1.13.0 | link order | ordered |
|---|---|---|
| pages that hold 90 % of the kernel instructions | 96 | 48-50 |
| pages that hold 99 % | 254 | 134-137 |
| kernel instructions that run in JIT code (with `jit.bin`) | 52 % | 77 % |
| kernel instructions per module entry | 29 | 38 |
| goapp: kernel instructions in JIT code (with `jit.bin`) | 37 % | 67 % |

The lower bound (all functions packed by heat, without gaps) is ~32 pages
for 90 %.

## Measured gain

Node.js, `tools/pod-bench` of jkroepke/v86, kernel JIT modules of the same
kernel (`KJIT=20000 KJIT_WL=warm KJIT_TOP=50`, like `jit.bin`), medians of
alternating runs, 2026-11-14. The order for these numbers was made from the
workloads warm, goapp and glibc only, so nginx shows what a workload that
was not in the recording gains:

| workload | link order | ordered | |
|---|---|---|---|
| nginx, first pod of a process, modules warmed like `jit.js` (12 runs) | 556 ms | 510 ms | -8 % |
| nginx, later pods (24 runs) | 516 ms | 457 ms | -11 % |
| nginx without kernel JIT modules (16 runs) | 562 ms | 538 ms | -4 % |
| glibc text tools (6 runs) | 3442 ms | 3074 ms | -11 % |
| goapp, a Go binary up and 20 scrapes (15 runs) | 1110 ms | 1059 ms | -5 % |
| goapp, first pod of a process (8 runs) | 1107 ms | 1105 ms | 0 |
| cpu, a JIT-bound shell loop with gzip (6 runs) | 3906 ms | 3946 ms | 0 (noise) |

Headless Chromium, the same runs in a module worker (v86.wasm compiled by the
page, a new browser for every run, so the first round is the first pod of a
tab; image reads synchronous like the OPFS handles of `layers.js`):

| workload | link order | ordered | |
|---|---|---|---|
| nginx, first pod (8 runs) | 662 ms | 554 ms | -16 % |
| nginx, later pods (16) | 444 ms | 408 ms | -8 % |
| goapp, first pod (12) | 1242 ms | 1215 ms | -2 % (noise) |
| goapp, later pods (6) | 1007 ms | 963 ms | -4 % |

The run-to-run noise is about 10 %. A density-only order (no call
clusters) gave about half of the gain on nginx.

`jit.bin` grows: its 50 modules now cover fuller pages (~0.9 -> ~1.7 MB,
still small with gzip).

## How the build uses it

- `kernel-patches/kernel-order.patch` adds `KERNEL_ORDER` to the top
  Makefile of the kernel: `make vmlinux KERNEL_ORDER=<file>` passes the
  file to ld.lld as `--symbol-ordering-file`, with
  `--no-warn-symbol-ordering`, so names that the kernel does not have are
  skipped quietly.
- The kernel is built with full LTO (`LTO_CLANG_FULL`). lld then generates
  the code itself and puts every function in its own section, so it can
  place each function. This does not work with ld.bfd or without
  function sections.
- Only `.text` is ordered. The entry code (`.entry.text`, e.g.
  `entry_SYSENTER_32`) and `.noinstr.text` (e.g. `exc_page_fault`) stay in
  their own output sections; they are small and on few pages anyway.
- The Dockerfile checks two things and fails the build when one is wrong:
  the first function of the file must be in the first 64 KiB of `.text`
  (else the order was not applied at all), and at least 90 % of the names
  of the file must exist in the new kernel (else the order is stale, see
  below). It prints the share of names it found.
- `jit.bin` is recorded from the kernel of each build (the web task of
  `mise.toml`), so it follows the order on its own.

## After a kernel upgrade

A patch release of the same kernel (7.3.x) rarely renames functions; the
order keeps working. A new minor version (7.4) or a new clang can rename,
inline or split hot functions and add new ones. What happens then:

- Names that are gone are skipped. The functions behind them land after
  the ordered ones, in link order, so their code is spread out again.
- New hot functions (a new name for old code, a new helper) are not in the
  file, so they are not near their callers.
- Nothing breaks and the kernel behaves the same. Pods only get slower,
  up to the numbers of the link order above.

The 90 % check of the Dockerfile catches mass renames, not a few moved hot
functions. So after each kernel minor upgrade, and when a profile shows
much kernel time in the interpreter, check the order like this:

1. Build the kernel and keep its `System.map`: `docker build --target
   kernel -t tabernetes-kernel vm`, then `docker create` a container of
   that image and `docker cp` `/src/System.map` and `/out/vmlinux` out of
   it. Put both and `init` (from `mise run vm` or a release) in one
   directory, e.g. `/tmp/vm`.
2. In a checkout of jkroepke/v86, build v86 with the guest counters and set
   up the benchmark (once):

       V86_GUESTSTAT=1 make build/v86.wasm build/libv86.mjs
       tools/pod-bench/setup.sh

3. Record nginx and check the pages:

       cd tools/pod-bench
       READINTO=1 VMDIR=/tmp/vm GSTAT=/tmp/nginx.json node bench.mjs nginx 1
       node kpages.cjs /tmp/vm/System.map /tmp/nginx.json

   It prints how many pages hold 50, 90 and 99 % of the kernel
   instructions, and the same for a perfect order ("packed"). With a good
   order, 90 % of the instructions are on less than ~1.6x the packed
   number (2026-11-14: 48 against 32). In link order it was ~3x (96).
   If the value is clearly above, make a new order.

## Making a new order

The recording needs the interpreter for all code, because only the
interpreter records the calls between kernel functions
(`gueststat::edge` in the fork). With the JIT threshold at 2 billion, no
page gets a module.

1. Build the kernel and get `vmlinux`, `System.map` and `init` into
   `/tmp/vm` as in step 1 above. The old order does not matter for the
   recording: it maps addresses to function names with `System.map`.
2. Record the four workloads with the `V86_GUESTSTAT=1` build of v86:

       cd tools/pod-bench
       for wl in warm nginx goapp glibc; do
         READINTO=1 JITCFG=4=2000000000 VMDIR=/tmp/vm GSTAT=/tmp/$wl.json node bench.mjs $wl 1
       done

   warm is a generic busybox start (processes, files, pipes, TCP over
   loopback), nginx a Debian image with a shell entrypoint, goapp a static
   Go binary (Go uses `int 0x80`, threads and futexes), glibc text tools on
   Debian. Each recording counts the same in the order. Do not record
   only one workload: an order of busybox alone did not help goapp.
3. Make the order and put it into tabernetes:

       node korder.cjs /tmp/vm/System.map /tmp/warm.json /tmp/nginx.json /tmp/goapp.json /tmp/glibc.json > vm/kernel-order.txt

4. Build again and check it with `kpages.cjs` as above, and compare the
   two kernels with pod-bench (`KJIT=20000 KJIT_WL=warm KJIT_TOP=50`,
   several alternating runs, `VMDIR` of each kernel).

`korder.cjs` uses C3 (Ottoni and Maher, "Optimizing Function Placement
for Large-Scale Data-Center Applications", CGO 2017): from the hottest
function down, a function joins the cluster of its most frequent caller,
while the cluster stays within one page (`CLUSTER=4096`); then the
clusters are sorted by heat per byte, hottest first. The heat of a function
is the share of the sampled kernel instructions (one sample per 997
instructions, at the start address of the block) that fell into it.

The current file was made on 2026-11-14 from Linux 7.3.4 with the patches
and the configuration of this directory, built with clang 18 (the release
build uses the clang of Ubuntu 26.04: 1917 of its 1961 names exist there,
and nginx had 90 % of its kernel instructions on 48 pages).

## What did not go in

- The order of user code (the image) cannot change: tabernetes runs
  images as they are.
- Propeller and AutoFDO of the kernel (`CONFIG_PROPELLER_CLANG`,
  `CONFIG_AUTOFDO_CLANG`) also place code by a profile, even basic blocks,
  but they need a `perf` profile with branch records of real hardware. A
  profile of the emulator would have to be converted; not tried.
