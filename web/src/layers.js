// layers is the image store of the tab node, what /var/lib/containerd is on
// a real node. Every layer of an image is unpacked once into the origin
// private file system (OPFS) of the browser: the uncompressed tar as it is,
// plus an index with the path, metadata and offset of every entry. The pods
// read their files straight from the tar (rootfs.js), so a layer is shared
// by all pods and all tabs, and it is still there after a reload.
//
// Without OPFS (a private window in some browsers) the layers stay in the
// memory of the worker and go with the pod.
//
// OPFS: tabernetes/layers/<digest>.tar and <digest>.json. The index is
// written last: a layer without one was not unpacked to the end.
// <digest>.used is touched whenever a pod starts with the layer.
// <digest>.forgotten marks a layer that forget could not delete, because a
// pod still ran with it (state.js).
//
// Image garbage collection works like the one of the kubelet: when the
// layers take more than 85 % of the image file system (IMAGE_FS, or half of
// the quota of the origin if that is less), the least recently used layers
// go until 80 % are left. A layer that a pod in any tab runs with is never
// collected (a shared Web Lock per layer, held by the worker of the pod), and
// neither is one used in the last 2 minutes (imageMinimumGCAge).
// Not 4 << 30: JavaScript shifts in 32 bits, and that is 0.
const IMAGE_FS = 4 * 2 ** 30;
const HIGH = 0.85, LOW = 0.8, MIN_AGE = 2 * 60 * 1000;

// imageFSCapacity returns the size of the image file system in bytes:
// IMAGE_FS, or half of the quota of the origin if that is less. The root
// file system of a pod shows it as its size (vmworker.js), like the disk of
// /var/lib/containerd in a container on a real node.
export async function imageFSCapacity() {
  let capacity = IMAGE_FS;
  try {
    const { quota } = await navigator.storage.estimate();
    if (quota) capacity = Math.min(capacity, quota / 2);
  } catch { /* no estimate */ }
  return capacity;
}

import { Decompress } from "fzstd";
import { useLock, unpackLock, removeLayer } from "./state.js";

const dec = new TextDecoder();
const VERSION = 1; // of the index

function str(b, off, len) {
  let end = off;
  while (end < off + len && b[end] !== 0) end++;
  return dec.decode(b.subarray(off, end));
}

function oct(b, off, len) {
  // base-256 for large numbers (GNU)
  if (b[off] & 0x80) {
    let n = 0;
    for (let i = off + 1; i < off + len; i++) n = n * 256 + b[i];
    return n;
  }
  const s = str(b, off, len).trim();
  return s ? parseInt(s, 8) : 0;
}

// pax parses an extended header: "<len> <key>=<value>\n" records.
function pax(b) {
  const out = {};
  let i = 0;
  while (i < b.length) {
    const sp = b.indexOf(0x20, i);
    if (sp < 0) break;
    const len = parseInt(dec.decode(b.subarray(i, sp)), 10);
    if (!len) break;
    const rec = dec.decode(b.subarray(sp + 1, i + len - 1));
    const eq = rec.indexOf("=");
    if (eq > 0) out[rec.slice(0, eq)] = rec.slice(eq + 1);
    i += len;
  }
  return out;
}

// TarIndex reads a tar as it streams by and lists its entries:
// [path, type, permissions, uid, gid, mtime, size, offset of the data, link].
export class TarIndex {
  constructor() {
    this.pos = 0;
    this.hdr = new Uint8Array(512);
    this.hlen = 0;
    this.skip = 0; // bytes of data (and padding) still to come
    this.meta = null; // the data of a header that belongs to the next entry
    this.done = false;
    this.longName = this.longLink = null;
    this.ext = {};
    this.global = {};
    this.entries = [];
  }

  push(chunk) {
    let i = 0;
    while (i < chunk.length && !this.done) {
      if (this.skip > 0) {
        const n = Math.min(this.skip, chunk.length - i);
        const m = this.meta;
        if (m && m.got < m.buf.length) {
          const k = Math.min(n, m.buf.length - m.got);
          m.buf.set(chunk.subarray(i, i + k), m.got);
          m.got += k;
        }
        this.skip -= n;
        this.pos += n;
        i += n;
        if (this.skip === 0 && m) this.#meta();
        continue;
      }
      const n = Math.min(512 - this.hlen, chunk.length - i);
      this.hdr.set(chunk.subarray(i, i + n), this.hlen);
      this.hlen += n;
      this.pos += n;
      i += n;
      if (this.hlen === 512) {
        this.hlen = 0;
        this.#header();
      }
    }
  }

