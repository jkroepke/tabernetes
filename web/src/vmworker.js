// The worker of a pod on a tab node that runs a container image (vm.js,
// k8s/tabernetes/v86.go). It pulls the linux/386 variant of the image from
// the registry mirror, unpacks the layers into the image store of the
// browser (layers.js) and boots v86, an x86 emulator that compiles x86 code
// to WebAssembly, with the merged layers as its root file system over 9p
// (rootfs.js). The files stay outside the memory of the VM.
//
// Docker Hub sends no CORS headers, so the page cannot pull from it. The
// mirror is a pull-through cache of Docker Hub that does. The unpacked
// layers stay in the browser: a second pull finds the image "already present
// on machine", in every tab and after a reload.
import { V86 } from "v86";
import { Tree, LayerStorage, modes } from "./rootfs.js";
import { openStore } from "./layers.js";
import { SHA256 } from "./sha256.js";

const post = (m, transfer) => self.postMessage(m, transfer || []);
const CACHE = "tabernetes-configs"; // the configs of the images, small JSON blobs
// The layers were here before they moved to the image store.
caches.delete("tabernetes-images").catch(() => {});
const PLATFORM = { os: "linux", architecture: "386" };
const INDEX_TYPES = ["application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"];
const MANIFEST_TYPES = ["application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"];

// MiB, of the VM of a pod: its memory limit, or 256 MiB without one. The
// files of the image are not in it, only what the container runs and writes
// to tmpfs, plus the page cache. Every pod has a worker and a wasm memory of
// its own, but v86 gives a VM at most 2 GiB (less 128 KiB). The kernel uses
// all of it (highmem, vm/kernel.config), not only the first 896 MiB.
const MIN_MEMORY = 128, DEFAULT_MEMORY = 256, MAX_MEMORY = 2048;

