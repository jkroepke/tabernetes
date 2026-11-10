// rootfs turns the layers of an image into an initramfs (cpio "newc") for
// v86 (vmworker.js): the layers are tar files, applied in order, with the
// whiteouts of the OCI image spec (.wh.<name> deletes, .wh..wh..opq empties
// a directory). Extra files (the init of the pod) go on top.

const S_IFMT = 0o170000, S_IFDIR = 0o040000, S_IFREG = 0o100000, S_IFLNK = 0o120000, S_IFIFO = 0o010000;

const dec = new TextDecoder();
const enc = new TextEncoder();

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

function clean(p) {
  const parts = [];
  for (const x of p.split("/")) {
    if (!x || x === ".") continue;
    if (x === "..") parts.pop();
    else parts.push(x);
  }
  return parts.join("/");
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

// Tree is the file system while the layers are applied.
export class Tree {
  constructor() {
    this.files = new Map(); // path (no leading slash) -> entry
    this.layer = 0; // the layer being applied, for opaque directories
  }

  #parents(path) {
    let p = path;
    while ((p = p.slice(0, Math.max(p.lastIndexOf("/"), 0)))) {
      if (!this.files.has(p)) this.files.set(p, { mode: S_IFDIR | 0o755, uid: 0, gid: 0, mtime: 0 });
    }
  }

  #remove(path) {
    this.files.delete(path);
    const prefix = path + "/";
    for (const k of this.files.keys()) if (k.startsWith(prefix)) this.files.delete(k);
  }

  set(path, entry) {
    path = clean(path);
    if (!path) return;
    const old = this.files.get(path);
    // a directory over a directory keeps its contents
    if (old && !((old.mode & S_IFMT) === S_IFDIR && (entry.mode & S_IFMT) === S_IFDIR)) this.#remove(path);
    this.#parents(path);
    this.files.set(path, { ...entry, layer: this.layer });
  }

  // addLayer applies one layer (an uncompressed tar).
  addLayer(tar) {
    const opaque = [];
    let longName = null, longLink = null, ext = {}, global = {};
    for (let off = 0; off + 512 <= tar.length;) {
      const h = tar.subarray(off, off + 512);
      if (h.every((x) => x === 0)) break;
      const size = oct(h, 124, 12);
      const type = String.fromCharCode(h[156] || 0x30);
      const data = tar.subarray(off + 512, off + 512 + size);
      off += 512 + Math.ceil(size / 512) * 512;
      if (type === "L") { longName = str(data, 0, data.length); continue; }
      if (type === "K") { longLink = str(data, 0, data.length); continue; }
      if (type === "x") { ext = pax(data); continue; }
      if (type === "g") { global = { ...global, ...pax(data) }; continue; }
      let name = str(h, 0, 100);
      const magic = str(h, 257, 6);
      if (magic.startsWith("ustar")) {
        const prefix = str(h, 345, 155);
        if (prefix) name = prefix + "/" + name;
      }
      const attrs = { ...global, ...ext };
      name = longName ?? attrs.path ?? name;
      const link = longLink ?? attrs.linkpath ?? str(h, 157, 100);
      longName = longLink = null;
      ext = {};
      const path = clean(name);
      const perm = oct(h, 100, 8) & 0o7777;
      const meta = {
        uid: attrs.uid ? +attrs.uid : oct(h, 108, 8),
        gid: attrs.gid ? +attrs.gid : oct(h, 116, 8),
        mtime: Math.floor(attrs.mtime ? +attrs.mtime : oct(h, 136, 12)),
      };

      // whiteouts
      const base = path.slice(path.lastIndexOf("/") + 1);
      const dir = path.slice(0, Math.max(path.lastIndexOf("/"), 0));
      if (base === ".wh..wh..opq") { opaque.push(dir); continue; }
      if (base.startsWith(".wh.")) { this.#remove(clean(dir + "/" + base.slice(4))); continue; }

      switch (type) {
        case "0": case "\0": case "7":
          this.set(path, { ...meta, mode: S_IFREG | perm, data });
          break;
        case "1": { // hard link: the same inode (busybox has 400 of them)
          const target = this.files.get(clean(link));
          if (target && (target.mode & S_IFMT) === S_IFREG) this.set(path, { ...target, inode: target.inode || target });
          break;
        }
        case "2":
          this.set(path, { ...meta, mode: S_IFLNK | 0o777, data: enc.encode(link) });
          break;
        case "5":
          this.set(path, { ...meta, mode: S_IFDIR | perm });
          break;
        case "6":
          this.set(path, { ...meta, mode: S_IFIFO | perm });
          break;
        // character and block devices: devtmpfs provides /dev
      }
    }
    // An opaque directory hides what the lower layers had in it, but not
    // what this layer put there.
    for (const d of opaque) {
      const prefix = d ? d + "/" : "";
      for (const [k, v] of this.files) {
        if (k.startsWith(prefix) && v.layer !== this.layer) this.files.delete(k);
      }
    }
  }

  // cpio writes the tree as an initramfs, directories before their contents.
  // Hard links share the inode number; the first of them carries the data,
  // the kernel links the others to it.
  cpio() {
    const paths = [...this.files.keys()].sort();
    const chunks = [];
    let next = 1;
    const links = new Map(); // inode -> { ino, nlink, written }
    for (const e of this.files.values()) {
      if (!e.inode) continue;
      const l = links.get(e.inode) || { ino: 0, nlink: 0, written: false };
      l.nlink++;
      links.set(e.inode, l);
    }
    // the file a link points to counts, too, if it is still there
    for (const e of this.files.values()) if (links.has(e)) links.get(e).nlink++;
    const hex = (n) => (n >>> 0).toString(16).padStart(8, "0");
    const pad = (n) => (4 - (n % 4)) % 4;
    const zero = new Uint8Array(4);
    const put = (name, e) => {
      const nameBytes = enc.encode(name + "\0");
      let data = e.data || new Uint8Array(0);
      let ino = next++, nlink = 1;
      const l = links.get(e.inode || e);
      if (l && l.nlink > 1) {
        if (!l.ino) l.ino = ino;
        ino = l.ino;
        nlink = l.nlink;
        if (l.written) data = new Uint8Array(0);
        l.written = true;
      }
      const header = "070701" + hex(ino) + hex(e.mode) + hex(e.uid) + hex(e.gid) + hex(nlink) + hex(e.mtime) +
        hex(data.length) + hex(0) + hex(0) + hex(0) + hex(0) + hex(nameBytes.length) + hex(0);
      chunks.push(enc.encode(header), nameBytes, zero.subarray(0, pad(110 + nameBytes.length)));
      if (data.length) chunks.push(data, zero.subarray(0, pad(data.length)));
    };
    for (const p of paths) put(p, this.files.get(p));
    put("TRAILER!!!", { mode: 0, uid: 0, gid: 0, mtime: 0 });
    let size = 0;
    for (const c of chunks) size += c.length;
    const out = new Uint8Array(size);
    let off = 0;
    for (const c of chunks) { out.set(c, off); off += c.length; }
    return out;
  }
}

export function buildRootfs(layers, extra) {
  const t = new Tree();
  layers.forEach((tar, i) => {
    t.layer = i;
    t.addLayer(tar);
  });
  t.layer = layers.length;
  for (const [path, e] of Object.entries(extra)) {
    t.set(path, { uid: 0, gid: 0, mtime: Math.floor(Date.now() / 1000), ...e });
  }
  return t.cpio();
}

export const modes = { S_IFDIR, S_IFREG, S_IFLNK };
