// The worker of a pod on a tab node that runs a container image (vm.js,
// k8s/tabernetes/v86.go). It pulls the linux/386 variant of the image from
// the registry mirror, unpacks the layers into the image store of the
// browser (layers.js) and boots v86, an x86 emulator that compiles x86 code
// to WebAssembly, with the merged layers as its root file system over 9p
// (rootfs.js). The files stay outside the memory of the VM.
//
// The VM boots while the image is pulled. init waits in an initramfs for the
// pod (vm/init/init.c): the first VM of each memory size saves its state
// there, and later ones restore it instead of booting (vmstate.js).
//
// Docker Hub sends no CORS headers, so the page cannot pull from it. The
// mirror is a pull-through cache of Docker Hub that does. The unpacked
// layers stay in the browser: a second pull finds the image "already present
// on machine", in every tab and after a reload.
import { V86 } from "v86";
import { Tree, LayerStorage, modes, statRoot } from "./rootfs.js";
import { openStore, imageFSCapacity } from "./layers.js";
import { SHA256 } from "./sha256.js";
import { initramfs } from "./initramfs.js";
import { bootKernel } from "./kernelboot.js";
import { buildID, loadState, saveState, dropState, parseState } from "./vmstate.js";
import { importJIT } from "./jit.js";

const post = (m, transfer) => self.postMessage(m, transfer || []);
const CACHE = "tabernetes-configs"; // the configs of the images, small JSON blobs
// The layers were here before they moved to the image store.
caches.delete("tabernetes-images").catch(() => {});
const PLATFORM = { os: "linux", architecture: "386" };
const INDEX_TYPES = ["application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"];
const MANIFEST_TYPES = ["application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"];
// Layers that download at once, the default of containerd.
const PULL_PARALLEL = 3;
// A layer up to this size (compressed) is kept in memory while it streams
// in and hashed by crypto.subtle at the end, which is native and runs off
// the thread of the worker. Larger layers are hashed as they stream by, in
// JavaScript (sha256.js). The JavaScript hash took ~400 ms of the ~1.3 s
// pull of nginx (64 MB in 7 layers) on the only thread that also unpacks.
const SUBTLE_HASH_MAX = 64 * 2 ** 20;

// MiB, of the VM of a pod: its memory limit, or 256 MiB without one. The
// files of the image are not in it, only what the container runs and writes
// to tmpfs, plus the page cache. Every pod has a worker and a wasm memory of
// its own, but v86 gives a VM at most 2 GiB (less 128 KiB). The kernel uses
// all of it (highmem, vm/kernel.config), not only the first 896 MiB.
const MIN_MEMORY = 128, DEFAULT_MEMORY = 256, MAX_MEMORY = 2048;

class PullError extends Error {}
// The image is not there and the pull policy is Never.
class NeverPullError extends PullError {}

// parseRef normalizes an image reference as Docker does: nginx is
// docker.io/library/nginx:latest.
export function parseRef(image) {
  let rest = image, domain = "docker.io";
  const slash = rest.indexOf("/");
  if (slash > 0) {
    const first = rest.slice(0, slash);
    if (first.includes(".") || first.includes(":") || first === "localhost") {
      domain = first;
      rest = rest.slice(slash + 1);
    }
  }
  let digest = "";
  const at = rest.indexOf("@");
  if (at >= 0) { digest = rest.slice(at + 1); rest = rest.slice(0, at); }
  let tag = "";
  const colon = rest.lastIndexOf(":");
  if (colon > rest.lastIndexOf("/")) { tag = rest.slice(colon + 1); rest = rest.slice(0, colon); }
  if (domain === "docker.io" && !rest.includes("/")) rest = "library/" + rest;
  if (domain === "index.docker.io" || domain === "registry-1.docker.io") domain = "docker.io";
  if (!tag && !digest) tag = "latest";
  const name = `${domain}/${rest}` + (tag ? `:${tag}` : "") + (digest ? `@${digest}` : "");
  return { domain, repo: rest, reference: digest || tag, name };
}

