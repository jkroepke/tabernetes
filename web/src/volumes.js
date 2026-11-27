// Persistent volumes of the tab pods (k8s/tabernetes/provision.go) in the
// origin private file system: tabernetes/volumes/<PV name>/. A volume has
// an index of its files (path, type, mode, owner, time, size, the blob with
// the data or the target of a link) and one blob per file. The index has
// two slots, index-0.json and index-1.json, written in turn with a growing
// generation: a write that breaks off (a tab that closes) leaves the other
// one. Blobs are new for every write of a file, so an index never points to
// a blob that a later write changed; blobs that no index points to go.
//
// The pod worker (vmworker.js) loads a volume at the start of a pod and
// writes back what changed every few seconds and when the pod stops.

async function volumesDir(create) {
  const root = await navigator.storage.getDirectory();
  const t = await root.getDirectoryHandle("tabernetes", { create });
  return t.getDirectoryHandle("volumes", { create });
}

// removeVolume deletes the data of a volume (the provisioner, when its
// claim is gone).
export async function removeVolume(name) {
  let dir;
  try {
    dir = await volumesDir(false);
  } catch {
    return; // nothing kept
  }
  await dir.removeEntry(name, { recursive: true }).catch((e) => {
    if (e?.name !== "NotFoundError") throw e;
  });
}

// volumeNames lists the volumes that have data.
export async function volumeNames() {
  const out = [];
  try {
    for await (const name of (await volumesDir(false)).keys()) out.push(name);
  } catch { /* none */ }
  return out;
}

// forgetVolumes deletes all volumes (forget).
export async function forgetVolumes() {
  const root = await navigator.storage.getDirectory();
  const t = await root.getDirectoryHandle("tabernetes").catch(() => null);
  await t?.removeEntry("volumes", { recursive: true }).catch(() => {});
}

async function readFile(dir, name) {
  const f = await (await dir.getFileHandle(name)).getFile();
  return new Uint8Array(await f.arrayBuffer());
}

// writeFile writes a file with a sync access handle (in a worker: all
// browsers have it there).
async function writeFile(dir, name, data) {
  const h = await (await dir.getFileHandle(name, { create: true })).createSyncAccessHandle();
  try {
    h.truncate(0);
    let off = 0;
    while (off < data.length) off += h.write(data.subarray(off), { at: off });
    h.flush();
  } finally {
    h.close();
  }
}

// openVolume loads a volume: {files: Map path -> entry with data}, and a
// writer for later saves.
export async function openVolume(name) {
  const dir = await (await volumesDir(true)).getDirectoryHandle(name, { create: true });
  let index = { gen: 0, next: 0, files: {} };
  for (const slot of ["index-0.json", "index-1.json"]) {
    try {
      const i = JSON.parse(new TextDecoder().decode(await readFile(dir, slot)));
      if (i.gen >= index.gen && i.files) index = i;
    } catch { /* no such slot, or a broken one */ }
  }
  const files = new Map();
  for (const [path, e] of Object.entries(index.files)) {
    const f = { ...e };
    if (e.blob) {
      try {
        f.data = await readFile(dir, e.blob);
      } catch {
        f.data = new Uint8Array(0);
      }
    }
    files.set(path, f);
  }
  return { name, dir, index, files };
}

// saveVolume writes the entries of vol that changed. entries: Map path ->
// {type: "file"|"dir"|"link", mode, uid, gid, mtime, size, target,
// sig (what tells a change), read() for the data}.
export async function saveVolume(vol, entries) {
  const old = vol.index.files;
  const files = {};
  let changed = false;
  for (const [path, e] of entries) {
    const o = old[path];
    const meta = { type: e.type, mode: e.mode, uid: e.uid, gid: e.gid, mtime: e.mtime };
    if (e.type === "link") meta.target = e.target;
    if (e.type === "file") {
      meta.size = e.size;
      meta.sig = e.sig;
      if (o && o.type === "file" && o.blob && o.sig === e.sig && !e.dirty) {
        meta.blob = o.blob;
      } else {
        const blob = `b${vol.index.next++}`;
        await writeFile(vol.dir, blob, await e.read());
        meta.blob = blob;
      }
    }
    if (!o || JSON.stringify(o) !== JSON.stringify(meta)) changed = true;
    files[path] = meta;
  }
  if (!changed && Object.keys(old).length === entries.size) return false;
  const index = { gen: vol.index.gen + 1, next: vol.index.next, files };
  await writeFile(vol.dir, `index-${index.gen % 2}.json`, new TextEncoder().encode(JSON.stringify(index)));
  // blobs of neither slot go
  const keep = new Set([...Object.values(files), ...Object.values(old)].map((f) => f.blob).filter(Boolean));
  vol.index = index;
  for await (const name of vol.dir.keys()) {
    if (/^b\d+$/.test(name) && !keep.has(name)) await vol.dir.removeEntry(name).catch(() => {});
  }
  return true;
}
