# kwasm-node: a Kubernetes node that joins a control plane running in a browser.
ARG GO_IMAGE=mirror.gcr.io/library/golang:1.26-trixie
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
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
WORKDIR /out
RUN set -eux; \
    mkdir -p usr/local/bin opt/cni/bin; \
    curl -fsSLo usr/local/bin/kubelet "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/${TARGETARCH}/kubelet"; \
    curl -fsSL "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz" \
      | tar -xz -C usr/local bin/containerd bin/containerd-shim-runc-v2 bin/ctr; \
    curl -fsSLo usr/local/bin/runc "https://github.com/opencontainers/runc/releases/download/${RUNC_VERSION}/runc.${TARGETARCH}"; \
    curl -fsSL "https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${TARGETARCH}-${CNI_PLUGINS_VERSION}.tgz" \
      | tar -xz -C opt/cni/bin ./bridge ./host-local ./loopback ./portmap ./ptp; \
    curl -fsSL "https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-${TARGETARCH}.tar.gz" \
      | tar -xz -C usr/local/bin crictl; \
    chmod +x usr/local/bin/*

FROM ${BASE_IMAGE}
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates iproute2 iptables conntrack ethtool socat mount util-linux kmod \
 && rm -rf /var/lib/apt/lists/*
COPY --from=download /out/ /
COPY --from=agent /kwasm-node /usr/local/bin/kwasm-node
COPY crictl.yaml /etc/crictl.yaml
# containerd needs a real file system for its overlayfs snapshots
VOLUME ["/var/lib/containerd"]
ENTRYPOINT ["/usr/local/bin/kwasm-node"]
