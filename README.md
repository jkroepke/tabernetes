# kwasm

A real Kubernetes control plane in the browser. kube-apiserver,
kube-controller-manager, kube-scheduler and kubectl are compiled with
`GOOS=js GOARCH=wasm` into a single binary. The page at
[/kubernetes/](https://jkroepke.de/kubernetes/) boots it in a terminal.
A real node can join with Docker. The node talks to the browser over WebRTC.

## How it works

```text
browser tab (single wasm binary)                     docker run kwasm-node
+---------------------------------------------+      +---------------------------+
| kubectl  --->  kube-apiserver :6443         |      | kubelet, kube-proxy, pods |
|                   |                         |      |        |                  |
| controller-manager, scheduler               |      |   127.0.0.1:6443          |
|                   |                         |      |   10.0.0.1:6443           |
|          etcd shim :2379 (in memory)       |      |        |                  |
|                                             |      |   kwasm-node agent        |
| <ip>:10250 ---------- WebRTC data channels -------------> kubelet :10250    |
| 127.0.0.1:6443 <----- (one per TCP stream) <------------- 127.0.0.1:6443    |
+---------------------------------------------+      +---------------------------+
```

- Go's `js/wasm` port has an in-process fake network. `net.Listen` and
  `net.Dial` work between goroutines of the same binary. All components
  talk to each other over it, without any change to their code.
- etcd is replaced by `k8s/kwasm/etcdshim`. It is a small in-memory
  implementation of the etcd v3 gRPC API (KV, Txn, Watch, Lease). It
  implements only what kube-apiserver uses. It persists nothing.
- The browser has no file system. `web/src/fs.js` is an in-memory file
  system with the Node.js callback API that Go's `syscall/js` expects.
- `join` creates a bootstrap token and a WebRTC offer. Signaling is copy and
  paste: the offer goes into the `docker run` command, and the answer goes
  back into the terminal. STUN is used for NAT traversal. The node agent
  also advertises `127.0.0.1` for the `-p 127.0.0.1:<port>:<port>/udp`
  mapping. That way it works on Docker Desktop, where the browser cannot
  reach container IPs.

## Patches

`k8s/patch.go` patches a Kubernetes checkout. It is plain Go, so it works the
same on macOS and Linux:

- Build-tag stubs for unix-only syscalls (journal, flock, termios, mount
  helpers, SO_LINGER, `syscall.Exec` for kubectl plugins, signals).
- ECDSA instead of RSA for self-signed certificates. RSA key generation takes
  ages in single-threaded Wasm.
- A sleep in the APF bootstrap ensurer. It busy-loops until the informer
  catches up, and Wasm has no preemption, so the informer never gets the CPU.
- An idempotent `SetupSignalContext`, because all components share one process.

Every content patch fails when its target is gone. A Kubernetes update shows
exactly what needs attention.

## Build

The tasks live in `kwasm/mise.toml`. mise installs Go and Node.js.

```bash
cd kwasm
mise run build       # static/kwasm/{kwasm.wasm.zst,kwasm.js,xterm.css}
mise run node-image  # ghcr.io/jkroepke/kwasm-node:<version> for the local platform
cd .. && hugo server # http://localhost:1313/kubernetes/
```

The binary is around 290 MB, or 28 MB with zstd (gzip: 45 MB). GitHub Pages
does not send `Content-Encoding` for it, and `DecompressionStream` has no
zstd. The page fetches the `.zst` file and unpacks it with
[fzstd](https://github.com/101arrowz/fzstd) in about 1.5 seconds.

## Update Kubernetes

1. Bump `KUBERNETES_VERSION` in `mise.toml`.
2. Run `mise run build`. When `patch.go` stops with "patch target not found",
   adapt that patch.
3. Run `mise run test` (etcd shim tests), then boot the page and run
   `join`.

The node image is built by `.github/workflows/kwasm-node.yaml` with the same
version.

## Limits

- Wasm runs on one thread. The boot takes about 15 seconds. Everything shares
  one CPU core.
- No persistence: a reload starts a new cluster.
- `kubectl exec`, `logs` and `port-forward` go through the WebRTC tunnel to
  the kubelet. The apiserver cannot reach pod or service IPs, so service
  proxy URLs do not work.
- The printed `docker run` works when Docker runs on the same machine as the
  browser. For Docker on another machine in the LAN, publish the port on all
  interfaces (`-p 50001:50001/udp` for the first node) and pass
  `--advertise <LAN IP>` before the token. The browser hides its own addresses
  behind mDNS names, and the container cannot resolve them, so the browser has
  to reach the container.
- Across different networks, WebRTC needs a port forward or TURN. There is no
  TURN server.
