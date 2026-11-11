// rootfs merges the layers of an image into the root file system of a pod
// for v86 (vmworker.js). The layers are indexed tar files (layers.js),
// applied in order with the whiteouts of the OCI image spec (.wh.<name>
// deletes, .wh..wh..opq empties a directory). Extra files (the init of the
// pod) go on top.
//
// The result is the file system JSON of v86 (version 3): the tree with the
// metadata of every file and, instead of the data, a key. v86 serves the
// tree over virtio 9p and asks the storage (LayerStorage) for the data when
// the guest reads a file. The kernel mounts it as its root. A file the pod
// writes stays in the memory of the worker, the layers do not change.

const S_IFMT = 0o170000, S_IFDIR = 0o040000, S_IFREG = 0o100000, S_IFLNK = 0o120000, S_IFIFO = 0o010000;

const enc = new TextEncoder();

export function clean(p) {
  const parts = [];
  for (const x of p.split("/")) {
    if (!x || x === ".") continue;
    if (x === "..") parts.pop();
    else parts.push(x);
  }
  return parts.join("/");
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

  // addLayer applies one layer: the entries of its index (layers.js), the
  // data of a file is at offset in the layer with the id.
  addLayer(id, entries) {
    const opaque = [];
    for (const [name, type, perm, uid, gid, mtime, size, offset, link] of entries) {
      const path = clean(name);
      const meta = { uid, gid, mtime };
      const base = path.slice(path.lastIndexOf("/") + 1);
      const dir = path.slice(0, Math.max(path.lastIndexOf("/"), 0));
      if (base === ".wh..wh..opq") { opaque.push(dir); continue; }
      if (base.startsWith(".wh.")) { this.#remove(clean(dir + "/" + base.slice(4))); continue; }
      switch (type) {
        case "0": case "\0": case "7":
          this.set(path, { ...meta, mode: S_IFREG | perm, size, key: `${id}:${offset}` });
          break;
        case "1": { // hard link: the data of the target
          const target = this.files.get(clean(link));
          if (target && (target.mode & S_IFMT) === S_IFREG) this.set(path, { ...target });
          break;
        }
        case "2":
          this.set(path, { ...meta, mode: S_IFLNK | 0o777, target: link });
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

  // v86 returns the file system JSON (version 3) of v86: every entry is
  // [name, size, mtime, mode, uid, gid, children | key | symlink target].
  v86() {
    const fsroot = [];
    const dirs = new Map([["", fsroot]]);
    let total = 0;
    for (const path of [...this.files.keys()].sort()) {
      const e = this.files.get(path);
      const parent = dirs.get(path.slice(0, Math.max(path.lastIndexOf("/"), 0)));
      if (!parent) continue; // below something that is no directory
      const name = path.slice(path.lastIndexOf("/") + 1);
      const fmt = e.mode & S_IFMT;
      let size = 0, x;
      if (fmt === S_IFDIR) {
        x = [];
        dirs.set(path, x);
      } else if (fmt === S_IFREG) {
        size = e.size;
        x = e.key;
      } else if (fmt === S_IFLNK) {
        size = enc.encode(e.target).length;
        x = e.target;
      } else continue; // v86 has no FIFOs
      total += size;
      parent.push([name, size, e.mtime, e.mode, e.uid, e.gid, x]);
    }
    return { version: 3, size: total, fsroot };
  }
}

// LayerStorage gives v86 the data of the files: from the layers, or from
// memory for the extra files (key "mem:<n>"). A small file is read whole and
// kept for a while, because the guest reads it in pieces.
export class LayerStorage {
  constructor(layers, mem) {
    this.layers = layers; // id -> {read(offset, length): Promise<Uint8Array>}
    this.mem = mem; // key -> Uint8Array
    this.lru = new Map();
    this.bytes = 0;
  }

  async read(key, offset, count, size) {
    const m = this.mem.get(key);
    if (m) return m.subarray(offset, offset + count);
    let data = this.lru.get(key);
    if (data) {
      this.lru.delete(key);
      this.lru.set(key, data);
      return data.subarray(offset, offset + count);
    }
    const colon = key.lastIndexOf(":");
    const layer = this.layers.get(key.slice(0, colon));
    const start = Number(key.slice(colon + 1));
    if (size > LayerStorage.WHOLE) {
      return layer.read(start + offset, Math.max(0, Math.min(count, size - offset)));
    }
    data = await layer.read(start, size);
    this.lru.set(key, data);
    this.bytes += data.length;
    for (const [k, v] of this.lru) {
      if (this.bytes <= LayerStorage.CACHE) break;
      this.lru.delete(k);
      this.bytes -= v.length;
    }
    return data.subarray(offset, offset + count);
  }

  // v86 calls these for files it keeps itself; the layers stay as they are.
  async cache() {}
  uncache() {}
}
LayerStorage.WHOLE = 4 << 20;
LayerStorage.CACHE = 64 << 20;

export const modes = { S_IFDIR, S_IFREG, S_IFLNK };
