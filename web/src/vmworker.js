// The worker of a pod on a tab node that runs a container image (vm.js,
// k8s/tabernetes/v86.go). It pulls the linux/386 variant of the image from
// the registry mirror, packs the layers into an initramfs (rootfs.js) and
// boots it in v86, an x86 emulator that compiles x86 code to WebAssembly.
//
// Docker Hub sends no CORS headers, so the page cannot pull from it. The
// mirror is a pull-through cache of Docker Hub that does. Blobs stay in the
// Cache Storage of the browser: a second pull finds the image "already
// present on machine".
import { V86 } from "v86";
import { decompress as unzstd } from "fzstd";
import { buildRootfs, modes } from "./rootfs.js";

const post = (m, transfer) => self.postMessage(m, transfer || []);
const CACHE = "tabernetes-images";
const PLATFORM = { os: "linux", architecture: "386" };
const INDEX_TYPES = ["application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"];
const MANIFEST_TYPES = ["application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"];

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

// Pull fetches manifests and blobs, the blobs through the Cache Storage.
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

  // cached tells whether all blobs are in the Cache Storage.
  async cached(digests) {
    const c = await this.cache;
    if (!c) return false;
    for (const d of digests) if (!(await c.match(this.blobURL(d)))) return false;
    return true;
  }

  blobURL(digest) {
    // registry-independent, so that a new mirror finds the old blobs
    return new URL(`/tabernetes-blobs/${digest}`, self.location.href).href;
  }

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

async function gunzip(bytes) {
  const s = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("gzip"));
  return new Uint8Array(await new Response(s).arrayBuffer());
}

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
  const blobs = [m.config.digest, ...m.layers.map((l) => l.digest)];
  const size = m.layers.reduce((n, l) => n + (l.size || 0), 0) + (m.config.size || 0);
  const present = await p.cached(blobs);
  if (!present) onFetching();
  const config = JSON.parse(new TextDecoder().decode(await p.blob(m.config.digest)));
  if (config.architecture && config.architecture !== PLATFORM.architecture) {
    throw new PullError(`failed to pull and unpack image "${ref.name}": no match for platform in manifest: not found (the tab node runs linux/386 images, this one is ${config.os}/${config.architecture})`);
  }
  const layers = [];
  for (const l of m.layers) {
    const b = await p.blob(l.digest);
    if (l.mediaType?.includes("zstd")) layers.push(unzstd(b));
    else if (l.mediaType?.includes("gzip") || (b[0] === 0x1f && b[1] === 0x8b)) layers.push(await gunzip(b));
    else layers.push(b);
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

  const spec = containerSpec(img.config, m.container);
  if (!spec.args.length) {
    post({ type: "exit", code: 128, message: "no command specified" });
    return;
  }
  const init = new Uint8Array(await (await fetch(m.initURL)).arrayBuffer());
  const c = m.container;
  const conf = { ...spec, hostname: m.hostname, ip: m.ip, gateway: m.gateway, stdin: !!c.stdin, stdinOnce: !!c.stdinOnce, tty: !!c.tty };
  const initrd = buildRootfs(img.layers, {
    ".tabernetes/init": { mode: modes.S_IFREG | 0o755, data: init },
    ".tabernetes/config.json": { mode: modes.S_IFREG | 0o644, data: new TextEncoder().encode(JSON.stringify(conf)) },
  });
  img.layers = null;
  // The initramfs is unpacked into the root file system, a tmpfs of half the
  // RAM, next to the kernel and the running container. A memory limit of the
  // pod can give the VM more room, not less than that.
  const memory = Math.max(m.memory || 0, Math.min(2048, Math.max(256, Math.ceil((initrd.length * 2.5) / 2 ** 20) + 96)) * 2 ** 20);
  emulator = new V86({
    wasm_path: m.wasmURL,
    bios: { url: m.biosURL },
    bzimage: { url: m.kernelURL },
    initrd: { buffer: initrd.buffer },
    cmdline: `console=ttyS0 quiet loglevel=3 rdinit=/.tabernetes/init ip=${m.ip}::${m.gateway}:255.255.255.0:${m.hostname}:eth0:off`,
    memory_size: memory,
    uart1: true,
    autostart: true,
    disable_keyboard: true,
    disable_mouse: true,
    disable_speaker: true,
    net_device: { type: "virtio", relay_url: "fetch", vm_ip: m.ip, router_ip: m.gateway },
  });
  // The console is stdout and stderr of the container. Batch the bytes.
  let out = [], timer = 0;
  const flush = () => { timer = 0; if (out.length) post({ type: "console", data: String.fromCharCode(...out) }); out = []; };
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
