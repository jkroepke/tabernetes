# kwasm-node: a Kubernetes node that joins a control plane running in a browser.
ARG GO_IMAGE=mirror.gcr.io/library/golang:1.27-trixie
ARG BASE_IMAGE=mirror.gcr.io/library/debian:trixie-slim

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS agent
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY agent/go.mod agent/go.sum ./
RUN go mod download
COPY agent/ ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /kwasm-node .

FROM ${BASE_IMAGE} AS download
ARG TARGETARCH
ARG KUBERNETES_VERSION=v1.37.2
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
    chmod +x usr/local/bin/*

FROM ${BASE_IMAGE}
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates iproute2 iptables mount util-linux passt \
 && rm -rf /var/lib/apt/lists/*
COPY --from=download /out/ /
COPY --from=agent /kwasm-node /usr/local/bin/kwasm-node
# containerd calls runc through the agent when the node is not privileged
RUN ln -s kwasm-node /usr/local/bin/kwasm-runc
COPY crictl.yaml /etc/crictl.yaml
# containerd needs a real file system for its overlayfs snapshots
VOLUME ["/var/lib/containerd"]
ENTRYPOINT ["/usr/local/bin/kwasm-node"]