  #meta() {
    const { type, buf } = this.meta;
    this.meta = null;
    if (type === "L") this.longName = str(buf, 0, buf.length);
    else if (type === "K") this.longLink = str(buf, 0, buf.length);
    else if (type === "x") this.ext = pax(buf);
    else if (type === "g") this.global = { ...this.global, ...pax(buf) };
  }

  #header() {
    const h = this.hdr;
    if (h.every((x) => x === 0)) { this.done = true; return; }
    const type = String.fromCharCode(h[156] || 0x30);
    if ("LKxg".includes(type)) {
      const size = oct(h, 124, 12);
      this.skip = Math.ceil(size / 512) * 512;
      this.meta = { type, buf: new Uint8Array(size), got: 0 };
      if (!size) this.#meta();
      return;
    }
    const a = { ...this.global, ...this.ext };
    const size = a.size ? +a.size : oct(h, 124, 12);
    // links and directories have no data, whatever the size field says
    this.skip = "0\x007".includes(type) || !"125634".includes(type) ? Math.ceil(size / 512) * 512 : 0;
    let name = str(h, 0, 100);
    // POSIX only: GNU tar ("ustar  ") keeps other fields in that place
    if (str(h, 257, 6) === "ustar") {
      const prefix = str(h, 345, 155);
      if (prefix) name = prefix + "/" + name;
    }
    name = this.longName ?? a.path ?? name;
    const link = this.longLink ?? a.linkpath ?? str(h, 157, 100);
    this.longName = this.longLink = null;
    this.ext = {};
    const uid = a.uid ? +a.uid : oct(h, 108, 8);
    const gid = a.gid ? +a.gid : oct(h, 116, 8);
    const mtime = Math.floor(a.mtime ? +a.mtime : oct(h, 136, 12));
    this.entries.push([name, type, oct(h, 100, 8) & 0o7777, uid, gid, mtime, size, this.pos, type === "1" || type === "2" ? link : ""]);
  }
}

// unpack decompresses a layer as it streams in, hands the tar to
// write(chunk, at) and returns its index and size. The compressed layer is
// never in memory as a whole: for the 230 MB layer of python that halved the
// peak memory of the pull (~1 GB before).
async function unpack(stream, mediaType, write) {
  const idx = new TarIndex();
  let at = 0;
  const put = (chunk) => {
    write(chunk, at);
    at += chunk.length;
    idx.push(chunk);
  };
  const r = stream.getReader();
  let { done, value: first } = await r.read();
  first ??= new Uint8Array(0);
  const zstd = mediaType?.includes("zstd") || (first[0] === 0x28 && first[1] === 0xb5 && first[2] === 0x2f && first[3] === 0xfd);
  const gzip = !zstd && (mediaType?.includes("gzip") || (first[0] === 0x1f && first[1] === 0x8b));
  if (zstd) {
    // fzstd needs to know which chunk is the last one, so each chunk waits
    // for the next.
    const d = new Decompress((chunk) => put(chunk));
    let prev = first;
    while (!done) {
      const { done: end, value } = await r.read();
      done = end;
      if (value) {
        d.push(prev, false);
        prev = value;
      }
    }
    d.push(prev, true);
  } else if (gzip) {
    const rest = new ReadableStream({
      start(c) { if (first.length) c.enqueue(first); if (done) c.close(); },
      async pull(c) {
        const { done, value } = await r.read();
        if (value) c.enqueue(value);
        if (done) c.close();
      },
      cancel(why) { return r.cancel(why); },
    });
    const z = rest.pipeThrough(new DecompressionStream("gzip")).getReader();
    for (;;) {
      const { done, value } = await z.read();
      if (done) break;
      put(value);
    }
  } else {
    if (first.length) put(first);
    while (!done) {
      const { done: end, value } = await r.read();
      done = end;
      if (value) put(value);
    }
  }
  return { entries: idx.entries, size: at };
}

const fileName = (digest) => digest.replace(":", "-");

// openStore returns the image store: in OPFS if the browser has it, else in
// memory.
export async function openStore() {
  try {
    const root = await navigator.storage.getDirectory();
    const dir = await (await root.getDirectoryHandle("tabernetes", { create: true })).getDirectoryHandle("layers", { create: true });
    // writing needs a sync access handle (dedicated workers only)
    if (typeof FileSystemFileHandle !== "undefined" && "createSyncAccessHandle" in FileSystemFileHandle.prototype) return new OPFSStore(dir);
  } catch { /* no OPFS */ }
  return new MemoryStore();
}

class OPFSStore {
  constructor(dir) {
    this.dir = dir;
    this.persistent = true;
  }

  // index returns the index of an unpacked layer, or null.
  async index(digest) {
    try {
      const f = await (await this.dir.getFileHandle(fileName(digest) + ".json")).getFile();
      const idx = JSON.parse(await f.text());
      return idx.version === VERSION ? idx : null;
    } catch {
      return null;
    }
  }

