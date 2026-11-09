// Shared by the page (terminal.js) and the shared worker (cluster.js): the
// binary is a row of zstd frames, and it compiles while it arrives.

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

// instantiate compiles while the bytes still arrive.
export async function instantiate(stream, imports) {
  if (WebAssembly.instantiateStreaming) {
    const resp = new Response(stream, { headers: { "Content-Type": "application/wasm" } });
    return (await WebAssembly.instantiateStreaming(resp, imports)).instance;
  }
  const bytes = await new Response(stream).arrayBuffer();
  return (await WebAssembly.instantiate(bytes, imports)).instance;
}
