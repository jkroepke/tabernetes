// vmstate keeps the saved states of booted pod VMs (vmworker.js) in the
// origin private file system: tabernetes/vm/<build>-<memory in MiB>.state.
// A pod boots the kernel only when there is no state for its memory size
// yet; it saves one when init waits for the pod (vm/init/stage0.go). Every
// later pod with that memory size restores the state instead, in every tab.
//
// build is a hash of v86, the kernel and init: a new release makes new
// states and deletes the old ones. forget deletes all of them (state.js).

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

// loadState returns the saved state, or null.
export async function loadState(build, memory) {
  try {
    const f = await (await (await dir(false)).getFileHandle(fileName(build, memory))).getFile();
    return await f.arrayBuffer();
  } catch {
    return null;
  }
}

// saveState stores a state and deletes the states of other builds. Two pods
// may save at once: the file is replaced as a whole when the write closes.
export async function saveState(build, memory, state) {
  const d = await dir(true);
  const w = await (await d.getFileHandle(fileName(build, memory), { create: true })).createWritable();
  await w.write(state);
  await w.close();
  for await (const name of d.keys()) {
    if (name.endsWith(".state") && !name.startsWith(build + "-")) await d.removeEntry(name).catch(() => {});
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
