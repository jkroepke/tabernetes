# Tabernetes: a Kubernetes node that joins a control plane running in a browser.
ARG GO_IMAGE=mirror.gcr.io/library/golang:1.27-trixie
ARG BASE_IMAGE=mirror.gcr.io/library/debian:trixie-slim
ARG KUBERNETES_VERSION=v1.37.2

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS agent
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY agent/go.mod agent/go.sum ./
RUN go mod download
COPY agent/ ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /tabernetes .

FROM ${BASE_IMAGE} AS download
ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
ARG KUBERNETES_VERSION
ARG CONTAINERD_VERSION=2.4.2
ARG RUNC_VERSION=v1.5.2
ARG CNI_PLUGINS_VERSION=v1.9.1
ARG CRICTL_VERSION=v1.37.0
RUN apt-get update && apt-get install -y --no-install-recommends binutils ca-certificates curl && rm -rf /var/lib/apt/lists/*
WORKDIR /out
# Every download is checked against the sha256 its release publishes. That
# catches broken or swapped files on the way, not a compromised release.
RUN set -eux; \
    get() { \
      name="${1##*/}"; \
      curl -fsSLo "/tmp/${name}" "$1"; \
      sum="$(curl -fsSL "$2" | awk -v f="${name}" 'NF == 1 { print $1; exit } $2 == f || $2 == "*" f { print $1; exit }')"; \
      [ -n "${sum}" ] || { echo "no checksum for ${name} in $2" >&2; exit 1; }; \
      echo "${sum}  /tmp/${name}" | sha256sum -c -; \
    }; \
    mkdir -p usr/local/bin opt/cni/bin; \
    url="https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/${TARGETARCH}/kubelet"; \
    get "${url}" "${url}.sha256"; \
    mv /tmp/kubelet usr/local/bin/kubelet; \
    url="https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    get "${url}" "${url}.sha256sum"; \
    tar -xzf "/tmp/${url##*/}" -C usr/local bin/containerd bin/containerd-shim-runc-v2; \
    url="https://github.com/opencontainers/runc/releases/download/${RUNC_VERSION}"; \
    get "${url}/runc.${TARGETARCH}" "${url}/runc.sha256sum"; \
    mv "/tmp/runc.${TARGETARCH}" usr/local/bin/runc; \
    url="https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${TARGETARCH}-${CNI_PLUGINS_VERSION}.tgz"; \
    get "${url}" "${url}.sha256"; \
    tar -xzf "/tmp/${url##*/}" -C opt/cni/bin ./bridge ./host-local ./loopback ./portmap; \
    url="https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    get "${url}" "${url}.sha256"; \
    tar -xzf "/tmp/${url##*/}" -C usr/local/bin crictl; \
    rm -f /tmp/*.tar.gz /tmp/*.tgz; \
    # crictl and the CNI plugins ship with symbols: about 12 MB less compressed
    strip usr/local/bin/crictl opt/cni/bin/*; \
    chmod +x usr/local/bin/*; \
    # the pause image of this containerd, for the images stage
    usr/local/bin/containerd config default | sed -n "s/^ *sandbox = [\"']\(.*\)[\"']\$/\1/p" > /pause-image; \
    grep . /pause-image

# pasta from upstream: Debian trixie stays on a snapshot from May 2025. A fixed
# commit, so the hash checks what git fetches.
FROM ${BASE_IMAGE} AS passt
ARG DEBIAN_FRONTEND=noninteractive
ARG PASST_COMMIT=6aa4f88bd9846411edff3ed70460475b538addca
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates gcc git libc6-dev make && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN set -eux; \
    git clone --quiet https://passt.top/passt .; \
    git -c advice.detachedHead=false checkout --quiet "${PASST_COMMIT}"; \
    make; \
    mkdir -p /out/usr/local/bin; \
    # passt.avx2 and pasta.avx2 exist on x86_64 only; pasta is a link to passt
    for f in passt passt.avx2 pasta pasta.avx2; do if [ -e "$f" ]; then cp -d "$f" /out/usr/local/bin/; fi; done; \
    /out/usr/local/bin/pasta --version

# pause, kube-proxy, flannel and CoreDNS: every join needs them, and their
# downloads sometimes stall on Docker Desktop. The agent serves them to
# containerd from this OCI image layout (agent/imagemirror.go). The images come
# from the addon manifests of the control plane, so they always match:
#   docker build --build-context addons=k8s/tabernetes/addons node
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS images
ARG TARGETARCH
ARG KUBERNETES_VERSION
ARG CRANE_VERSION=v0.22.1
RUN CGO_ENABLED=0 go install "github.com/google/go-containerregistry/cmd/crane@${CRANE_VERSION}"
COPY --from=addons *.yaml /addons/
COPY --from=download /pause-image /pause-image
# One crane call per image in a fixed order, so index.json is the same on every build.
RUN set -eux; \
    sed -n 's/^ *image: *//p' /addons/*.yaml | sed "s/KUBERNETES_VERSION/${KUBERNETES_VERSION}/" | cat /pause-image - | sort -u > /images; \
    out=/out/usr/share/tabernetes/images; \
    while read -r image; do \
      crane pull --platform "linux/${TARGETARCH}" --format oci --annotate-ref "${image}" "${out}"; \
    done < /images; \
    cat "${out}/index.json"

FROM ${BASE_IMAGE}
# only while building, not in the image
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates iproute2 iptables mount util-linux \
 && rm -rf /var/lib/apt/lists/*
COPY --from=download /out/ /
COPY --from=passt /out/ /
COPY --from=images /out/ /
COPY --from=agent /tabernetes /usr/local/bin/tabernetes
# containerd calls runc through the agent when the node is not privileged
RUN ln -s tabernetes /usr/local/bin/tabernetes-runc
COPY crictl.yaml /etc/crictl.yaml
# containerd needs a real file system for its overlayfs snapshots
VOLUME ["/var/lib/containerd"]
ENTRYPOINT ["/usr/local/bin/tabernetes"]
