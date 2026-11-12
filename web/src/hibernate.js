// Hibernation of the control plane: when the last tab closes, the shared
// worker (cluster.js) saves the whole running Go program, and the next visit
// resumes it instead of booting kube-apiserver again (~5 s).
//
// It is the heap snapshot of the build (k8s/snapshot.mjs) taken at run time.
// Between two JavaScript tasks Go does not run: the scheduler has handed
// control back and waits for a timer or an event. At that moment all of
// the program is in the linear memory, the wasm globals (exported as g<i>
// by the build), the JavaScript values Go holds (go._values) and the files
// of the in-memory file system (fs.js). capture copies all of it in one
// task. resume puts it back into a fresh instance of the same binary.
//
// The JavaScript values are the hard part. Each needs a way to come back:
//   - globals of the page (fs, Object, tabernetesState, ...) by name,
//   - the callbacks of js.FuncOf by their Go id (a wrapper of wasm_exec.js),
//   - objects and arrays that Go built, rebuilt from their parts,
//   - byte arrays as a copy, a Promise as a resolved one,
//   - everything else (a MessagePort of a closed tab, an RTCDataChannel)
//     as a dead stand-in: calls on it do nothing. Its tab is gone anyway.

const PAGE = 65536;
const CHUNK = 65536;

// trackFuncs makes the wrappers of js.FuncOf known by their Go id. It must
// run before the program starts.
export function trackFuncs(go) {
  go._funcIDs = new WeakMap();
  const make = go._makeFuncWrapper;
  go._makeFuncWrapper = function (id) {
    const f = make.call(this, id);
    this._funcIDs.set(f, id);
    return f;
  };
}

// isPlain: an object that Go built with Object.New, or a record of fs.js.
const isPlain = (v) => {
  const p = Object.getPrototypeOf(v);
  return p === Object.prototype || p === null;
};

// capture returns the image of the paused program. It must run in a task of
// its own: never inside a callback from Go.
export function capture(go, fs) {
  const t0 = performance.now();
  const exp = go._inst.exports;
  const mem = new Uint8Array(exp.mem.buffer);
  const globals = [];
  for (let i = 0; `g${i}` in exp; i++) globals.push(exp[`g${i}`].value);
  if (!globals.length) throw new Error("the binary does not export its globals");

  // Only the chunks that are not all zero.
  const chunks = [];
  const words = new BigUint64Array(exp.mem.buffer);
  for (let c = 0; c < mem.length / CHUNK; c++) {
    let i = c * (CHUNK / 8);
    const end = i + CHUNK / 8;
    while (i < end && words[i] === 0n) i++;
    if (i < end) chunks.push(c);
  }
  const data = new Uint8Array(chunks.length * CHUNK);
  chunks.forEach((c, i) => data.set(mem.subarray(c * CHUNK, (c + 1) * CHUNK), i * CHUNK));

  // The JavaScript values, ids 7 and up (0 to 6 are fixed, see wasm_exec.js).
  // A value on globalThis comes back by its name, like the NAMES of
  // k8s/snapshot.mjs; the new worker puts the same ones there.
  const named = new Map();
  for (const k of Object.getOwnPropertyNames(globalThis)) {
    let v;
    try { v = globalThis[k]; } catch { continue; }
    if (v && (typeof v === "object" || typeof v === "function") && !named.has(v)) named.set(v, [k]);
  }
  const dead = {};
  const describe = (v, depth = 0) => {
    if (v === undefined) return { u: 1 };
    if (v === null || typeof v !== "object" && typeof v !== "function") return { v };
    if (v === globalThis) return { g: 1 };
    if (v === go) return { go: 1 };
    const id = go._funcIDs.get(v);
    if (id !== undefined) return { func: id };
    const name = named.get(v);
    if (name && v !== globalThis.tabernetes) return { name };
    if (depth > 8) throw new Error("hibernate: a value nests too deep");
    // a buffer that was transferred (postMessage) is empty: it stays so
    if (ArrayBuffer.isView(v)) return v.buffer.detached ? { bytes: new Uint8Array(0), type: "Uint8Array" } : { bytes: v.slice(), type: v.constructor.name };
    if (v instanceof ArrayBuffer) return { buffer: v.detached ? new ArrayBuffer(0) : v.slice(0) };
    if (v instanceof Promise) return { promise: 1 };
    if (standIns.has(v)) return { dead: standIns.get(v) };
    if (Array.isArray(v)) return { array: v.map((x) => ref(x, depth + 1)) };
    if (typeof v === "object" && isPlain(v)) {
      const props = {};
      for (const k of Object.keys(v)) props[k] = ref(v[k], depth + 1);
      return { object: props, global: v === globalThis.tabernetes ? "tabernetes" : undefined };
    }
    const kind = Object.prototype.toString.call(v).slice(8, -1);
    dead[kind] = (dead[kind] || 0) + 1;
    // The handlers that Go set on a MessagePort or RTCDataChannel: resume
    // calls them as if the other side closed it, so that the goroutines
    // that wait for it end.
    const ends = {};
    for (const h of ["onmessage", "onclose"]) {
      let f;
      try { f = v[h]; } catch { continue; }
      // only a handler that Go still holds: a released one is gone in Go
      const id = typeof f === "function" && go._ids.has(f) ? go._funcIDs.get(f) : undefined;
      if (id !== undefined) ends[h] = id;
    }
    return { dead: kind, ends };
  };
  // ref points to a value Go holds by its id, so that one value stays one.
  const ref = (v, depth) => {
    const id = go._ids.get(v);
    if (id !== undefined && id >= 7 && (v && (typeof v === "object" || typeof v === "function"))) return { id };
    return describe(v, depth);
  };
  const values = [];
  for (let id = 7; id < go._values.length; id++) {
    if (go._idPool.includes(id)) { values.push(null); continue; }
    values.push(describe(go._values[id]));
  }
  const image = {
    pages: mem.length / PAGE,
    chunks,
    data,
    globals,
    values,
    refs: go._goRefCounts.slice(7),
    idPool: [...go._idPool],
    nextTimeout: go._nextCallbackTimeoutID,
    env: go.env,
    fs: fs.dump(),
    at: Date.now(),
  };
  image.ms = performance.now() - t0;
  image.dead = dead;
  return image;
}

