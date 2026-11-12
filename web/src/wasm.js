// The page (terminal.js) unpacks the binary, a row of zstd frames, and
// compiles it while it arrives. The shared worker (cluster.js) gets the
// unpacked bytes from the page and compiles them the same way.

// zstdFrames yields each zstd frame of the stream once all of it is there.
export async function* zstdFrames(stream, size) {
  let buf = new Uint8Array(size || 32 << 20), len = 0, start = 0;
  const r = stream.getReader();
  for (;;) {
    const { value, done } = await r.read();
    if (value) {
      if (len + value.length > buf.length) {
        const b = new Uint8Array(Math.max(2 * buf.length, len + value.length));
        b.set(buf.subarray(0, len));
        buf = b;
      }
      buf.set(value, len);
      len += value.length;
    }
    for (let end; (end = frameEnd(buf.subarray(start, len))); start += end) yield buf.slice(start, start + end);
    if (done) {
      if (start < len) throw new Error("zstd: the download is truncated");
      return;
    }
  }
}

// frameEnd returns the length of the zstd frame at the start of b, or 0 when
// b does not hold all of it yet. See RFC 8878, section 3.1.1.
function frameEnd(b) {
  if (b.length < 6) return 0;
  if (((b[0] | b[1] << 8 | b[2] << 16 | b[3] << 24) >>> 0) !== 0xfd2fb528) throw new Error("zstd: bad frame magic");
  const fhd = b[4], single = (fhd >> 5) & 1;
  let p = 5 + (single ? 0 : 1) + [0, 1, 2, 4][fhd & 3] + [single, 2, 4, 8][fhd >> 6];
  for (let last = 0; !last;) {
    if (p + 3 > b.length) return 0;
    const h = b[p] | b[p + 1] << 8 | b[p + 2] << 16;
    last = h & 1;
    p += 3 + (((h >> 1) & 3) === 1 ? 1 : h >>> 3); // an RLE block has one byte
  }
  p += fhd & 4 ? 4 : 0; // checksum
  return p <= b.length ? p : 0;
}

// instantiate compiles while the bytes still arrive. go is the Go of
// wasm_exec.js that will run the instance; a binary with a snapshot of the
// heap after init (k8s/snapshot.mjs) puts its description on go.snapshot.
export async function instantiate(stream, go) {
  let result;
  if (WebAssembly.instantiateStreaming) {
    const resp = new Response(stream, { headers: { "Content-Type": "application/wasm" } });
    result = await WebAssembly.instantiateStreaming(resp, go.importObject);
  } else {
    const bytes = await new Response(stream).arrayBuffer();
    result = await WebAssembly.instantiate(bytes, go.importObject);
  }
  const [sec] = WebAssembly.Module.customSections(result.module, "tabernetes.snapshot");
  if (sec) go.snapshot = JSON.parse(new TextDecoder().decode(sec));
  return result.instance;
}

// runGo runs the Go program like go.run(instance). With a snapshot, the
// memory and the globals of the instance already are those of the program
// paused in main: this sets up the JavaScript side of wasm_exec.js as
// go.run would have left it at that point, and resumes the program.
export async function runGo(go, instance) {
  const s = go.snapshot;
  if (!s) return go.run(instance);
  // Go read these during init and keeps what it read.
  if (JSON.stringify(go.env) !== JSON.stringify(s.env)) throw new Error("the environment differs from the snapshot");
  if (JSON.stringify(globalThis.fs?.constants) !== s.constants) throw new Error("fs.constants differ from the snapshot");
  go._inst = instance;
  go.mem = new DataView(instance.exports.mem.buffer);
  // the values that wasm_exec.js always has, see Go.run
  go._values = [NaN, 0, null, true, false, globalThis, go];
  go._goRefCounts = new Array(go._values.length).fill(Infinity);
  go._ids = new Map([[0, 1], [null, 2], [true, 3], [false, 4], [globalThis, 5], [go, 6]]);
  s.values.forEach((path, i) => {
    const id = go._values.length;
    const v = path && path.reduce((o, k) => o?.[k], globalThis);
    if (path && v === undefined) throw new Error(`the snapshot needs globalThis.${path.join(".")}`);
    go._values.push(v);
    go._goRefCounts.push(s.refs[i]);
    if (path) go._ids.set(v, id);
  });
  go._idPool = [...s.idPool];
  go._nextCallbackTimeoutID = s.nextTimeout; // Go still knows the timer of its sleep
  go.exited = false;
  go._resume();
  return go._exitPromise;
}
