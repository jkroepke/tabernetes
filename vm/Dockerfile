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
  && apt-get install -y --no-install-recommends bc bison clang flex libelf-dev libssl-dev lld llvm lz4 make \
  && rm -rf /var/lib/apt/lists/*
ADD --checksum=7595a62d45ee1a2297eafa5f2ef5d0a45142d84d https://github.com/gregkh/linux.git#v7.3.4 /src
WORKDIR /src
COPY kernel.config /kernel.config
ENV ARCH=i386 LLVM=1
RUN set -eux; \
    make -s tinyconfig; \
    ./scripts/kconfig/merge_config.sh -m .config /kernel.config >/dev/null; \
    make -s olddefconfig; \
    for o in X86_32 M686 SERIAL_8250_CONSOLE VIRTIO_PCI VIRTIO_NET NET_9P_VIRTIO 9P_FS DEVTMPFS BLK_DEV_INITRD COMPAT_32BIT_TIME HIGHMEM4G KERNEL_LZ4 NO_HZ_IDLE; do \
      grep -q "^CONFIG_$o=y" .config || { echo "kernel.config: CONFIG_$o did not make it into .config" >&2; exit 1; }; \
    done; \
    make -s -j"$(nproc)" vmlinux; \
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
