# The VM of a pod that runs a container image on a tab node
# (k8s/tabernetes/v86.go): the kernel and PID 1 that v86 boots. There is no
# BIOS: the page starts the kernel itself (web/src/kernelboot.js).
#
#   docker build --output type=local,dest=out vm
#
# writes vmlinux and init to out/ (mise run vm).
ARG PYTHON_IMAGE=mirror.gcr.io/library/python:3.15-alpine
ARG KERNEL_IMAGE=mirror.gcr.io/library/ubuntu:26.04

# An upstream stable kernel (gregkh/linux mirrors the stable tree of
# kernel.org) for i386, cross-compiled with clang on any build platform. The
# checksum is the commit of the tag: Renovate updates both (renovate.json).
FROM --platform=$BUILDPLATFORM ${KERNEL_IMAGE} AS kernel
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
  && apt-get install -y --no-install-recommends bc bison clang flex libelf-dev libssl-dev lld llvm lz4 make patch perl python3 \
  && rm -rf /var/lib/apt/lists/*
ADD --checksum=7595a62d45ee1a2297eafa5f2ef5d0a45142d84d https://github.com/gregkh/linux.git#v7.3.4 /src
WORKDIR /src
COPY kernel.config /kernel.config
# Small changes to the kernel for pods; the head of each file says why.
COPY kernel-patches/ /kernel-patches/
# The hot kernel functions of pods first, those that call each other on the
# same page (kernel-patches/kernel-order.patch says why).
COPY kernel-order.txt /kernel-order.txt
ENV ARCH=i386 LLVM=1
RUN set -eux; \
    for p in /kernel-patches/*.patch; do patch -p1 --forward < "$p"; done; \
    make -s tinyconfig; \
    ./scripts/kconfig/merge_config.sh -m .config /kernel.config >/dev/null; \
    make -s olddefconfig; \
    for o in X86_32 M686 SERIAL_8250_CONSOLE VIRTIO_PCI VIRTIO_NET NET_9P_VIRTIO 9P_FS DEVTMPFS BLK_DEV_INITRD COMPAT_32BIT_TIME HIGHMEM4G KERNEL_LZ4 NO_HZ_IDLE X86_LOCAL_APIC LTO_CLANG_FULL; do \
      grep -q "^CONFIG_$o=y" .config || { echo "kernel.config: CONFIG_$o did not make it into .config" >&2; exit 1; }; \
    done; \
    # SLUB_TINY must be off; the loop above checks only options that are on.
    ! grep -q "^CONFIG_SLUB_TINY=y" .config || { echo "kernel.config: CONFIG_SLUB_TINY is still on" >&2; exit 1; }; \
    make -s -j"$(nproc)" vmlinux KERNEL_ORDER=/kernel-order.txt; \
    first="$(head -n 1 /kernel-order.txt)"; \
    addr="$(awk -v s="$first" '$3 == s { print $1; exit }' System.map)"; \
    stext="$(awk '$3 == "_stext" { print $1 }' System.map)"; \
    [ -n "$addr" ] && [ $((0x$addr - 0x$stext)) -lt 65536 ] \
      || { echo "kernel-order.txt: $first is not at the start of .text" >&2; exit 1; }; \
    # A stale order (renamed functions after a kernel upgrade) only makes
    # pods slower; below 90 % of the names, make a new one (kernel-order.md).
    found="$(awk 'NR == FNR { want[$1] = 1; n++; next } ($3 in want) && !seen[$3]++ { m++ } END { printf "%d %d", m, n }' /kernel-order.txt System.map)"; \
    echo "kernel-order.txt: ${found% *} of ${found#* } functions found"; \
    [ $(( ${found% *} * 100 )) -ge $(( ${found#* } * 90 )) ] \
      || { echo "kernel-order.txt is stale for this kernel, see vm/kernel-order.md" >&2; exit 1; }; \
    mkdir /out; \
    # The kernel unpacked: a bzImage unpacks itself on the emulated CPU,
    # which took longer than the BIOS. Without symbols it has 5.6 MB, 2 MB
    # with gzip, which GitHub Pages uses (the bzImage had 2.2 MB).
    llvm-objcopy --strip-all vmlinux /out/vmlinux

# PID 1 of the VM, a static binary for linux/386 with musl. zig cc comes
# with musl for i386 and cross-compiles on any build platform (an i386
# image would need emulation on arm64).
FROM --platform=$BUILDPLATFORM ${PYTHON_IMAGE} AS init
# renovate: pypi=ziglang
ARG ZIG_VERSION=0.17.1
RUN pip install --no-cache-dir "ziglang==${ZIG_VERSION}"
COPY init/init.c /src/init.c
RUN mkdir /out \
  && python -m ziglang cc -target x86-linux-musl -static -Os -s -Wall -Wextra -Werror -o /out/init /src/init.c

FROM scratch
COPY --from=kernel /out/vmlinux /vmlinux
COPY --from=init /out/init /init
