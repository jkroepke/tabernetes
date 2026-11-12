// node snapshot.mjs <tabernetes.wasm> <wasm_exec.js> <fs.js>
//
// Saves the Go heap after package init into the binary itself, like the
// saved VM states of the pods (web/src/vmstate.js). The init of all the
// Kubernetes packages takes ~0.9 s in the browser, in every tab and in the
// shared worker, half of it for compiling code that runs only once. With the
// snapshot, main runs after a few milliseconds.
//
// How: main sleeps when globalThis.tabernetesSnapshot is set
// (k8s/tabernetes/main.go). The Go scheduler then has nothing to run and
// returns to JavaScript. At that moment all state of the program is in the
// linear memory and the wasm globals, plus the few JavaScript values Go
// holds. This script runs the binary in Node.js up to that point and writes
// a new binary: the memory becomes its data, the globals start with their
// saved values, and a custom section "tabernetes.snapshot" says how
// web/src/wasm.js rebuilds the JavaScript side and resumes the program.
//
// The JavaScript values Go holds must be found again by name in the page,
// so the build fails if Go holds anything else (a js.FuncOf, an object it
// created) after init.
import fs from "node:fs";
import vm from "node:vm";
import { pathToFileURL } from "node:url";

const [wasmPath, wasmExecPath, fsPath] = process.argv.slice(2);
const SECTION = "tabernetes.snapshot";
// Must be the env of web/src/terminal.js and web/src/cluster.js: Go reads it
// during init, and its strings point into the memory of the snapshot.
const ENV = { HOME: "/root", USER: "root", PATH: "/usr/bin" };
// The names under which Go finds the values it holds, in this order.
const NAMES = ["Object", "Array", "Uint8Array", "process", "path", "fs", "fetch", "crypto", "Date", "performance", "TextEncoder", "TextDecoder"];

// ---- the wasm binary format, as far as needed ----
const readLEB = (b, p) => {
  let r = 0, s = 0, x;
  do { x = b[p++]; r += (x & 0x7f) * 2 ** s; s += 7; } while (x & 0x80);
  return [r, p];
};
const readSLEB = (b, p) => {
  let r = 0n, s = 0n, x;
  do { x = b[p++]; r |= BigInt(x & 0x7f) << s; s += 7n; } while (x & 0x80);
  if (x & 0x40) r -= 1n << s;
  return [r, p];
};
const uLEB = (n) => {
  const o = [];
  do { let x = n % 128; n = Math.floor(n / 128); if (n) x |= 0x80; o.push(x); } while (n);
  return Buffer.from(o);
};
const sLEB = (n) => {
  n = BigInt(n);
  const o = [];
  for (;;) {
    const x = Number(n & 0x7fn);
    n >>= 7n;
    if ((n === 0n && !(x & 0x40)) || (n === -1n && x & 0x40)) { o.push(x); break; }
    o.push(x | 0x80);
  }
  return Buffer.from(o);
};
const name = (s) => Buffer.concat([uLEB(Buffer.byteLength(s)), Buffer.from(s)]);
const section = (id, body) => Buffer.concat([Buffer.from([id]), uLEB(body.length), body]);

function sections(b) {
  if (b.readUInt32BE(0) !== 0x0061736d) throw new Error("not a wasm binary");
  const out = [];
  for (let p = 8; p < b.length;) {
    const id = b[p];
    const [size, q] = readLEB(b, p + 1);
    out.push({ id, body: b.subarray(q, q + size) });
    p = q + size;
  }
  return out;
}

// globals parses the global section: [{type, mut}].
function globals(body) {
  let [n, p] = readLEB(body, 0);
  const g = [];
  while (n--) {
    const type = body[p], mut = body[p + 1];
    p += 2;
    const op = body[p++];
    if ((type === 0x7f && op !== 0x41) || (type === 0x7e && op !== 0x42)) throw new Error("unexpected global initializer");
    p = readSLEB(body, p)[1];
    if (body[p++] !== 0x0b) throw new Error("unexpected global initializer");
    g.push({ type, mut });
  }
  return g;
}

// ---- run the program up to main ----
const wasm = fs.readFileSync(wasmPath);
const secs = sections(wasm);
if (secs.some((s) => s.id === 0 && readName(s.body) === SECTION)) throw new Error("the binary has a snapshot already");
function readName(body) {
  const [n, p] = readLEB(body, 0);
  return body.subarray(p, p + n).toString();
}
const gsec = secs.find((s) => s.id === 6);
const types = globals(gsec.body);

// The same binary with every global exported as g<i>, to read them.
const probe = Buffer.concat([wasm.subarray(0, 8), ...secs.map((s) => {
  if (s.id !== 7) return section(s.id, s.body);
  const [n, p] = readLEB(s.body, 0);
  const extra = types.map((_, i) => Buffer.concat([name(`g${i}`), Buffer.from([3]), uLEB(i)]));
  return section(7, Buffer.concat([uLEB(n + types.length), s.body.subarray(p), ...extra]));
})]);

