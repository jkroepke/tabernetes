// Tiny in-memory file system for Go's js/wasm syscall layer.
// Go expects a subset of the Node.js callback based fs API on globalThis.fs.
// The files below /root of a tab survive a reload: every change goes to
// IndexedDB a moment later (state.js), and the next start loads them first.
// Other tabs see a change after their next reload.

const S_IFMT = 0o170000, S_IFREG = 0o100000, S_IFDIR = 0o040000, S_IFLNK = 0o120000;
const constants = {
  O_RDONLY: 0, O_WRONLY: 1, O_RDWR: 2, O_CREAT: 0o100, O_EXCL: 0o200, O_TRUNC: 0o1000,
  O_APPEND: 0o2000, O_DIRECTORY: 0o200000, O_SYNC: 0o4010000,
};

// Node.js applies the umask in the kernel, here open and mkdir do: a new
// file is -rw-r--r--, not -rw-rw-rw-.
const UMASK = 0o022;

function fsErr(code, path) {
  const e = new Error(`${code}: ${path ?? ""}`);
  e.code = code;
  return e;
}

let inoCounter = 1;
function newNode(mode, extra = {}) {
  const now = Date.now();
  return { ino: inoCounter++, mode, nlink: 1, uid: 0, gid: 0, atime: now, mtime: now, ctime: now, ...extra };
}
const isDir = (n) => (n.mode & S_IFMT) === S_IFDIR;
const isLink = (n) => (n.mode & S_IFMT) === S_IFLNK;