// resume runs a fresh instance of the same binary from the image, like
// runGo in wasm.js runs it from the snapshot of the build.
export function resume(go, instance, image, fs) {
  const exp = instance.exports;
  if (JSON.stringify(go.env) !== JSON.stringify(image.env)) throw new Error("the environment differs from the hibernated one");
  const grow = image.pages - exp.mem.buffer.byteLength / PAGE;
  if (grow < 0) throw new Error("the hibernated memory is smaller than the binary's");
  if (grow) exp.mem.grow(grow);
  const mem = new Uint8Array(exp.mem.buffer);
  // The data of the binary is there now. The chunks of the image overwrite
  // it, the rest of the binary's data is zero in the image.
  let next = 0;
  image.chunks.forEach((c, i) => {
    if (c > next) mem.fill(0, next * CHUNK, Math.min(c * CHUNK, (image.pages - grow) * PAGE));
    mem.set(image.data.subarray(i * CHUNK, (i + 1) * CHUNK), c * CHUNK);
    next = c + 1;
  });
  if (next * CHUNK < (image.pages - grow) * PAGE) mem.fill(0, next * CHUNK, (image.pages - grow) * PAGE);
  image.globals.forEach((v, i) => { exp[`g${i}`].value = v; });
  fs.load(image.fs);

  go._inst = instance;
  go.mem = new DataView(exp.mem.buffer);
  go._values = [NaN, 0, null, true, false, globalThis, go];
  go._goRefCounts = new Array(go._values.length).fill(Infinity);
  go._ids = new Map([[0, 1], [null, 2], [true, 3], [false, 4], [globalThis, 5], [go, 6]]);
  // two passes: objects may point to values of a higher id
  const made = [];
  const build = (d) => {
    if (d.u) return undefined;
    if ("v" in d) return d.v;
    if (d.g) return globalThis;
    if (d.go) return go;
    if (d.id !== undefined) return made[d.id] ??= placeholder(image.values[d.id - 7]);
    if (d.func !== undefined) return go._makeFuncWrapper(d.func);
    if (d.name) {
      const v = d.name.reduce((o, k) => o?.[k], globalThis);
      if (v === undefined) throw new Error(`the hibernated control plane needs globalThis.${d.name.join(".")}`);
      return v;
    }
    if (d.bytes) return new globalThis[d.type](d.bytes);
    if (d.buffer) return d.buffer;
    if (d.promise) return Promise.resolve();
    if (d.array) return d.array.map(build);
    if (d.object) return fill({}, d);
    if (d.ends) for (const [h, id] of Object.entries(d.ends)) closers.push([h, go._makeFuncWrapper(id)]);
    return deadStandIn(d.dead);
  };
  const closers = [];
  const fill = (o, d) => {
    for (const [k, p] of Object.entries(d.object)) o[k] = build(p);
    if (d.global) globalThis[d.global] = o;
    return o;
  };
  // An object or array that another value points to exists before it is filled.
  const placeholder = (d) => (d?.object ? {} : d?.array ? [] : undefined);
  image.values.forEach((d, i) => {
    const id = 7 + i;
    if (!d) { go._values.push(undefined); go._goRefCounts.push(0); return; }
    let v = made[id];
    if (v !== undefined && d.object) fill(v, d);
    else if (v !== undefined && d.array) v.push(...d.array.map(build));
    else v = made[id] = build(d);
    go._values.push(v);
    go._goRefCounts.push(image.refs[i]);
    go._ids.set(v, id);
  });
  go._idPool = [...image.idPool];
  // Go still knows the timer it waited for; the next one must get a new id.
  go._nextCallbackTimeoutID = image.nextTimeout;
  go.exited = false;
  go._resume();
  // The connections of the tabs that were open when the image was taken
  // are gone: close them, in a task of their own.
  setTimeout(() => {
    for (const [h, f] of closers) {
      if (go.exited) break;
      f(h === "onmessage" ? { data: null } : {});
    }
  }, 0);
  return go._exitPromise;
}

// deadStandIn stands for an object of a tab that is gone: any method call
// does nothing. standIns knows them, so that the next capture sees them as
// dead again and not as plain objects.
const standIns = new WeakMap();
function deadStandIn(kind) {
  const v = kind === "Function" ? () => undefined : new Proxy({}, {
    get: (t, k) => (k === "then" || typeof k === "symbol" ? undefined : () => undefined),
    set: () => true,
  });
  standIns.set(v, kind);
  return v;
}

// pack gzips the memory of an image into a Blob, unpack reverses it. The
// memory is ~350 MiB, mostly old heap that the GC freed but did not zero;
// gzip makes it ~37 MiB. CompressionStream runs outside this thread.
export async function pack(data) {
  return new Response(new Blob([data]).stream().pipeThrough(new CompressionStream("gzip"))).blob();
}

export async function unpack(blob) {
  return new Uint8Array(await new Response(blob.stream().pipeThrough(new DecompressionStream("gzip"))).arrayBuffer());
}
