# The VM of a pod that runs a container image on a tab node
# (k8s/tabernetes/v86.go): the kernel, PID 1 and the BIOS that v86 boots.
#
#   docker build --output type=local,dest=out vm
#
# writes bzImage, init and seabios.bin to out/ (mise run vm).
ARG PYTHON_IMAGE=mirror.gcr.io/library/python:3.15-alpine
ARG KERNEL_IMAGE=mirror.gcr.io/library/ubuntu:26.04

# Linux 6.8 from Ubuntu 24.04 (linux-source), for i386, cross-compiled with
# clang on any build platform.
FROM --platform=$BUILDPLATFORM ${KERNEL_IMAGE} AS kernel
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
  && apt-get install -y --no-install-recommends bc bison bzip2 clang flex libelf-dev libssl-dev lld llvm lz4 make linux-source-6.8.0 \
  && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN tar xjf /usr/src/linux-source-6.8.0/linux-source-6.8.0.tar.bz2 --strip-components=1
COPY kernel.config /kernel.config
ENV ARCH=i386 LLVM=1
RUN set -eux; \
    make -s tinyconfig; \
    ./scripts/kconfig/merge_config.sh -m .config /kernel.config >/dev/null; \
    make -s olddefconfig; \
    for o in X86_32 M686 SERIAL_8250_CONSOLE VIRTIO_PCI VIRTIO_NET NET_9P_VIRTIO 9P_FS DEVTMPFS BLK_DEV_INITRD COMPAT_32BIT_TIME HIGHMEM4G KERNEL_LZ4 NO_HZ_IDLE; do \
      grep -q "^CONFIG_$o=y" .config || { echo "kernel.config: CONFIG_$o did not make it into .config" >&2; exit 1; }; \
    done; \
    make -s -j"$(nproc)" bzImage; \
    mkdir /out; \
    cp arch/x86/boot/bzImage /out/

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

# The BIOS that comes with v86, from the commit of the npm release in
# web/package.json (0.5.473+g315b2b0).
FROM scratch AS bios
ADD --checksum=sha256:73e3f359102e3a9982c35fce98eb7cd08f18303ac7f1ba6ebfbe6cdc1c244d98 \
    https://raw.githubusercontent.com/copy/v86/315b2b0/bios/seabios.bin /seabios.bin

FROM scratch
COPY --from=kernel /out/bzImage /bzImage
COPY --from=init /out/init /init
COPY --from=bios /seabios.bin /seabios.bin