// A page has no process object: wasm_exec.js puts a stub there and fs.js
// adds to it. Node's own process is read-only in parts, so hide it.
const nodeProcess = process;
Object.defineProperty(globalThis, "process", { value: undefined, configurable: true, writable: true });
vm.runInThisContext(fs.readFileSync(wasmExecPath, "utf8"), { filename: wasmExecPath });
const { installFS } = await import(pathToFileURL(fsPath).href);
installFS(() => {});
// Files that init leaves open would be missing from the fs of the page.
let open = 0;
for (const [fn, d] of [["open", 1], ["close", -1]]) {
  const f = globalThis.fs[fn];
  globalThis.fs[fn] = (...a) => {
    const cb = a[a.length - 1];
    a[a.length - 1] = (err, ...r) => { if (!err) open += d; cb(err, ...r); };
    return f.apply(globalThis.fs, a);
  };
}
const constants = JSON.stringify(globalThis.fs.constants);

const go = new globalThis.Go();
go.env = ENV;
globalThis.tabernetesSnapshot = true;
const { instance } = await WebAssembly.instantiate(probe, go.importObject);
const t0 = performance.now();
go.run(instance); // returns at the first pause, the sleep in main
const ms = performance.now() - t0;
if (go.exited) throw new Error("the program exited before main paused");
if (open) throw new Error(`init left ${open} files open`);
// Nothing may run on: the snapshot is the state right now.
for (const t of go._scheduledTimeouts.values()) clearTimeout(t);
if (go._scheduledTimeouts.size !== 1) throw new Error(`expected the one timer of the sleep in main, got ${go._scheduledTimeouts.size}`);

const mem = new Uint8Array(instance.exports.mem.buffer).slice();
const gvals = types.map((_, i) => instance.exports[`g${i}`].value);

// The JavaScript values Go holds, by the name the page finds them with.
const values = [];
const found = [];
const lookup = (v) => {
  for (const k of NAMES) if (globalThis[k] === v) return [k];
  for (const [p, o] of found) {
    for (const k of Object.getOwnPropertyNames(o)) {
      try { if (o[k] === v) return [...p, k]; } catch { /* a getter that throws */ }
    }
  }
  return null;
};
for (let id = 7; id < go._values.length; id++) {
  const v = go._values[id];
  if (v === undefined && go._idPool.includes(id)) { values.push(null); continue; }
  const path = lookup(v);
  if (!path) throw new Error(`Go holds a JavaScript value that the page cannot find by name: ${typeof v} ${Object.prototype.toString.call(v)}`);
  if (v && (typeof v === "object" || typeof v === "function")) found.push([path, v]);
  values.push(path);
}
const meta = {
  values,
  refs: go._goRefCounts.slice(7),
  idPool: go._idPool,
  nextTimeout: go._nextCallbackTimeoutID,
  env: ENV,
  constants,
};

// ---- write the new binary ----
const PAGE = 65536, BLOCK = 4096, MAX_SEGMENTS = 100000; // the limit of V8
const segments = [];
for (let at = 0; at < mem.length; at += BLOCK) {
  if (!mem.subarray(at, at + BLOCK).some((x) => x)) continue;
  const last = segments[segments.length - 1];
  if (last && last.end === at) last.end = at + BLOCK;
  else segments.push({ start: at, end: at + BLOCK });
}
if (segments.length > MAX_SEGMENTS) throw new Error(`too many data segments: ${segments.length}`);
const data = Buffer.concat([uLEB(segments.length), ...segments.flatMap(({ start, end }) => {
  // trailing zeros are zero anyway
  while (end > start && !mem[end - 1]) end--;
  return [Buffer.from([0x00, 0x41]), sLEB(start), Buffer.from([0x0b]), uLEB(end - start), Buffer.from(mem.subarray(start, end))];
})]);
const global = Buffer.concat([uLEB(types.length), ...types.map(({ type, mut }, i) =>
  Buffer.concat([Buffer.from([type, mut, type === 0x7f ? 0x41 : 0x42]), sLEB(gvals[i]), Buffer.from([0x0b])]))]);

const out = [wasm.subarray(0, 8)];
for (const s of secs) {
  let body = s.body;
  if (s.id === 5) {
    // the memory starts as big as the saved one
    const [n, p] = readLEB(body, 0);
    if (n !== 1) throw new Error("expected one memory");
    const flags = body[p];
    let q = readLEB(body, p + 1)[1];
    const max = flags & 1 ? body.subarray(q) : Buffer.alloc(0);
    body = Buffer.concat([uLEB(1), Buffer.from([flags]), uLEB(mem.length / PAGE), max]);
  } else if (s.id === 6) body = global;
  else if (s.id === 11) body = data;
  else if (s.id === 12) body = uLEB(segments.length);
  out.push(section(s.id, body));
}
out.push(section(0, Buffer.concat([name(SECTION), Buffer.from(JSON.stringify(meta))])));
const result = Buffer.concat(out);
await WebAssembly.compile(result); // fails here, not in the browser
fs.writeFileSync(wasmPath, result);
console.log(`snapshot: init took ${Math.round(ms)} ms, ${(mem.length / 2 ** 20).toFixed(0)} MiB of memory in ${segments.length} segments, Go holds ${values.map((p) => p?.join(".")).join(", ")}`);
nodeProcess.exit(0); // the Go program would go on in the background
