// Tiny in-memory file system for Go's js/wasm syscall layer.
// Go expects a subset of the Node.js callback based fs API on globalThis.fs.
// Nothing is persisted: a reload starts a fresh cluster.

const S_IFMT = 0o170000, S_IFREG = 0o100000, S_IFDIR = 0o040000, S_IFLNK = 0o120000;
const constants = {
  O_RDONLY: 0, O_WRONLY: 1, O_RDWR: 2, O_CREAT: 0o100, O_EXCL: 0o200, O_TRUNC: 0o1000,
  O_APPEND: 0o2000, O_DIRECTORY: 0o200000, O_SYNC: 0o4010000,
};

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

export function createFS({ cwd = "/", onOutput = () => {} } = {}) {
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
        node = newNode(S_IFREG | (mode & 0o7777), { data: new Uint8Array(0), size: 0 });
        parent.children.set(name, node);
        parent.mtime = Date.now();
      }
      if (isDir(node) && (flags & 3) !== 0) throw fsErr("EISDIR", path);
      if (!isDir(node) && (flags & constants.O_DIRECTORY)) throw fsErr("ENOTDIR", path);
      if ((flags & constants.O_TRUNC) && !isDir(node)) { node.size = 0; node.mtime = Date.now(); }
      const fd = nextFd++;
      fds.set(fd, { node, pos: 0, flags });
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
      parent.children.set(name, newNode(S_IFDIR | (perm & 0o7777), { children: new Map() }));
    },
    rmdir(path) {
      const { node, parent, name } = must(path, false);
      if (!isDir(node)) throw fsErr("ENOTDIR", path);
      if (node.children.size) throw fsErr("ENOTEMPTY", path);
      parent.children.delete(name);
    },
    unlink(path) {
      const { node, parent, name } = must(path, false);
      if (isDir(node)) throw fsErr("EISDIR", path);
      parent.children.delete(name);
      node.nlink--;
    },
    rename(from, to) {
      const src = must(from, false);
      const dst = resolve(to, false);
      if (!dst.parent) throw fsErr("EBUSY", to);
      if (dst.node && isDir(dst.node) && dst.node.children.size) throw fsErr("ENOTEMPTY", to);
      src.parent.children.delete(src.name);
      dst.parent.children.set(dst.name, src.node);
    },
    link(from, to) {
      const src = must(from);
      const dst = resolve(to);
      if (dst.node) throw fsErr("EEXIST", to);
      dst.parent.children.set(dst.name, src.node);
      src.node.nlink++;
    },
    symlink(target, path) {
      const dst = resolve(path, false);
      if (dst.node) throw fsErr("EEXIST", path);
      dst.parent.children.set(dst.name, newNode(S_IFLNK | 0o777, { target }));
    },
    readlink(path) {
      const { node } = must(path, false);
      if (!isLink(node)) throw fsErr("EINVAL", path);
      return node.target;
    },
    chmod(path, mode) { const { node } = must(path); node.mode = (node.mode & S_IFMT) | (mode & 0o7777); },
    fchmod(fd, mode) { const n = getFd(fd).node; n.mode = (n.mode & S_IFMT) | (mode & 0o7777); },
    chown(path, uid, gid) { const { node } = must(path); node.uid = uid; node.gid = gid; },
    fchown(fd, uid, gid) { const n = getFd(fd).node; n.uid = uid; n.gid = gid; },
    lchown(path, uid, gid) { const { node } = must(path, false); node.uid = uid; node.gid = gid; },
    utimes(path, atime, mtime) { const { node } = must(path); node.atime = atime * 1000; node.mtime = mtime * 1000; },
    truncate(path, len) { const { node } = must(path); ensureCap(node, len); if (len > node.size) node.data.fill(0, node.size, len); node.size = len; },
    ftruncate(fd, len) { const n = getFd(fd).node; ensureCap(n, len); if (len > n.size) n.data.fill(0, n.size, len); n.size = len; },
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
  fs.mkdirSync = (path) => {
    let cur = "";
    for (const part of split(path)) {
      cur += "/" + part;
      if (!resolve(cur).node) ops.mkdir(cur, 0o755);
    }
  };
  return fs;
}

export function installFS(onOutput) {
  const fs = createFS({ cwd: "/root", onOutput });
  fs.mkdirSync("/tmp");
  fs.mkdirSync("/root");
  globalThis.fs = fs;
  globalThis.process ??= {};
  Object.assign(globalThis.process, {
    getuid: () => 0, getgid: () => 0, geteuid: () => 0, getegid: () => 0,
    getgroups: () => [0], pid: 1, ppid: 0, umask: () => 0o022,
    cwd: () => "/root", chdir: () => {},
  });
  return fs;
}