  // add unpacks a layer, once for all tabs. fetch() returns the stream of
  // the compressed layer and verify() throws when its digest is wrong; the
  // index is written only after it passed, so a bad layer is never used.
  async add(digest, fetch, verify, mediaType) {
    const run = async () => {
      const have = await this.index(digest);
      if (have) return have;
      const name = fileName(digest);
      const h = await (await this.dir.getFileHandle(name + ".tar", { create: true })).createSyncAccessHandle();
      let idx;
      try {
        h.truncate(0);
        try {
          const { entries, size } = await unpack(await fetch(), mediaType, (chunk, at) => {
            if (h.write(chunk, { at }) !== chunk.length) throw new DOMException("short write", "QuotaExceededError");
          });
          verify();
          h.flush();
          idx = { version: VERSION, size, entries };
        } catch (e) {
          h.truncate(0); // give the room back, the layer has no index anyway
          throw e;
        }
      } finally {
        h.close();
      }
      const j = await (await this.dir.getFileHandle(name + ".json", { create: true })).createSyncAccessHandle();
      try {
        const b = new TextEncoder().encode(JSON.stringify(idx));
        j.truncate(0);
        j.write(b, { at: 0 });
        j.flush();
      } finally {
        j.close();
      }
      return idx;
    };
    return navigator.locks ? navigator.locks.request(unpackLock(digest), run) : run();
  }

  // use marks a layer as used by this worker until it ends, before the
  // pull reads its index: the garbage collection waits for that, or the pull
  // waits for the garbage collection and unpacks the layer again.
  async use(digest) {
    if (navigator.locks) {
      await new Promise((granted) => navigator.locks.request(useLock(digest), { mode: "shared" }, () => {
        granted();
        return new Promise(() => {}); // until the worker ends
      }));
    }
    // The time of use only orders the garbage collection. Writing it takes
    // ~25 ms per layer (createWritable), so the pull does not wait for it.
    (async () => {
      const w = await (await this.dir.getFileHandle(fileName(digest) + ".used", { create: true })).createWritable();
      await w.write(String(Date.now()));
      await w.close();
    })().catch(() => { /* another pod touches it right now */ });
  }

  // gc removes the least recently used layers that no pod uses, once they
  // take too much room. It returns the bytes it freed.
  async gc() {
    if (!navigator.locks) return 0;
    // one tab at a time
    return navigator.locks.request("tabernetes-image-gc", { ifAvailable: true }, async (lock) => {
      if (!lock) return 0;
      const layers = new Map(); // file name -> {size, used, forgotten}
      const get = (n) => layers.get(n) || layers.set(n, { size: 0, used: 0, forgotten: false }).get(n);
      for await (const [name, h] of this.dir.entries()) {
        const m = name.match(/^(.+)\.(tar|json|used|forgotten)$/);
        if (!m || h.kind !== "file") continue;
        const f = await h.getFile();
        const l = get(m[1]);
        if (m[2] === "tar") l.size = f.size;
        if (m[2] === "forgotten") l.forgotten = true;
        else l.used = Math.max(l.used, f.lastModified);
      }
      // forget left these to the pods that ran with them
      for (const [name, l] of layers) {
        if (l.forgotten && await removeLayer(this.dir, name)) layers.delete(name);
      }
      let total = 0;
      for (const l of layers.values()) total += l.size;
      const capacity = await imageFSCapacity();
      if (total <= capacity * HIGH) return 0;
      const goal = total - capacity * LOW;
      console.info(`tabernetes: image garbage collection: the image file system is at ${Math.round((total / capacity) * 100)} %, which is over the high threshold (${HIGH * 100} %). Trying to free ${goal} bytes down to the low threshold (${LOW * 100} %).`);
      let freed = 0;
      const now = Date.now();
      for (const [name, l] of [...layers].sort((a, b) => a[1].used - b[1].used)) {
        if (freed >= goal) break;
        if (now - l.used < MIN_AGE) continue;
        // not while a pod runs with it, and not while a pull unpacks it
        if (await removeLayer(this.dir, name)) freed += l.size;
      }
      if (freed < goal) console.warn(`tabernetes: image garbage collection freed ${freed} bytes, wanted ${goal}: the other layers are in use`);
      return freed;
    });
  }

  // layer is the reader of the tar of an unpacked layer.
  async layer(digest) {
    const file = await (await this.dir.getFileHandle(fileName(digest) + ".tar")).getFile();
    return { read: async (offset, length) => new Uint8Array(await file.slice(offset, offset + length).arrayBuffer()) };
  }
}

class MemoryStore {
  constructor() {
    this.tars = new Map();
    this.indexes = new Map();
    this.persistent = false;
  }

  async index(digest) {
    return this.indexes.get(digest) || null;
  }

  // The layers go with the worker: nothing to collect.
  async use() {}
  async gc() { return 0; }

  async add(digest, fetch, verify, mediaType) {
    const chunks = [];
    const { entries, size } = await unpack(await fetch(), mediaType, (chunk) => chunks.push(chunk.slice()));
    verify();
    const tar = new Uint8Array(size);
    let off = 0;
    for (const c of chunks) { tar.set(c, off); off += c.length; }
    const idx = { version: VERSION, size, entries };
    this.tars.set(digest, tar);
    this.indexes.set(digest, idx);
    return idx;
  }

  async layer(digest) {
    const tar = this.tars.get(digest);
    return { read: async (offset, length) => tar.subarray(offset, offset + length) };
  }
}
