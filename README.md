# Tabernetes

A real Kubernetes control plane in a browser tab. kube-apiserver,
kube-controller-manager, kube-scheduler and kubectl are compiled with
`GOOS=js GOARCH=wasm` into a single binary. The page at
[/tabernetes/](https://jkroepke.de/tabernetes/) boots it in a terminal.
A real node can join with Docker. The node talks to the browser over WebRTC.

## How it works

```text
browser tab (single wasm binary)                     docker run tabernetes
+---------------------------------------------+      +---------------------------+
| kubectl  --->  kube-apiserver :6443         |      | kubelet, kube-proxy, pods |
|                   |                         |      |        |                  |
| controller-manager, scheduler               |      |   127.0.0.1:6443          |
|                   |                         |      |   10.0.0.1:6443           |
|          etcd shim :2379 (in memory)       |      |        |                  |
|                                             |      |   tabernetes agent        |
| <ip>:10250 ---------- WebRTC data channels -------------> kubelet :10250    |
| 127.0.0.1:6443 <----- (one per TCP stream) <------------- 127.0.0.1:6443    |
+---------------------------------------------+      +---------------------------+
```

- Go's `js/wasm` port has an in-process fake network. `net.Listen` and
  `net.Dial` work between goroutines of the same binary. All components
  talk to each other over it, without any change to their code.
- etcd is replaced by `k8s/tabernetes/etcdshim`. It is a small in-memory
  implementation of the etcd v3 gRPC API (KV, Txn, Watch, Lease). It
  implements only what kube-apiserver uses. It persists nothing.
- The browser has no file system. `web/src/fs.js` is an in-memory file
  system with the Node.js callback API that Go's `syscall/js` expects.
- The terminal runs a real POSIX shell: [mvdan.cc/sh](https://github.com/mvdan/sh),
  a shell interpreter in Go, compiled into the same binary. Pipes, variables,
  loops, `$(...)`, redirects and here-documents work. There are no processes,
  so every command is a function: kubectl runs in-process, a few coreutils
  (`grep`, `head`, `wc`, `xargs`, ...) live in `k8s/tabernetes/coreutils.go`, and
  `join`, `logs` and `clear` are JavaScript. Files land in the in-memory file
  system, so `kubectl get cm x -o yaml > cm.yaml` works.
  Tab completes commands and files, and kubectl through its own `__complete`
  command, so resource types and names come from the cluster.
  `wget` downloads through the `fetch()` of the browser, so it only works for
  servers that allow CORS, for example `raw.githubusercontent.com`.
  `localhost` and `127.0.0.1` stay in the in-process network of the Wasm, so
  `k port-forward deploy/web 8080:80 &` and `wget -qO- localhost:8080` work.
- `history` lists the command lines of the tab, and Ctrl-R searches them
  backwards like Bash. The history ends with the tab.
- `cmd &` runs in the background, `jobs` lists the jobs, `kill %1` stops one.
  Ctrl-C stops the foreground command, `kubectl port-forward` and
  `kubectl proxy` included. On Windows and Linux, Ctrl-C copies instead while
  text is selected, and Ctrl-V pastes.
- `proxy 8080` opens `localhost:8080` of the Wasm in a new browser tab. A
  service worker under `/tabernetes-proxy/` (`static/tabernetes-proxy/sw.js`) hands
  every request of that tab to the terminal tab, which sends it to the
  in-process network. The tab works as long as the terminal tab is open;
  leaving the terminal page unregisters the worker. While
  `kubectl port-forward` runs, the status line under the terminal links each
  of its local ports the same way (`k8s/tabernetes/forwards.go`).
- `vnc <port>` shows a VNC server on `localhost` of the Wasm (a
  `kubectl port-forward`) on top of the terminal. [noVNC](https://github.com/novnc/noVNC)
  speaks RFB, and `tabernetes.dial` (`k8s/tabernetes/dial.go`) gives it a TCP
  connection into the in-process network. `doom` uses it: it starts
  [KubeDoom](https://github.com/storax/kubedoom) on the node with a Deployment
  of five pause pods (`k8s/tabernetes/doom/kubedoom.yaml`), and every pod of
  the default namespace is a monster. Shoot one, and KubeDoom deletes the pod.
- Every tab is a node. The control plane runs in a SharedWorker
  (`web/src/cluster.js`, `k8s/tabernetes/tabs.go`) that all tabs of the page
  share, and it lives as long as one of them is open: open a second tab, and
  it joins as `tab-2`; close the first one, and the cluster keeps running.
  The first tab hands its download to the worker, which compiles a second
  instance of the binary for the control plane. Each tab runs the binary for
  its shell, kubectl and kubelet, and every connection between a tab and the
  worker is a MessagePort of its own. A tab holds a Web Lock while it is open;
  the worker waits for that lock, so it learns when a tab closes, deletes its
  node and renews the node leases in between (timers of a hidden tab may fire
  only once a minute). WebRTC does not exist in workers: the data channels of
  a node from `join` stay in the tab that ran `join`, and go on to the worker
  over MessagePorts. When that tab closes, the node agent exits, and the
  worker deletes its node. Without SharedWorker or Web Locks (Chrome on Android),
  the tab runs the control plane itself, as before.
- `llm [model]` runs a language model on the GPU of the tab. The tab node
  (`k8s/tabernetes/tabnode.go`) has no
  container runtime; a small kubelet in the Wasm registers the node `tab-1`
  (lease, status, taint `tabernetes.dev/tab`, `kubernetes.io/os=browser`, so
  the DaemonSets stay away, and the extended resource `tabernetes.dev/webgpu`),
  runs the pods bound to it and serves logs, exec and port-forward on
  `10.0.1.1:10250` with the streaming code of the upstream kubelet
  (`k8s.io/cri-streaming`). An image `webllm:<model>` is a
  [WebLLM](https://webllm.mlc.ai) model ID (`k8s/tabernetes/webllm.go`): a
  worker (`web/src/gpuworker.js`) loads the weights from Hugging Face, which
  allows CORS, and keeps them in the Cache Storage. Container registries do
  not send CORS headers, so the tab cannot pull images. The pod serves the
  OpenAI chat API on port 8000, and `kubectl exec llm -- chat [question]`
  answers on the terminal. `llm` chats through `kubectl exec -i`; without a
  tty, it edits the lines itself and sends Ctrl-C as `\x03`. It needs WebGPU
  (Chrome or Edge); without `shader-f16` it takes the `q4f32` variant of the
  default model, Qwen2.5 1.5B.
- `free` shows the WebAssembly memory (wasm32 ends at 4 GiB, and the linear
  memory never shrinks), the Go heap and the number of goroutines.
- `vi` (`k8s/tabernetes/vi.go`) is a small vi for manifests. It runs in raw mode:
  while it runs, the terminal sends every key to it. `kubectl edit` opens it
  in-process, because js/Wasm cannot start an editor process.
- `join` creates a bootstrap token and a WebRTC offer. Signaling is copy and
  paste: the offer goes into the `docker run` command, and `join` then asks
  for the answer of the node (`accept <answer>` works later, too). A status
  line under the terminal shows every node and the state of its WebRTC
  connection. STUN is used for NAT traversal. The node agent
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
- mvdan.cc/sh is vendored into the checkout at `MVDAN_SH_VERSION` from
  `mise.toml`. Kubernetes already vendors its only dependencies. A hook in
  its interpreter gives background jobs their own context, so they outlive
  their command line.
- `kubectl port-forward` and `kubectl proxy` stop with their command
  context. Otherwise their listeners would stay until the tab closes.

Every content patch fails when its target is gone. A Kubernetes update shows
exactly what needs attention.

## Build

The tasks live in `tabernetes/mise.toml`. mise installs Go and Node.js.

```bash
cd tabernetes
mise run build       # assets/tabernetes/{tabernetes.wasm.zst,tabernetes.js,unzstd.js,xterm.css}
mise run node-image  # ghcr.io/jkroepke/tabernetes:latest for the local platform
cd .. && hugo server # http://localhost:1313/tabernetes/
```

The binary is around 290 MB, or 28 MB with zstd (gzip: 45 MB). GitHub Pages
does not send `Content-Encoding` for it, and `DecompressionStream` has no
zstd. The page fetches the `.zst` file and unpacks it with
[fzstd](https://github.com/101arrowz/fzstd) in a Web Worker, block by block
while it downloads. The browser compiles the WebAssembly while the bytes
arrive, so download, unpacking and compiling overlap.

Hugo fingerprints the files in `assets/tabernetes/`, so the name of the binary
changes with its content. The page keeps the binary in the Cache Storage of
the browser and the next visit downloads nothing. GitHub Pages itself sends
only `max-age=600`.

## Update Kubernetes

1. Bump `KUBERNETES_VERSION` in `mise.toml`.
2. Run `mise run build`. When `patch.go` stops with "patch target not found",
   adapt that patch.
3. Run `mise run test` (etcd shim tests), then boot the page and run
   `join`.

The node image is built by `.github/workflows/tabernetes-image.yaml` with the same
version and pushed as `latest` only. The printed `docker run` uses
`--pull always`, so a join always runs the current image.

The node image also brings the images that every join needs: pause,
kube-proxy, flannel and CoreDNS. The `images` stage of `node/Dockerfile` takes
the list from the addon manifests in `k8s/tabernetes/addons/` and the pause
image from containerd, so an update needs no extra step. The agent serves them
to containerd from a read-only registry on 127.0.0.1
(`node/agent/imagemirror.go`); every other image comes from its registry as
before. Downloads from the registries stalled now and then on Docker Desktop,
and the join waited for the retries.

## Node without --privileged

The node container runs a kubelet, containerd and runc, but it needs neither
`--privileged` nor extra capabilities. The printed `docker run` uses:

| Flag | Why |
| --- | --- |
| `--pull always` | the image has only the `latest` tag; this fetches the current one instead of a cached old one |
| `--memory 2g --pids-limit 4096` | the node evicts and garbage collects almost never, so the limits keep a runaway pod from taking down the host |
| `--security-opt seccomp=unconfined` | Docker's profile allows a new user namespace only with `CAP_SYS_ADMIN` |
| `--security-opt systempaths=unconfined` | the kernel refuses a new `sysfs` and `cgroup2` mount while Docker masks parts of `/proc` and `/sys` |
| `--device /dev/net/tun` | pasta connects the node's network namespace through a tap device |
| `--security-opt apparmor=unconfined` | the `docker-default` AppArmor profile denies `mount` (Linux hosts with AppArmor, untested) |

Without `CAP_SYS_ADMIN`, the agent starts itself again in new user, mount,
network, cgroup, UTS and IPC namespaces (`agent/userns.go`). There it is root
with all capabilities, but only over these namespaces:

- uid and gid 0-65535 map to themselves, so files keep their owners.
- pasta copies the container's address into the new network namespace, NATs
  outgoing traffic and forwards the WebRTC UDP port. WebRTC, flannel and
  kube-proxy work as before.
- A new `cgroup2` mount gives the kubelet a writable cgroup tree.
- The kubelet runs with the feature gate `KubeletInUserNamespace`.
  containerd runs with `restrict_oom_score_adj` and without AppArmor.
- Docker's `/sys` is read-only, and in a user namespace the kernel allows only
  read-only `sysfs` mounts. containerd calls runc through the agent
  (`agent/runc.go`), which marks the `sysfs` mounts of containers read-only.
- `/dev/kmsg` links to `/dev/null`, so the kubelet's OOM watcher starts. OOM
  kills still show up as `OOMKilled` in the pod status.

Alternative that keeps Docker's seccomp profile: capabilities instead of a user
namespace.

```text
--cap-add SYS_ADMIN --cap-add NET_ADMIN --cap-add SYS_RESOURCE --cap-add SYS_PTRACE --security-opt apparmor=unconfined
```

Then the agent remounts `/sys/fs/cgroup` and `/proc/sys` read-write and calls
runc with `--no-new-keyring --no-pivot`, because the profile blocks `keyctl`
and `pivot_root`. `CAP_SYS_ADMIN` is close to root, though. `--privileged`
still works, too. Real isolation from the host needs rootless Docker or Podman.

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
