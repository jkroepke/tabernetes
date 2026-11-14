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
  compileNow(modules);
  return { build: m[1], layout: head[2], modules };
}

// V8 compiles a function of a wasm module at its first call. That would be
// in the worker of the first pod, on its critical path (~100 ms in Chromium
// for 200 modules). So each function is called once here, in an instance of
// its own with a scratch memory whose imports throw: it ends at its first
// memory access outside the scratch memory, its first import or after its
// loop limit. The code it ran is thrown away; the compiled code stays with
// the Module.
function compileNow(modules) {
  const memory = new WebAssembly.Memory({ initial: 64 });
  const stop = () => { throw new Error("stop"); };
  const e = new Proxy({}, { get: (_, k) => (k === "m" ? memory : stop) });
  for (const { module } of modules) {
    try {
      new WebAssembly.Instance(module, { e }).exports.f(0);
    } catch {
      // the expected end
    }
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