// persist: {prefix, skip: [paths], save(put, del)} keeps the files below
// prefix, except the ones below skip.
export function createFS({ cwd = "/", onOutput = () => {}, persist = null } = {}) {
  const root = newNode(S_IFDIR | 0o755, { children: new Map() });
  const fds = new Map();
  let nextFd = 10;
  const dec = new TextDecoder();

  const split = (p) => {
    if (!p.startsWith("/")) p = cwd.replace(/\/$/, "") + "/" + p;
    const parts = [];
    for (const s of p.split("/")) {
      if (!s || s === ".") continue;
      if (s === "..") parts.pop();
      else parts.push(s);
    }
    return parts;
  };

  // resolve returns {node, parent, name}; node may be undefined if it does not exist.
  function resolve(path, follow = true, depth = 0) {
    if (depth > 32) throw fsErr("ELOOP", path);
    const parts = split(path);
    let node = root, parent = null, name = "";
    for (let i = 0; i < parts.length; i++) {
      if (!isDir(node)) throw fsErr("ENOTDIR", path);
      parent = node;
      name = parts[i];
      node = node.children.get(name);
      if (!node) {
        if (i < parts.length - 1) throw fsErr("ENOENT", path);
        return { node: undefined, parent, name };
      }
      if (isLink(node) && (follow || i < parts.length - 1)) {
        const base = "/" + parts.slice(0, i).join("/");
        const target = node.target.startsWith("/") ? node.target : base + "/" + node.target;
        const rest = parts.slice(i + 1).join("/");
        return resolve(rest ? target + "/" + rest : target, follow, depth + 1);
      }
    }
    return { node, parent, name };
  }

  const must = (path, follow = true) => {
    const r = resolve(path, follow);
    if (!r.node) throw fsErr("ENOENT", path);
    return r;
  };

  const stat = (n) => {
    const size = isDir(n) ? 4096 : isLink(n) ? n.target.length : n.size;
    return {
      dev: 1, ino: n.ino, mode: n.mode, nlink: n.nlink, uid: n.uid, gid: n.gid, rdev: 0,
      size, blksize: 4096, blocks: Math.ceil(size / 512),
      atimeMs: n.atime, mtimeMs: n.mtime, ctimeMs: n.ctime,
      isDirectory: () => isDir(n), isFile: () => (n.mode & S_IFMT) === S_IFREG,
    };
  };

  const ensureCap = (n, size) => {
    if (n.data.length >= size) return;
    const buf = new Uint8Array(Math.max(size, n.data.length * 2, 256));
    buf.set(n.data.subarray(0, n.size));
    n.data = buf;
  };

  // changed paths, saved together a moment after the last change
  const changed = new Set();
  let timer = 0;
  const abs = (path) => "/" + split(path).join("/");
  // below prefix: prefix itself is always there
  const kept = (path) => persist && path.startsWith(persist.prefix + "/") &&
    !persist.skip.some((s) => path === s || path.startsWith(s + "/"));
  const touch = (path) => {
    if (!persist) return;
    path = abs(path);
    if (!kept(path)) return;
    changed.add(path);
    clearTimeout(timer);
    timer = setTimeout(flush, 300);
  };
  const record = (path, n) => {
    const r = { path, mode: n.mode, mtime: n.mtime };
    if (isLink(n)) r.target = n.target;
    else if (!isDir(n)) r.data = n.data.slice(0, n.size);
    return r;
  };
  function flush() {
    const put = [], del = [];
    const walk = (path, n) => {
      if (!kept(path)) return;
      put.push(record(path, n));
      if (isDir(n)) for (const [name, c] of n.children) walk(path + "/" + name, c);
    };
    for (const path of changed) {
      let n;
      try { n = resolve(path, false).node; } catch { n = undefined; }
      if (n) walk(path, n);
      else del.push(path);
    }
    changed.clear();
    if (put.length || del.length) persist.save(put, del).catch((e) => console.warn("tabernetes: saving files:", e));
  }

  const getFd = (fd) => {
    const f = fds.get(fd);
    if (!f) throw fsErr("EBADF");
    return f;
  };

  const ops = {
    open(path, flags, mode) {
      let { node, parent, name } = resolve(path);
      if (node && (flags & constants.O_CREAT) && (flags & constants.O_EXCL)) throw fsErr("EEXIST", path);
      if (!node) {
        if (!(flags & constants.O_CREAT)) throw fsErr("ENOENT", path);
        if (!parent || !isDir(parent)) throw fsErr("ENOENT", path);
        node = newNode(S_IFREG | (mode & 0o7777 & ~UMASK), { data: new Uint8Array(0), size: 0 });
        parent.children.set(name, node);
        parent.mtime = Date.now();
      }
      if (isDir(node) && (flags & 3) !== 0) throw fsErr("EISDIR", path);
      if (!isDir(node) && (flags & constants.O_DIRECTORY)) throw fsErr("ENOTDIR", path);
      if ((flags & constants.O_TRUNC) && !isDir(node)) { node.size = 0; node.mtime = Date.now(); }
      const fd = nextFd++;
      const p = abs(path);
      fds.set(fd, { node, pos: 0, flags, path: p });
      if (flags & (constants.O_CREAT | constants.O_TRUNC)) touch(p);
      return fd;
    },
    close(fd) { getFd(fd); fds.delete(fd); },
    read(fd, buf, offset, length, position) {
      const f = getFd(fd), n = f.node;
      if (isDir(n)) throw fsErr("EISDIR");
      const pos = position ?? f.pos;
      const cnt = Math.max(0, Math.min(length, n.size - pos));
      buf.set(n.data.subarray(pos, pos + cnt), offset);
      if (position == null) f.pos += cnt;
      n.atime = Date.now();
      return cnt;
    },
    write(fd, buf, offset, length, position) {
      if (fd === 1 || fd === 2) {
        onOutput(fd, dec.decode(buf.subarray(offset, offset + length)));
        return length;
      }
      const f = getFd(fd), n = f.node;
      if ((f.flags & 3) === 0) throw fsErr("EBADF");
      let pos = position ?? ((f.flags & constants.O_APPEND) ? n.size : f.pos);
      ensureCap(n, pos + length);
      n.data.set(buf.subarray(offset, offset + length), pos);
      n.size = Math.max(n.size, pos + length);
      if (position == null) f.pos = pos + length;
      n.mtime = Date.now();
      touch(f.path);
      return length;
    },
    fstat(fd) { return stat(getFd(fd).node); },
    stat(path) { return stat(must(path).node); },
    lstat(path) { return stat(must(path, false).node); },
    readdir(path) {
      const { node } = must(path);
      if (!isDir(node)) throw fsErr("ENOTDIR", path);
      return [...node.children.keys()];
    },
    mkdir(path, perm) {
      const { node, parent, name } = resolve(path);
      if (node) throw fsErr("EEXIST", path);
      if (!parent) throw fsErr("EEXIST", path);
      parent.children.set(name, newNode(S_IFDIR | (perm & 0o7777 & ~UMASK), { children: new Map() }));
      touch(path);
    },
    rmdir(path) {
      const { node, parent, name } = must(path, false);
      if (!isDir(node)) throw fsErr("ENOTDIR", path);
      if (node.children.size) throw fsErr("ENOTEMPTY", path);
      parent.children.delete(name);
      touch(path);
    },
    unlink(path) {
      const { node, parent, name } = must(path, false);
      if (isDir(node)) throw fsErr("EISDIR", path);
      parent.children.delete(name);
      node.nlink--;
      touch(path);
    },
    rename(from, to) {
      const src = must(from, false);
      const dst = resolve(to, false);
      if (!dst.parent) throw fsErr("EBUSY", to);
      if (dst.node && isDir(dst.node) && dst.node.children.size) throw fsErr("ENOTEMPTY", to);
      src.parent.children.delete(src.name);
      dst.parent.children.set(dst.name, src.node);
      touch(from);
      touch(to);
    },
    link(from, to) {
      const src = must(from);
      const dst = resolve(to);
      if (dst.node) throw fsErr("EEXIST", to);
      dst.parent.children.set(dst.name, src.node);
      src.node.nlink++;
      touch(to);
    },
    symlink(target, path) {
      const dst = resolve(path, false);
      if (dst.node) throw fsErr("EEXIST", path);
      dst.parent.children.set(dst.name, newNode(S_IFLNK | 0o777, { target }));
      touch(path);
    },
    readlink(path) {
      const { node } = must(path, false);
      if (!isLink(node)) throw fsErr("EINVAL", path);
      return node.target;
    },
    chmod(path, mode) { const { node } = must(path); node.mode = (node.mode & S_IFMT) | (mode & 0o7777); touch(path); },
    fchmod(fd, mode) { const f = getFd(fd), n = f.node; n.mode = (n.mode & S_IFMT) | (mode & 0o7777); touch(f.path); },
    chown(path, uid, gid) { const { node } = must(path); node.uid = uid; node.gid = gid; },
    fchown(fd, uid, gid) { const n = getFd(fd).node; n.uid = uid; n.gid = gid; },
    lchown(path, uid, gid) { const { node } = must(path, false); node.uid = uid; node.gid = gid; },
    utimes(path, atime, mtime) { const { node } = must(path); node.atime = atime * 1000; node.mtime = mtime * 1000; touch(path); },
    truncate(path, len) { const { node } = must(path); ensureCap(node, len); if (len > node.size) node.data.fill(0, node.size, len); node.size = len; touch(path); },
    ftruncate(fd, len) { const f = getFd(fd), n = f.node; ensureCap(n, len); if (len > n.size) n.data.fill(0, n.size, len); n.size = len; touch(f.path); },
    fsync() {},
  };

  const fs = { constants };
  for (const [name, fn] of Object.entries(ops)) {
    fs[name] = (...args) => {
      const cb = args.pop();
      let res, err = null;
      try { res = fn(...args); } catch (e) { err = e; }
      if (name === "read" || name === "write") cb(err, res ?? 0);
      else cb(err, res);
    };
  }
  fs.writeSync = (fd, buf) => ops.write(fd, buf, 0, buf.length, null);
  fs.readFileSync = (path) => { const { node } = must(path); return node.data.slice(0, node.size); };
  fs.writeFileSync = (path, data) => {
    const fd = ops.open(path, constants.O_WRONLY | constants.O_CREAT | constants.O_TRUNC, 0o644);
    const b = typeof data === "string" ? new TextEncoder().encode(data) : data;
    ops.write(fd, b, 0, b.length, null);
    ops.close(fd);
  };
  // process.cwd/chdir: Go's os.Chdir and os.Getwd end up here.
  fs.cwd = () => cwd;
  fs.chdir = (path) => {
    const { node } = must(path);
    if (!isDir(node)) throw fsErr("ENOTDIR", path);
    cwd = "/" + split(path).join("/");
  };
  fs.mkdirSync = (path) => {
    let cur = "";
    for (const part of split(path)) {
      cur += "/" + part;
      if (!resolve(cur).node) ops.mkdir(cur, 0o755);
    }
  };
  // restore puts saved records (state.js) back, parents first, without
  // saving them again.
  fs.restore = (records) => {
    const keep = persist;
    persist = null;
    for (const r of records) {
      try {
        const { node, parent, name } = resolve(r.path, false);
        if (!parent) continue;
        let n = node;
        if (!n) {
          const fmt = r.mode & S_IFMT;
          if (fmt === S_IFDIR) n = newNode(r.mode, { children: new Map() });
          else if (fmt === S_IFLNK) n = newNode(r.mode, { target: r.target });
          else n = newNode(r.mode, { data: r.data, size: r.data.length });
          parent.children.set(name, n);
        }
        n.mtime = r.mtime;
      } catch { /* its parent is gone */ }
    }
    persist = keep;
  };
  return fs;
}

// installFS puts the file system on globalThis.fs. home (optional):
// {records, save(put, del)} keeps /root, see state.js.
export function installFS(onOutput, home = null) {
  const fs = createFS({
    cwd: "/root",
    onOutput,
    // the kubeconfig belongs to the cluster of this visit
    persist: home && { prefix: "/root", skip: ["/root/.kube"], save: home.save },
  });
  fs.mkdirSync("/tmp");
  fs.mkdirSync("/root");
  if (home) fs.restore(home.records);
  globalThis.fs = fs;
  globalThis.process ??= {};
  Object.assign(globalThis.process, {
    getuid: () => 0, getgid: () => 0, geteuid: () => 0, getegid: () => 0,
    getgroups: () => [0], pid: 1, ppid: 0, umask: () => UMASK,
    cwd: () => fs.cwd(), chdir: (path) => fs.chdir(path),
  });
  return fs;
}
