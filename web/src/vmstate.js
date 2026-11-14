// vmstate keeps the saved states of booted pod VMs (vmworker.js) in the
// origin private file system: tabernetes/vm/<build>-<memory in MiB>.state.
// A pod boots the kernel only when there is no state for its memory size
// yet; it saves one when init waits for the pod (vm/init/init.c). Every
// later pod with that memory size restores the state instead, in every tab.
//
// build is a hash of v86, the kernel and init: a new release makes new
// states and deletes the old ones. forget deletes all of them (state.js).
// A state is written as <name>.<random>.part first and then moved to its
// name (saveState), so that nobody reads half of it.

async function dir(create) {
  const root = await navigator.storage.getDirectory();
  const t = await root.getDirectoryHandle("tabernetes", { create });
  return t.getDirectoryHandle("vm", { create });
}

const fileName = (build, memory) => `${build}-${memory >> 20}.state`;

// buildID returns the hash of what makes a state valid.
export async function buildID(version, kernel, init) {
  const parts = [new TextEncoder().encode(version + "\0"), kernel, init];
  const all = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let at = 0;
  for (const p of parts) { all.set(p, at); at += p.length; }
  const h = new Uint8Array(await crypto.subtle.digest("SHA-256", all));
  return [...h.subarray(0, 8)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// valid tells whether a file holds a whole state of v86: its header (magic,
// version, total length, length of the info block) and the length the header
// names. A state that v86 compressed starts with the magic of zstd instead.
// An empty file is not a state: v86 failed on it with "RangeError: Invalid
// typed array length: 1" and the pod booted after all.
const STATE_MAGIC = 0x86768676, ZSTD_MAGIC = 0xfd2fb528, HEADER = 16;
function valid(buf) {
  if (buf.byteLength < HEADER) return false;
  const h = new DataView(buf);
  if (h.getUint32(0, true) === ZSTD_MAGIC) return true;
  return h.getUint32(0, true) === STATE_MAGIC && h.getUint32(8, true) === buf.byteLength;
}

// loadState returns the saved state, or null. A file that holds no whole
// state counts as none, and it stays: the pod boots and saves a good state
// over it.
export async function loadState(build, memory) {
  try {
    const d = await dir(false);
    sweep(d, build, false).catch(() => {});
    const f = await (await d.getFileHandle(fileName(build, memory))).getFile();
    const buf = await f.arrayBuffer();
    return valid(buf) ? buf : null;
  } catch {
    return null;
  }
}

// A state is written to a file of its own (.part) and then moved to its
// name. Created with its name, the file would be there, empty, until the
// write closed: a pod that started in that time read 0 bytes, and a worker
// that ended in that time (its pod was stopped, for example after a failed
// pull) left the empty file for good. A .part file of a worker that ended
// is deleted by a later save once it is older than this.
const PART_MAX_AGE = 60 * 1000;

// saveState stores a state and deletes the states of other builds. Two pods
// may save at once: each moves a whole state into place.
export async function saveState(build, memory, state) {
  const d = await dir(true);
  const name = fileName(build, memory);
  const write = async (h) => {
    const w = await h.createWritable();
    await w.write(state);
    await w.close();
  };
  // move() of files in OPFS: Chrome 110, Firefox 111, Safari 16.4.
  const part = await d.getFileHandle(`${name}.${crypto.randomUUID()}.part`, { create: true });
  if (typeof part.move === "function") {
    try {
      await write(part);
      try {
        await part.move(d, name);
      } catch {
        // a browser that does not replace a file by a move
        await d.removeEntry(name).catch(() => {});
        await part.move(d, name);
      }
    } catch (e) {
      await d.removeEntry(part.name).catch(() => {});
      throw e;
    }
  } else {
    await d.removeEntry(part.name).catch(() => {});
    await write(await d.getFileHandle(name, { create: true }));
  }
  await sweep(d, build, true);
}

// sweep deletes the .part files of workers that ended during a save, and
// with others, the states and .part files of other builds.
async function sweep(d, build, others) {
  const now = Date.now();
  for await (const [n, h] of d.entries()) {
    const mine = n.startsWith(build + "-");
    if (n.endsWith(".state") && others && !mine) await d.removeEntry(n).catch(() => {});
    else if (n.endsWith(".part") && h.kind === "file") {
      const old = await h.getFile().then((f) => now - f.lastModified > PART_MAX_AGE, () => false);
      if (old || (others && !mine)) await d.removeEntry(n).catch(() => {});
    }
  }
}

// dropState deletes a state that did not restore.
export async function dropState(build, memory) {
  await (await dir(false)).removeEntry(fileName(build, memory)).catch(() => {});
}

// forgetStates deletes all states.
export async function forgetStates() {
  const root = await navigator.storage.getDirectory();
  const t = await root.getDirectoryHandle("tabernetes");
  await t.removeEntry("vm", { recursive: true });
}
