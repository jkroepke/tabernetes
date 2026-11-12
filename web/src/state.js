// What Tabernetes keeps in the browser, so that a reload finds it again:
//
//   the cluster    IndexedDB "tabernetes", store "cluster": one record with
//                  the PKI and an etcd snapshot (k8s/tabernetes/state.go),
//                  and, after the last tab closed, the hibernated control
//                  plane (hibernate.js, ~40 MB)
//   /root          IndexedDB "tabernetes", store "home": a record per file of
//                  the terminal (fs.js)
//   the images     OPFS tabernetes/layers (layers.js), their configs in the
//                  cache "tabernetes-configs" (vmworker.js), and the models
//                  of llm pods in the caches of WebLLM
//   the pod VMs    OPFS tabernetes/vm: saved states of booted VMs (vmstate.js)
//
// IndexedDB, OPFS and the caches share the quota of the origin, a part of the
// free disk. localStorage would hold a few MB of strings only. forget deletes
// all of it.

import { forgetStates } from "./vmstate.js";

const DB = "tabernetes";

let opening = null;
function db() {
  opening ??= new Promise((resolve, reject) => {
    const r = indexedDB.open(DB, 1);
    r.onupgradeneeded = () => {
      r.result.createObjectStore("cluster");
      r.result.createObjectStore("home");
    };
    r.onsuccess = () => {
      // forget in another tab deletes the database: let it, and do not
      // create it again with the next save.
      r.result.onversionchange = (e) => {
        if (e.newVersion === null) forgotten = true;
        r.result.close();
        opening = null;
      };
      resolve(r.result);
    };
    r.onerror = () => reject(r.error);
    r.onblocked = () => reject(new Error("the database is blocked"));
  });
  return opening;
}

const done = (req) => new Promise((resolve, reject) => {
  req.onsuccess = () => resolve(req.result);
  req.onerror = () => reject(req.error);
});

async function tx(store, mode, fn) {
  const t = (await db()).transaction(store, mode);
  const result = fn(t.objectStore(store));
  await new Promise((resolve, reject) => {
    t.oncomplete = resolve;
    t.onerror = t.onabort = () => reject(t.error);
  });
  return result instanceof IDBRequest ? result.result : result;
}

let forgotten = false;

// clusterState is globalThis.tabernetesState for the control plane: load()
// and save(bytes). A save while the last one still runs waits for it, a
// newer one replaces a waiting one. drop() deletes a cluster that did not
// boot, so that the next reload starts a new one.
export function clusterState() {
  let running = null, next = null, dropped = false, paused = false;
  const write = async () => {
    while (next && !paused) {
      const b = next;
      next = null;
      if (forgotten || dropped) return;
      await tx("cluster", "readwrite", (s) => s.put(b, "cluster")).catch((e) => console.warn("tabernetes: saving the cluster:", e));
    }
  };
  return {
    async load() {
      try {
        return (await tx("cluster", "readonly", (s) => s.get("cluster"))) ?? null;
      } catch (e) {
        console.warn("tabernetes: no saved cluster:", e);
        return null;
      }
    },
    save(bytes) {
      next = bytes;
      if (paused) return Promise.resolve();
      running ??= write().finally(() => { running = null; });
      return running;
    },
    // pause(true) holds the saves back while the control plane is
    // hibernated (cluster.js); pause(false) writes the last one held back.
    pause(on) {
      paused = on;
      if (!on && next) this.save(next);
    },
    async drop() {
      dropped = true;
      await running;
      await tx("cluster", "readwrite", (s) => s.delete("cluster")).catch(() => {});
    },
  };
}

// The hibernated control plane (hibernate.js) is a second record of the
// store "cluster": the image of the running program, its memory as a
// gzipped Blob. It is newer than the etcd snapshot, and good for one start
// only: takeHibernated deletes it as it reads it, and so does a tab that
// joins the running control plane, which may change the cluster after the
// image was taken.
export async function saveHibernated(image) {
  if (forgotten) return;
  await tx("cluster", "readwrite", (s) => s.put(image, "hibernated"));
}

export async function takeHibernated() {
  try {
    return (await tx("cluster", "readwrite", (s) => {
      const r = s.get("hibernated");
      s.delete("hibernated");
      return r;
    })) ?? null;
  } catch (e) {
    console.warn("tabernetes: no hibernated control plane:", e);
    return null;
  }
}

export async function dropHibernated() {
  await tx("cluster", "readwrite", (s) => s.delete("hibernated")).catch(() => {});
}

