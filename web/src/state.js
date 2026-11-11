// What Tabernetes keeps in the browser, so that a reload finds it again:
//
//   the cluster    IndexedDB "tabernetes", store "cluster": one record with
//                  the PKI and an etcd snapshot (k8s/tabernetes/state.go)
//   /root          IndexedDB "tabernetes", store "home": a record per file of
//                  the terminal (fs.js)
//   the images     OPFS tabernetes/layers (layers.js)
//
// IndexedDB and OPFS share the quota of the origin, a part of the free disk.
// localStorage would hold a few MB of strings only. forget deletes all of it.

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
      // forget in another tab deletes the database: let it.
      r.result.onversionchange = () => { r.result.close(); opening = null; };
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
  let running = null, next = null, dropped = false;
  const write = async () => {
    while (next) {
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
      running ??= write().finally(() => { running = null; });
      return running;
    },
    async drop() {
      dropped = true;
      await running;
      await tx("cluster", "readwrite", (s) => s.delete("cluster")).catch(() => {});
    },
  };
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

// forget deletes what this browser keeps: the cluster, the files and the
// images. Saves stop in this context; others stop when the database is gone.
export async function forget() {
  forgotten = true;
  const d = await opening?.catch(() => null);
  d?.close();
  opening = null;
  await done(indexedDB.deleteDatabase(DB)).catch(() => {});
  try {
    const root = await navigator.storage.getDirectory();
    await root.removeEntry("tabernetes", { recursive: true });
  } catch { /* no OPFS, or a running pod still reads a layer */ }
}