const hex = (buf) => [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");

async function sha256(bytes) {
  return "sha256:" + hex(await crypto.subtle.digest("SHA-256", bytes));
}

// Pull fetches manifests and blobs, the config through the Cache Storage.
class Pull {
  constructor(registry, ref) {
    this.registry = registry;
    this.base = registry.replace(/\/+$/, "") + "/v2/" + ref.repo;
    this.ref = ref;
    this.fetched = 0; // bytes that came over the network
    this.cache = caches.open(CACHE).catch(() => null);
  }

  async manifest(reference, types) {
    const r = await fetch(`${this.base}/manifests/${reference}`, { headers: { Accept: types.join(",") } }).catch((e) => {
      throw new PullError(`failed to resolve reference "${this.ref.name}": ${e.message} (registry mirror unreachable)`);
    });
    if (r.status === 404) throw new PullError(`failed to resolve reference "${this.ref.name}": ${this.ref.name}: not found`);
    if (!r.ok) throw new PullError(`failed to resolve reference "${this.ref.name}": unexpected status from GET request to ${r.url}: ${r.status} ${r.statusText}`);
    const body = await r.arrayBuffer();
    return { digest: r.headers.get("Docker-Content-Digest") || (await sha256(body)), json: JSON.parse(new TextDecoder().decode(body)) };
  }

  // cached tells whether the blob is in the Cache Storage.
  async cached(digest) {
    const c = await this.cache;
    return !!(c && (await c.match(this.blobURL(digest))));
  }

  // resolved returns what the last pull of this reference resolved it to:
  // the image ID and the manifest of the linux/386 variant, or null.
  async resolved() {
    const c = await this.cache;
    const hit = c && (await c.match(this.refURL()).catch(() => null));
    return hit ? hit.json().catch(() => null) : null;
  }

  async remember(imageID, manifest) {
    const c = await this.cache;
    if (c) await c.put(this.refURL(), new Response(JSON.stringify({ imageID, manifest }))).catch(() => {});
  }

  refURL() {
    return new URL(`/tabernetes-refs/${encodeURIComponent(this.ref.name)}`, self.location.href).href;
  }

  blobURL(digest) {
    // registry-independent, so that a new mirror finds the old blobs
    return new URL(`/tabernetes-blobs/${digest}`, self.location.href).href;
  }

  // layer streams the blob of a layer and hashes it on the way. verify()
  // throws once the stream ended when the digest is wrong. size is the one
  // of the descriptor in the manifest.
  layer(digest, size) {
    // in memory and crypto.subtle, or streamed through sha256.js
    let buf = crypto.subtle && size > 0 && size <= SUBTLE_HASH_MAX ? new Uint8Array(size) : null;
    let hash = buf ? null : new SHA256();
    let at = 0;
    let got = "";
    const fetchLayer = async () => {
      const r = await fetch(`${this.base}/blobs/${digest}`).catch((e) => {
        throw new PullError(`failed to copy: httpReadSeeker: failed open: ${e.message} (registry mirror unreachable)`);
      });
      if (!r.ok) throw new PullError(`failed to copy: httpReadSeeker: failed open: unexpected status code ${r.url}: ${r.status} ${r.statusText}`);
      // A broken connection fails the stream with a PullError, so that the
      // pull does not report it as an unpack error.
      const body = r.body.getReader();
      return new ReadableStream({
        pull: async (c) => {
          let chunk;
          try {
            chunk = await body.read();
          } catch (e) {
            throw new PullError(`failed to copy: read ${digest}: ${e.message}`);
          }
          if (chunk.done) {
            got = "sha256:" + (buf ? hex(await crypto.subtle.digest("SHA-256", buf.subarray(0, at))) : hash.hex());
            buf = null;
            c.close();
            return;
          }
          if (buf && at + chunk.value.length > buf.length) {
            // more than the descriptor says: the digest cannot match, but
            // the error should be the one of a wrong digest
            hash = new SHA256().update(buf.subarray(0, at));
            buf = null;
          }
          if (buf) buf.set(chunk.value, at);
          else hash.update(chunk.value);
          at += chunk.value.length;
          this.fetched += chunk.value.length;
          c.enqueue(chunk.value);
        },
        cancel: (why) => body.cancel(why),
      });
    };
    const verify = () => {
      if (got !== digest) throw new PullError(`failed commit on ref "layer-${digest}": unexpected commit digest ${got}, expected ${digest}`);
    };
    return { fetch: fetchLayer, verify };
  }

  // blob fetches a small blob (the config) through the Cache Storage.
  async blob(digest) {
    const c = await this.cache;
    const hit = c && (await c.match(this.blobURL(digest)));
    if (hit) return new Uint8Array(await hit.arrayBuffer());
    const r = await fetch(`${this.base}/blobs/${digest}`);
    if (!r.ok) throw new PullError(`failed to copy: httpReadSeeker: failed open: unexpected status code ${r.url}: ${r.status} ${r.statusText}`);
    const body = new Uint8Array(await r.arrayBuffer());
    this.fetched += body.length;
    if ((await sha256(body)) !== digest) throw new PullError(`failed commit on ref "layer-${digest}": unexpected commit digest, expected ${digest}`);
    if (c) await c.put(this.blobURL(digest), new Response(body)).catch(() => {});
    return body;
  }
}

let store; // the image store (layers.js), once per worker
// pullImage pulls an image, or finds it present. policy is the
// imagePullPolicy of the container: with IfNotPresent and Never, an image
// that a pull of the same reference left complete is used without asking
// the registry, as the kubelet does.
// on.fetching() is called when something of the image is missing, and
// on.layers() once the layers download (or when there is nothing to
// download).
async function pullImage(registry, image, policy, on) {
  const ref = parseRef(image);
  if (ref.domain !== "docker.io") {
    throw new PullError(`failed to resolve reference "${ref.name}": the tab node pulls from Docker Hub only (through the registry mirror of this page)`);
  }
  const p = new Pull(registry, ref);
  // the image store and the last resolution of the reference, at once
  const resolved = policy !== "Always" ? p.resolved() : null;
  store ??= await openStore();
  if (policy !== "Always") {
    const hit = await resolved;
    const img = hit?.manifest?.layers && (await presentImage(p, ref, hit.imageID, hit.manifest));
    if (img) return img;
    if (policy === "Never") throw new NeverPullError(`Container image "${image}" is not present with pull policy of Never`);
  }
  return pullFromRegistry(p, ref, on);
}

// pullFromRegistry resolves the reference at the registry and pulls what is
// missing of the image.
async function pullFromRegistry(p, ref, on) {
  let { digest, json: m } = await p.manifest(ref.reference, [...INDEX_TYPES, ...MANIFEST_TYPES]);
  const imageID = `${ref.domain}/${ref.repo}@${digest}`;
  let matched = false; // the manifest of linux/386 in an index
  if (m.manifests) {
    const d = m.manifests.find((x) => x.platform?.os === PLATFORM.os && x.platform?.architecture === PLATFORM.architecture);
    if (!d) throw new PullError(`failed to pull and unpack image "${ref.name}": no match for platform in manifest: not found (the tab node runs linux/386 images, ${ref.name} has ${[...new Set(m.manifests.map((x) => x.platform && `${x.platform.os}/${x.platform.architecture}`).filter((x) => x && !x.startsWith("unknown")))].join(", ")})`);
    m = (await p.manifest(d.digest, MANIFEST_TYPES)).json;
    matched = true;
  } else if (m.config) {
    // a single-platform image: it must be linux/386 itself
  } else {
    throw new PullError(`failed to pull and unpack image "${ref.name}": unsupported manifest`);
  }
  const img = await unpackImage(p, ref, imageID, m, { ...on, matched });
  await p.remember(imageID, m);
  return img;
}

// presentImage returns the image if all of it is in the browser, else null.
const presentImage = (p, ref, imageID, m) => unpackImage(p, ref, imageID, m);

// unpackImage fetches and unpacks what is missing of the image with the
// manifest m. Without on.fetching, it returns null if something is missing.
// matched tells that an index chose m for linux/386.
async function unpackImage(p, ref, imageID, m, { fetching, layers: onLayers, matched = false } = {}) {
  const size = m.layers.reduce((n, l) => n + (l.size || 0), 0) + (m.config.size || 0);
  await Promise.all(m.layers.map((l) => store.use(l.digest)));
  const indexes = await Promise.all(m.layers.map((l) => store.index(l.digest)));
  const present = (await p.cached(m.config.digest)) && indexes.every(Boolean);
  // The layer workers start while the config comes (~50 ms each). Not
  // for an image that is present: kubectl run of a :latest image asks the
  // registry on every start, and the workers would only cost CPU.
  let pool = null;
  if (!present) {
    if (!fetching) return null;
    fetching();
    pool = Array.from({ length: Math.min(PULL_PARALLEL, indexes.filter((x) => !x).length) }, layerWorker);
  }
  const config = p.blob(m.config.digest).then((b) => {
    const c = JSON.parse(new TextDecoder().decode(b));
    if (c.architecture && c.architecture !== PLATFORM.architecture) {
      throw new PullError(`failed to pull and unpack image "${ref.name}": no match for platform in manifest: not found (the tab node runs linux/386 images, this one is ${c.os}/${c.architecture})`);
    }
    return c;
  });
  config.catch(() => {});
  // A manifest that an index chose for linux/386 has its layers for it:
  // they download while the config comes, a round trip to the mirror less.
  // Else the config must tell first.
  if (!matched) await config;
  const missing = m.layers.map((l, i) => i).filter((i) => !indexes[i]);
  const n = Math.min(PULL_PARALLEL, missing.length);
  const workers = pool ? pool.slice(0, n) : [];
  for (const w of pool?.slice(n) ?? []) w?.terminate();
  // Up to PULL_PARALLEL layers download and unpack at once, as containerd
  // does (max_concurrent_downloads = 3). It saves the round trips and the
  // time the mirror needs to fetch a blob from Docker Hub. The order of the
  // layers matters only for the root file system, not for the download.
  // Each of them unpacks in a layer worker of its own (see layerWorker).
  let next = 0, failed = false;
  try {
    const all = Promise.all([config.catch((e) => { failed = true; throw e; }), ...workers.map(async (w) => {
      while (!failed && next < missing.length) {
        const i = missing[next++];
        try {
          indexes[i] = await (w && !w.broken ? inWorker(w, p, m.layers[i]) : unpackLayer(p, m.layers[i]));
        } catch (e) {
          failed = true; // start no more layers, the pull fails anyway
          throw e;
        }
      }
    })]);
    onLayers?.();
    await all;
  } finally {
    for (const w of workers) w?.terminate();
  }
  const layers = m.layers.map((l, i) => ({ id: l.digest, entries: indexes[i].entries }));
  return { present, size, imageID, config: (await config).config || {}, layers };
}

// unpackLayer fetches a layer, checks its digest and unpacks it into the
// image store.
async function unpackLayer(p, l) {
  const { fetch, verify } = p.layer(l.digest, l.size);
  try {
    return await store.add(l.digest, fetch, verify, l.mediaType);
  } catch (e) {
    if (e instanceof PullError) throw e;
    const why = e?.name === "QuotaExceededError" ? "no space left on device" : e?.message || String(e);
    throw new PullError(`failed to extract layer ${l.digest}: write /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs: ${why}`);
  }
}

// A layer worker is a worker of the worker of the pod (this script again)
// that unpacks layers into the image store. The browser unpacks gzip on the
// thread that reads the stream, and hashing (crypto.subtle), writing to OPFS
// and the tar index run there too: with all layers in the worker of the pod,
// the two large layers of nginx shared one thread, and the VM of the pod
// waited for it. A pull of nginx from a local mirror took ~1.1 s of that
// thread. Without workers in workers, or without OPFS (the layers must stay
// in the memory of this worker), the layers unpack here as before.
function layerWorker() {
  if (!store.persistent || typeof Worker !== "function") return null;
  try {
    return new Worker(self.location.href, { name: "layer" });
  } catch {
    return null;
  }
}

// inWorker unpacks the layer l in the layer worker w, or here if w fails
// (a browser that does not start it, for example).
function inWorker(w, p, l) {
  return new Promise((resolve, reject) => {
    w.onmessage = ({ data }) => (data.error != null ? reject(new PullError(data.error)) : resolve(data.index));
    w.onerror = (e) => {
      e.preventDefault?.();
      w.onerror = w.onmessage = null;
      w.broken = true;
      unpackLayer(p, l).then(resolve, reject);
    };
    w.postMessage({ type: "layer", registry: p.registry, image: p.ref.name, layer: { digest: l.digest, size: l.size, mediaType: l.mediaType } });
  });
}

// layerJob is the side of the layer worker.
async function layerJob(m) {
  try {
    store ??= await openStore();
    post({ type: "layer", index: await unpackLayer(new Pull(m.registry, parseRef(m.image)), m.layer) });
  } catch (e) {
    post({ type: "layer", error: e instanceof PullError ? e.message : `failed to pull and unpack image "${m.image}": ${e?.message || e}` });
  }
}

// containerSpec merges the image config with the container of the pod, as
// the kubelet does: command replaces the entrypoint, args the cmd.
function containerSpec(img, c) {
  const entrypoint = c.command?.length ? c.command : img.Entrypoint || [];
  const cmd = c.command?.length ? c.args || [] : c.args?.length ? c.args : img.Cmd || [];
  const env = [...(img.Env || [])];
  for (const e of c.env || []) {
    const i = env.findIndex((x) => x.startsWith(e.split("=")[0] + "="));
    if (i >= 0) env[i] = e;
    else env.push(e);
  }
  return { args: [...entrypoint, ...cmd], env, cwd: c.workingDir || img.WorkingDir || "/", user: img.User || "" };
}

let emulator;
const conns = new Map();

// The lines that init writes to the second serial port (vm/init).
let onControl = () => {};

const fetchBytes = async (url) => new Uint8Array(await (await fetch(url)).arrayBuffer());

// tsc=reliable: v86 has no second clock to check the TSC against, and
// without the check the kernel never stops its tick in an idle pod
// (vm/kernel.config). highres=off: timers keep the resolution of the tick
// (10 ms). With high resolution timers the short sleeps of the Go runtime
// are real: a Go program ran 10 % more instructions (measured with
// access-log-exporter, start and 50 scrapes).
const KERNEL_CMDLINE = "console=ttyS0 quiet loglevel=3 tsc=reliable highres=off";

// shippedState fetches the state that the build ships (vmstate.js), or
// returns null: none, another memory size or another build.
async function shippedState(m, memory, build) {
  if (!m.stateURL) return null;
  try {
    const res = await fetch(m.stateURL);
    const s = res.ok ? parseState(await res.arrayBuffer()) : null;
    return s && s.memory === memory && (!build || s.build === build) ? s : null;
  } catch {
    return null;
  }
}

// bootVM starts the VM of the pod, without its file system: from a saved
// state, from the state of the build, or by booting the kernel. It resolves
// once init waits for the pod. A restore waits for the promise gate.
async function bootVM(m, memory, gate = null) {
  const base = {
    wasm_path: m.wasmURL,
    // the tab compiled v86.wasm already (postStart in vm.js)
    ...(m.wasmModule ? { wasm_fn: (env) => WebAssembly.instantiate(m.wasmModule, env).then((i) => i.exports) } : {}),
    // No BIOS: the page starts the kernel itself (kernelboot.js).
    // an empty 9p file system; the tree of the pod comes in before "go"
    filesystem: {},
    memory_size: memory,
    uart1: true,
    autostart: false,
    disable_keyboard: true,
    disable_mouse: true,
    disable_speaker: true,
    // The local APIC: its timer lets an idle kernel sleep for seconds. The
    // kernel has no ACPI; it finds the APIC by CPUID (vm/kernel.config).
    acpi: true,
    // The pod has no screen. v86 saves all VGA memory in the state: the
    // smallest size (256 KiB) instead of the default 8 MiB halves every
    // saved state (15.8 -> 7.6 MiB with 256 MiB of RAM).
    vga_memory_size: 256 << 10,
    // the network adapter must learn the MAC of a restored VM
    preserve_mac_from_state_image: true,
    net_device: { type: "virtio", relay_url: "fetch", vm_ip: m.ip, router_ip: m.gateway },
  };
  // Most pods restore a state: v86 compiles its wasm while the state loads.
  const loaded = (v) => new Promise((r) => v.add_listener("emulator-loaded", r));
  let e = new V86(base);
  let ok = loaded(e);
  // The kernel and init are needed for a boot, and for the ID of the saved
  // states, which vm.js keeps once a pod reported it: a pod that restores a
  // state then fetches and hashes neither (4.7 MB).
  let files;
  const fetchFiles = () => (files ??= Promise.all([fetchBytes(m.kernelURL), fetchBytes(m.initURL)]));
  const idOf = async () => {
    const [kernel, init] = await fetchFiles();
    const id = await buildID(m.version || "", kernel, init).catch(() => "");
    if (id) post({ type: "build", build: id });
    return id;
  };
  // Without the ID, jit.bin or the state of the build names it: a pod then
  // fetches neither the kernel nor init (4.7 MB) to hash them.
  let shipped = m.build || m.jit?.build ? null : await shippedState(m, memory, "");
  const build = m.build || m.jit?.build || shipped?.build || (await idOf());
  if (!m.build && build) post({ type: "build", build });
  let state = build && (await loadState(build, memory));
  if (!state) {
    shipped ??= await shippedState(m, memory, build);
    state = shipped?.state;
  }
  if (state) {
    await ok;
    await gate;
    try {
      // The state of the build stays compressed: v86 unpacks it at every
      // restore (~30 ms in Node.js). A spare worker that saved it unpacked
      // to OPFS made the next pod 50-70 ms slower (headless Chromium).
      await e.restore_state(state);
      // the JIT modules of the kernel that the tab compiled (jit.js)
      importJIT(e, m.jit, build);
      return { emulator: e };
    } catch (err) {
      console.warn("tabernetes: the saved state of the VM does not restore, booting:", err);
      if (!shipped) dropState(build, memory).catch(() => {});
      e.destroy();
      e = new V86(base);
      ok = loaded(e);
    }
  }
  // No state: the kernel starts in this VM, which has run nothing yet. The
  // ID that vm.js knew, or that the state of the build named, may be of
  // another kernel (a new release, a state that did not restore).
  const [kernel, init] = await fetchFiles();
  const saveAs = m.build || shipped ? await idOf() : build;
  emulator = e;
  await ok;
  bootKernel(e.v86.cpu, { kernel, initrd: initramfs(init), cmdline: KERNEL_CMDLINE });
  const ready = new Promise((resolve, reject) => {
    let error = "";
    onControl = (l) => {
      if (l === "ready") resolve();
      else if (l.startsWith("error ")) error = l.slice(6);
      else if (l.startsWith("exit ")) reject(new Error(`the VM did not boot: ${error || l}`));
    };
  });
  e.add_listener("serial1-output-byte", control);
  e.run();
  await ready;
  if (saveAs && (await opfs())) {
    // init waits for the line, nothing else runs: save the VM for the next pods
    await e.stop();
    const saved = await e.save_state().catch(() => null);
    e.run();
    if (saved) saveState(saveAs, memory, saved).catch((err) => console.warn("tabernetes: save the state of the VM:", err));
  }
  // the kernel has its code in place now (the saved state has no JIT code)
  importJIT(e, m.jit, saveAs);
  return { emulator: e };
}

const opfs = () => navigator.storage?.getDirectory?.().then(() => true, () => false) ?? Promise.resolve(false);

let line = "";
function control(b) {
  if (b === 13) return; // the tty sends \r\n
  if (b !== 10) { line += String.fromCharCode(b); return; }
  const l = line;
  line = "";
  onControl(l);
}

const memoryOf = (m) => Math.min(MAX_MEMORY, Math.max(MIN_MEMORY, m.memory ? Math.ceil(m.memory / 2 ** 20) : DEFAULT_MEMORY)) * 2 ** 20;

// A spare worker (vm.js) gets "warm" before there is a pod for it: it
// restores (or boots) the VM with the default memory, which then waits for
// the address and the root file system of the next pod, and opens the image
// store.
let warm = null; // {memory, vm}
function warmUp(m) {
  const memory = memoryOf(m);
  warm = { memory, vm: bootVM(m, memory) };
  warm.vm.catch(() => {});
  openStore().then((s) => { store ??= s; }, () => {});
}

// address gives a VM that booted without a pod the address of the pod and
// its gateway: the network adapter of v86 answers ARP and TCP with them.
function address(e, m) {
  const net = e.network_adapter;
  const ip4 = (s) => new Uint8Array(s.split(".").map(Number));
  if (net && m.ip) net.vm_ip = ip4(m.ip);
  if (net && m.gateway) net.router_ip = ip4(m.gateway);
}

async function start(m) {
  const t0 = Date.now();
  const memory = memoryOf(m);
  let go;
  const layersDownload = new Promise((resolve) => (go = resolve));
  // The VM of a spare worker, or one that boots while the image is pulled.
  let vm;
  if (warm?.memory === memory) {
    vm = warm.vm.then((v) => (address(v.emulator, m), v), () => bootVM(m, memory));
  } else {
    warm?.vm.then((v) => v.emulator.destroy(), () => {});
    // Restoring the VM takes the thread for ~60 ms: not before the layers
    // download, else the manifests of a pull wait for it.
    vm = bootVM(m, memory, layersDownload);
  }
  warm = null;
  vm.catch(() => {});
  let img;
  try {
    img = await pullImage(m.registry, m.image, m.pullPolicy, { fetching: () => post({ type: "pulling" }), layers: go });
  } catch (e) {
    vm.then((v) => v.emulator.destroy(), () => {});
    go();
    post({ type: "error", pull: true, never: e instanceof NeverPullError, message: e instanceof PullError ? e.message : `failed to pull and unpack image "${m.image}": ${e.message}` });
    return;
  }
  go();
  post({ type: "pulled", present: img.present, size: img.size, imageID: img.imageID, ms: Date.now() - t0 });
  // The layers of this pod are in use now; a new image may need room.
  if (!img.present) store.gc().catch((e) => console.warn("tabernetes: image garbage collection:", e));

  const spec = containerSpec(img.config, m.container);
  if (!spec.args.length) {
    vm.then((v) => v.emulator.destroy(), () => {});
    post({ type: "exit", code: 128, message: "no command specified" });
    return;
  }
  const c = m.container;
  const conf = { ...spec, hostname: m.hostname, ip: m.ip, gateway: m.gateway, stdin: !!c.stdin, stdinOnce: !!c.stdinOnce, tty: !!c.tty };
  // The readers of the layers open while the tree is built: each takes
  // a few file system calls (OPFS).
  const opened = Promise.all(img.layers.map(async (l) => [l.id, await store.layer(l.id)]));
  // The root file system: the layers, and the config of the pod on top. The
  // mount points must be there for init.
  const tree = new Tree();
  img.layers.forEach((l, i) => {
    tree.layer = i;
    tree.addLayer(l.id, l.entries);
  });
  tree.layer = img.layers.length;
  const now = Math.floor(Date.now() / 1000);
  const mem = new Map();
  const enc = new TextEncoder();
  const extra = {
    ".tabernetes/config.json": { mode: modes.S_IFREG | 0o644, data: enc.encode(JSON.stringify(conf)) },
    // what the kubelet mounts into every container. init wrote them over
    // 9p, which took 0.3 million instructions of every start.
    "etc/hosts": { mode: modes.S_IFREG | 0o644, data: enc.encode(`# Kubernetes-managed hosts file.\n127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n${m.ip ? `${m.ip}\t${m.hostname || ""}\n` : ""}`) },
  };
  if (m.gateway) extra["etc/resolv.conf"] = { mode: modes.S_IFREG | 0o644, data: enc.encode(`nameserver ${m.gateway}\n`) };
  for (const [path, e] of Object.entries(extra)) {
    const key = `mem:${mem.size}`;
    mem.set(key, e.data);
    tree.set(path, { uid: 0, gid: 0, mtime: now, mode: e.mode, size: e.data.length, key });
  }
  for (const d of ["dev", "proc", "sys", "tmp"]) {
    if (!tree.files.has(d)) tree.set(d, { uid: 0, gid: 0, mtime: now, mode: modes.S_IFDIR | (d === "tmp" ? 0o1777 : 0o755) });
  }
  const layers = new Map(await opened);
  const fsJSON = tree.v86();
  img.layers = null;

  ({ emulator } = await vm);
  // The console is stdout and stderr of the container. Batch the bytes.
  // A character of several bytes may be split between two batches.
  const utf8 = new TextDecoder();
  let out = [], timer = 0;
  const flush = () => {
    clearTimeout(timer);
    timer = 0;
    const data = utf8.decode(new Uint8Array(out), { stream: true });
    out = [];
    if (data) post({ type: "console", data });
  };
  emulator.add_listener("serial0-output-byte", (b) => {
    out.push(b);
    if (out.length >= 4096) flush();
    else if (!timer) timer = setTimeout(flush, 20);
  });
  // The init of the pod (vm/init) reports on the second serial port.
  let error = "";
  onControl = (l) => {
    if (l === "started") post({ type: "started", memory });
    else if (l.startsWith("error ")) error = l.slice(6);
    else if (l.startsWith("exit ")) { flush(); post({ type: "exit", code: Number(l.slice(5)), message: error }); }
  };
  if (!emulator.is_running()) {
    // restored: it waits for the pod already
    emulator.add_listener("serial1-output-byte", control);
  }
  emulator.fs9p.storage = new LayerStorage(layers, mem);
  emulator.fs9p.load_from_json(fsJSON);
  statRoot(emulator.fs9p, m.ephemeralStorage || (await imageFSCapacity()));
  emulator.run();
  // A restored VM has the clock and the random numbers of the saved one.
  const seed = [...crypto.getRandomValues(new Uint8Array(32))].map((b) => b.toString(16).padStart(2, "0")).join("");
  emulator.serial_send_bytes(1, new TextEncoder().encode(`go ${Date.now()} ${seed}\n`));
}

function connect(id, port) {
  const net = emulator?.network_adapter;
  if (!net?.connect) { post({ type: "conn", id, event: "close" }); return; }
  const c = net.connect(port);
  conns.set(id, c);
  c.on("connect", () => post({ type: "conn", id, event: "connect" }));
  c.on("data", (d) => post({ type: "conn", id, event: "data", data: d.slice() }));
  // the pod closed its side, or reset the connection
  c.on("shutdown", () => post({ type: "conn", id, event: "close" }));
  c.on("close", () => { conns.delete(id); post({ type: "conn", id, event: "close" }); });
}

self.onmessage = ({ data: m }) => {
  switch (m.type) {
    case "warm": warmUp(m); break;
    case "layer": layerJob(m); break;
    case "start": start(m).catch((e) => post({ type: "error", message: String(e?.message || e) })); break;
    case "connect": connect(m.id, m.port); break;
    case "write": conns.get(m.id)?.write(new Uint8Array(m.data)); break;
    case "close": conns.get(m.id)?.close(); conns.delete(m.id); break;
  }
};
