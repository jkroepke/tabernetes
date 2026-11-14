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
  // The data segments (the heap of the snapshot, ~140 MB) do not go through
  // the compiler: they are copied into the memory of the instance.
  const { code, segments } = splitData(stream);
  let result;
  if (WebAssembly.instantiateStreaming) {
    const resp = new Response(code, { headers: { "Content-Type": "application/wasm" } });
    result = await WebAssembly.instantiateStreaming(resp, go.importObject);
  } else {
    const bytes = await new Response(code).arrayBuffer();
    result = await WebAssembly.instantiate(bytes, go.importObject);
  }
  const mem = new Uint8Array(result.instance.exports.mem.buffer);
  for (const { offset, parts } of await segments) {
    let at = offset;
    for (const p of parts) {
      mem.set(p, at);
      at += p.length;
    }
  }
  const [sec] = WebAssembly.Module.customSections(result.module, "tabernetes.snapshot");
  if (sec) go.snapshot = JSON.parse(new TextDecoder().decode(sec));
  return result.instance;
}

// splitData takes the active data segments out of a wasm binary while it
// streams. code is the binary with an empty data section (and a data count
// of 0); segments resolves to [{offset, parts}] once the stream has ended.
// Streaming compilation of the data section cost the thread that feeds it
// ~0.3 s for the ~140 MB of the heap snapshot; copying them into the memory
// takes ~40 ms. The parts are views of the chunks of the stream, no copies.
export function splitData(stream) {
  const reader = stream.getReader();
  let chunk = new Uint8Array(0), pos = 0;
  const more = async () => {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return false;
      if (value.length) {
        chunk = value instanceof Uint8Array ? value : new Uint8Array(value);
        pos = 0;
        return true;
      }
    }
  };
  const byte = async () => {
    if (pos === chunk.length && !(await more())) throw new Error("wasm: the binary is truncated");
    return chunk[pos++];
  };
  const uleb = async () => {
    let v = 0, shift = 0, b;
    do {
      b = await byte();
      v += (b & 127) * 2 ** shift;
      shift += 7;
    } while (b & 128);
    return v;
  };
  // take hands n bytes to fn, as views of the chunks.
  const take = async (n, fn) => {
    while (n > 0) {
      if (pos === chunk.length && !(await more())) throw new Error("wasm: the binary is truncated");
      const k = Math.min(n, chunk.length - pos);
      fn(chunk.subarray(pos, pos + k));
      pos += k;
      n -= k;
    }
  };
  let done;
  const segments = new Promise((resolve, reject) => { done = { resolve, reject }; });
  segments.catch(() => {});
  const code = new ReadableStream({
    async start(ctl) {
      try {
        const list = [];
        await take(8, (b) => ctl.enqueue(b)); // magic and version
        while (pos < chunk.length || (await more())) {
          const id = await byte();
          const size = await uleb();
          if (id === 11 || id === 12) {
            // data, data count: no segment for the compiler
            ctl.enqueue(Uint8Array.of(id, 1, 0));
            if (id === 12) {
              await take(size, () => {});
              continue;
            }
            for (let n = await uleb(); n > 0; n--) {
              const flags = await uleb();
              // Go writes active segments of memory 0 with an i32.const offset.
              if (flags !== 0 || (await byte()) !== 0x41) throw new Error("wasm: unexpected data segment");
              let offset = 0, shift = 0, b;
              do {
                b = await byte();
                offset += (b & 127) * 2 ** shift;
                shift += 7;
              } while (b & 128);
              if (b & 0x40) offset -= 2 ** shift; // sign
              if ((await byte()) !== 0x0b) throw new Error("wasm: unexpected data segment");
              const parts = [];
              await take(await uleb(), (b) => parts.push(b));
              list.push({ offset: offset >>> 0, parts });
            }
            continue;
          }
          let head = [id], s = size;
          do { head.push((s & 127) | (s > 127 ? 128 : 0)); s = Math.floor(s / 128); } while (s > 0);
          ctl.enqueue(Uint8Array.from(head));
          await take(size, (b) => ctl.enqueue(b));
        }
        ctl.close();
        done.resolve(list);
      } catch (e) {
        ctl.error(e);
        done.reject(e);
      }
    },
  });
  return { code, segments };
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