// loadHome returns the records of the files in /root:
// {path, mode, mtime, data?: Uint8Array, target?: string}, parents first.
export async function loadHome() {
  try {
    const all = await tx("home", "readonly", (s) => s.getAll());
    return all.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  } catch (e) {
    console.warn("tabernetes: no saved files:", e);
    return [];
  }
}

// saveHome writes the records of changed files and deletes the paths of
// removed ones, with everything below them.
export async function saveHome(put, del) {
  if (forgotten) return;
  await tx("home", "readwrite", (s) => {
    for (const p of del) {
      s.delete(p);
      s.delete(IDBKeyRange.bound(p + "/", p + "/￿"));
    }
    for (const r of put) s.put(r, r.path);
  });
}

// keepStorage asks the browser to keep what Tabernetes saved. Without it the
// storage of the origin is "best-effort": under disk pressure the browser may
// delete the cluster, /root and the image layers without a word. Chrome
// decides by itself (bookmarked or often used sites), Safari grants it to
// sites in use, Firefox asks the user. It asks once the cluster runs, not on
// page load, and not again once granted. Only a page can ask, not a worker.
export async function keepStorage() {
  try {
    if (!navigator.storage?.persist || (await navigator.storage.persisted())) return;
    if (!(await navigator.storage.persist())) console.info("tabernetes: the browser keeps the saved cluster and images as best-effort storage only");
  } catch { /* no Storage API */ }
}

// The Web Locks of a layer (layers.js): a pod holds the first one, shared,
// for as long as it runs with the layer. A pull holds the second one while it
// unpacks the layer.
export const useLock = (digest) => `tabernetes-image-${digest}`;
export const unpackLock = (digest) => `tabernetes-layer-${digest}`;

// removeLayer deletes the files of a layer (layers.js), unless a pod in any
// tab runs with it or a pull unpacks it. It reports whether the layer is gone.
export async function removeLayer(dir, name) {
  const rm = async () => {
    for (const ext of ["json", "tar", "used", "forgotten"]) await dir.removeEntry(`${name}.${ext}`).catch(() => {});
    return true;
  };
  if (!navigator.locks) return rm();
  const digest = name.replace("-", ":");
  return !!(await navigator.locks.request(useLock(digest), { ifAvailable: true }, (free) => free &&
    navigator.locks.request(unpackLock(digest), { ifAvailable: true }, (idle) => idle && rm())));
}

async function layersDir() {
  try {
    const root = await navigator.storage.getDirectory();
    return await (await root.getDirectoryHandle("tabernetes")).getDirectoryHandle("layers");
  } catch {
    return null; // no OPFS, or no layers
  }
}

async function layerNames(dir, ext) {
  const names = new Set();
  for await (const name of dir.keys()) {
    const m = name.match(/^(.+)\.(tar|json|used|forgotten)$/);
    if (m && (!ext || m[2] === ext)) names.add(m[1]);
  }
  return [...names];
}

// forgetLayers deletes the images and returns how many layers are left. A
// pod reads its files from the layers for as long as it runs, so the layers
// of a running pod stay and get a mark: sweepLayers deletes them later.
async function forgetLayers() {
  const dir = await layersDir();
  if (!dir) return 0;
  let kept = 0;
  for (const name of await layerNames(dir)) {
    if (await removeLayer(dir, name).catch(() => false)) continue;
    kept++;
    await dir.getFileHandle(`${name}.forgotten`, { create: true }).catch(() => {});
  }
  if (!kept) {
    const root = await navigator.storage.getDirectory();
    await root.removeEntry("tabernetes", { recursive: true }).catch(() => {});
  }
  return kept;
}

// sweepLayers deletes the layers that forget had to keep, once their pods
// are gone. The page runs it when it loads, and so does the image garbage
// collection.
export async function sweepLayers() {
  const dir = await layersDir();
  if (!dir) return;
  for (const name of await layerNames(dir, "forgotten")) await removeLayer(dir, name).catch(() => {});
}

// forget deletes what this browser keeps: the cluster, the files, the images
// and the models. Saves stop in this context, and in the others when they
// see the database go. It returns the number of image layers that running
// pods still read: these go later (sweepLayers).
export async function forget() {
  forgotten = true;
  const d = await opening?.catch(() => null);
  d?.close();
  opening = null;
  await done(indexedDB.deleteDatabase(DB)).catch(() => {});
  try {
    for (const name of await caches.keys()) {
      if (name === "tabernetes-configs" || name.startsWith("webllm/")) await caches.delete(name);
    }
  } catch { /* no Cache Storage */ }
  await forgetStates().catch(() => {});
  return forgetLayers().catch(() => 0);
}
