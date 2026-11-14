// The JIT modules of the kernel of the pods (jit.bin, made by web/jit.mjs).
//
// The JIT of v86 compiles a page of code once the interpreter has run it
// 750k instructions. The kernel of a pod spreads over ~400 pages, so a pod
// start ran most of the kernel in the interpreter (nginx: 55 % of the
// instructions, 69 % of the time). But every pod restores the same VM state:
// the kernel has the same code at the same physical pages in every pod.
// jit.bin holds the modules of the JIT for the hottest kernel code. The tab
// compiles them once, like v86.wasm (vm.js), and every pod installs them
// after the restore (vmworker.js): the kernel runs as JIT code from the
// first instruction, and V8 compiles and optimises each module once per
// tab, not once per pod.
//
// The file: a line "tabernetes-jit <build>\n", padded with spaces to 32
// bytes, then the modules as jit_export of v86 packs them. build is the ID of
// the saved states (vmstate.js), of the v86 version, kernel and init that
// made the file: a pod installs the modules only for the same build. v86
// also checks its own build and the bytes of every page, so a module that
// does not fit is never used.

const HEADER = 32;
const V86_MAGIC = 0x4a363876; // "v86J", the version 1 format of jit_export

// parseJIT compiles the modules of jit.bin. The result can be posted to the
// workers of the pods; null if the file is not one.
export async function parseJIT(buf) {
  const line = new TextDecoder().decode(new Uint8Array(buf, 0, Math.min(HEADER, buf.byteLength)));
  const m = line.match(/^tabernetes-jit (\S+)\n/);
  if (!m || buf.byteLength < HEADER + 16) return null;
  const head = new Uint32Array(buf, HEADER, 4);
  if (head[0] !== V86_MAGIC || head[1] !== 1) return null;
  const modules = [];
  let off = HEADER + 16;
  for (let i = 0; i < head[3]; i++) {
    const [page, index, flags, h1, h2, k, len] = new Uint32Array(buf, off, 7);
    const entries = new Uint32Array(buf.slice(off + 28, off + 28 + 4 * k));
    off += 28 + 4 * k;
    modules.push({ page, index, flags, hash: [h1, h2], entries, module: WebAssembly.compile(new Uint8Array(buf, off, len)) });
    off += (len + 3) & ~3;
  }
  for (const mod of modules) mod.module = await mod.module;
  warm(modules);
  return { build: m[1], layout: head[2], modules };
}

// warm gets the modules optimised before a pod needs them. V8 compiles a
// function of a wasm module with Liftoff at its first call, and with
// TurboFan (on other threads) once it has used up its tiering budget, which
// only normal returns and loop back edges count down. In the first pod that
// cost ~0.8 s of CPU (headless Chromium, 50 modules). So each function is
// called here, in an instance of its own with a scratch memory
// whose imports throw, until V8 tiers it up: it reads zeroes as the TLB of
// the guest, so it leaves at its first memory access, as if the page of the
// access were not mapped (headless Chromium, 2026-11-14: 1000 calls tiered
// 47 of 50 modules up, 2000 all of them; 50 x 3000 calls take ~40 ms). The
// optimised code stays with the Module, which the tab posts to the workers
// of its pods. The scratch memory must hold the static data of v86.wasm (~17
// MiB): an access beyond it would end the calls of the module with a trap.
const SCRATCH_PAGES = 512; // 32 MiB
const CALLS = 3000;
async function warm(modules) {
  const memory = new WebAssembly.Memory({ initial: SCRATCH_PAGES });
  const stop = () => { throw new Error("stop"); };
  const e = new Proxy({}, { get: (_, k) => (k === "m" ? memory : stop) });
  for (const { module } of modules) {
    try {
      const f = new WebAssembly.Instance(module, { e }).exports.f;
      for (let i = 0; i < CALLS; i++) f(0);
    } catch {
      // an import or a trap: thrown calls do not count for the tier-up
    }
    // the tab stays responsive
    await new Promise((r) => setTimeout(r, 0));
  }
}

// packJIT puts the header of build in front of the export of v86.
export function packJIT(build, exported) {
  const line = `tabernetes-jit ${build}\n`.padEnd(HEADER, " ");
  const out = new Uint8Array(HEADER + exported.byteLength);
  out.set(new TextEncoder().encode(line));
  out.set(new Uint8Array(exported), HEADER);
  return out;
}

// importJIT installs the modules in the VM of a pod: after restore_state, or
// after the boot, before the pod runs. It returns the number of modules.
export function importJIT(e, jit, build) {
  if (!jit || !build || jit.build !== build || !e.jit_import) return 0;
  try {
    return e.jit_import(jit);
  } catch (err) {
    console.warn("tabernetes: the JIT modules of the kernel:", err);
    return 0;
  }
}