class PullError extends Error {}

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

  blobURL(digest) {
    // registry-independent, so that a new mirror finds the old blobs
    return new URL(`/tabernetes-blobs/${digest}`, self.location.href).href;
  }

  // layer streams the blob of a layer and hashes it on the way. verify()
  // throws once the stream ended when the digest is wrong.
  layer(digest) {
    const hash = new SHA256();
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
            got = "sha256:" + hash.hex();
            c.close();
            return;
          }
          hash.update(chunk.value);
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
async function pullImage(registry, image, onFetching) {
  const ref = parseRef(image);
  if (ref.domain !== "docker.io") {
    throw new PullError(`failed to resolve reference "${ref.name}": the tab node pulls from Docker Hub only (through the registry mirror of this page)`);
  }
  const p = new Pull(registry, ref);
  let { digest, json: m } = await p.manifest(ref.reference, [...INDEX_TYPES, ...MANIFEST_TYPES]);
  const imageID = `${ref.domain}/${ref.repo}@${digest}`;
  if (m.manifests) {
    const d = m.manifests.find((x) => x.platform?.os === PLATFORM.os && x.platform?.architecture === PLATFORM.architecture);
    if (!d) throw new PullError(`failed to pull and unpack image "${ref.name}": no match for platform in manifest: not found (the tab node runs linux/386 images, ${ref.name} has ${[...new Set(m.manifests.map((x) => x.platform && `${x.platform.os}/${x.platform.architecture}`).filter((x) => x && !x.startsWith("unknown")))].join(", ")})`);
    m = (await p.manifest(d.digest, MANIFEST_TYPES)).json;
  } else if (m.config) {
    // a single-platform image: it must be linux/386 itself
  } else {
    throw new PullError(`failed to pull and unpack image "${ref.name}": unsupported manifest`);
  }
  store ??= await openStore();
  const size = m.layers.reduce((n, l) => n + (l.size || 0), 0) + (m.config.size || 0);
  for (const l of m.layers) await store.use(l.digest);
  const indexes = await Promise.all(m.layers.map((l) => store.index(l.digest)));
  const present = (await p.cached(m.config.digest)) && indexes.every(Boolean);
  if (!present) onFetching();
  const config = JSON.parse(new TextDecoder().decode(await p.blob(m.config.digest)));
  if (config.architecture && config.architecture !== PLATFORM.architecture) {
    throw new PullError(`failed to pull and unpack image "${ref.name}": no match for platform in manifest: not found (the tab node runs linux/386 images, this one is ${config.os}/${config.architecture})`);
  }
  const layers = [];
  for (const [i, l] of m.layers.entries()) {
    let idx = indexes[i];
    if (!idx) {
      const { fetch, verify } = p.layer(l.digest);
      try {
        idx = await store.add(l.digest, fetch, verify, l.mediaType);
      } catch (e) {
        if (e instanceof PullError) throw e;
        const why = e?.name === "QuotaExceededError" ? "no space left on device" : e?.message || String(e);
        throw new PullError(`failed to extract layer ${l.digest}: write /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs: ${why}`);
      }
    }
    layers.push({ id: l.digest, entries: idx.entries });
  }
  return { present, size, imageID, config: config.config || {}, layers };
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

async function start(m) {
  const t0 = Date.now();
  let img;
  try {
    img = await pullImage(m.registry, m.image, () => post({ type: "pulling" }));
  } catch (e) {
    post({ type: "error", pull: true, message: e instanceof PullError ? e.message : `failed to pull and unpack image "${m.image}": ${e.message}` });
    return;
  }
  post({ type: "pulled", present: img.present, size: img.size, imageID: img.imageID, ms: Date.now() - t0 });
  // The layers of this pod are in use now; a new image may need room.
  if (!img.present) store.gc().catch((e) => console.warn("tabernetes: image garbage collection:", e));

  const spec = containerSpec(img.config, m.container);
  if (!spec.args.length) {
    post({ type: "exit", code: 128, message: "no command specified" });
    return;
  }
  const init = new Uint8Array(await (await fetch(m.initURL)).arrayBuffer());
  const c = m.container;
  const conf = { ...spec, hostname: m.hostname, ip: m.ip, gateway: m.gateway, stdin: !!c.stdin, stdinOnce: !!c.stdinOnce, tty: !!c.tty };
  // The root file system: the layers, and the init of the pod on top. The
  // mount points must be there for the kernel (devtmpfs) and init.
  const tree = new Tree();
  img.layers.forEach((l, i) => {
    tree.layer = i;
    tree.addLayer(l.id, l.entries);
  });
  tree.layer = img.layers.length;
  const now = Math.floor(Date.now() / 1000);
  const mem = new Map();
  const extra = {
    ".tabernetes/init": { mode: modes.S_IFREG | 0o755, data: init },
    ".tabernetes/config.json": { mode: modes.S_IFREG | 0o644, data: new TextEncoder().encode(JSON.stringify(conf)) },
  };
  for (const [path, e] of Object.entries(extra)) {
    const key = `mem:${mem.size}`;
    mem.set(key, e.data);
    tree.set(path, { uid: 0, gid: 0, mtime: now, mode: e.mode, size: e.data.length, key });
  }
  for (const d of ["dev", "proc", "sys", "tmp"]) {
    if (!tree.files.has(d)) tree.set(d, { uid: 0, gid: 0, mtime: now, mode: modes.S_IFDIR | (d === "tmp" ? 0o1777 : 0o755) });
  }
  const layers = new Map();
  for (const l of img.layers) layers.set(l.id, await store.layer(l.id));
  const fsJSON = tree.v86();
  img.layers = null;
  const memory = Math.min(MAX_MEMORY, Math.max(MIN_MEMORY, m.memory ? Math.ceil(m.memory / 2 ** 20) : DEFAULT_MEMORY)) * 2 ** 20;
  emulator = new V86({
    wasm_path: m.wasmURL,
    bios: { url: m.biosURL },
    bzimage: { url: m.kernelURL },
    // an empty 9p file system; the tree comes in below, before the boot
    filesystem: {},
    // cache=loose: only the guest changes the files, it may keep them
    cmdline: "console=ttyS0 quiet loglevel=3 root=host9p rootfstype=9p rw " +
      "rootflags=trans=virtio,version=9p2000.L,cache=loose,msize=262144 init=/.tabernetes/init " +
      `ip=${m.ip}::${m.gateway}:255.255.255.0:${m.hostname}:eth0:off`,
    memory_size: memory,
    uart1: true,
    autostart: false,
    disable_keyboard: true,
    disable_mouse: true,
    disable_speaker: true,
    net_device: { type: "virtio", relay_url: "fetch", vm_ip: m.ip, router_ip: m.gateway },
  });
  emulator.add_listener("emulator-loaded", () => {
    emulator.fs9p.storage = new LayerStorage(layers, mem);
    emulator.fs9p.load_from_json(fsJSON);
    emulator.run();
  });
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
  let line = "", error = "";
  emulator.add_listener("serial1-output-byte", (b) => {
    if (b === 13) return; // the tty sends \r\n
    if (b !== 10) { line += String.fromCharCode(b); return; }
    const l = line;
    line = "";
    if (l === "started") post({ type: "started", memory });
    else if (l.startsWith("error ")) error = l.slice(6);
    else if (l.startsWith("exit ")) { flush(); post({ type: "exit", code: Number(l.slice(5)), message: error }); }
  });
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
    case "start": start(m).catch((e) => post({ type: "error", message: String(e?.message || e) })); break;
    case "connect": connect(m.id, m.port); break;
    case "write": conns.get(m.id)?.write(new Uint8Array(m.data)); break;
    case "close": conns.get(m.id)?.close(); conns.delete(m.id); break;
  }
};
