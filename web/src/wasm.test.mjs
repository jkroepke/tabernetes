import { test } from "node:test";
import assert from "node:assert/strict";
import { instantiate, splitData } from "./wasm.js";

// A module like the ones Go writes: one exported memory "mem", a function
// that reads a byte of it, a data count section and active data segments.
const leb = (n) => { const o = []; do { let b = n & 127; n >>>= 7; if (n) b |= 128; o.push(b); } while (n); return o; };
const sleb = (n) => { const o = []; for (;;) { const b = n & 127; n >>= 7; if ((n === 0 && !(b & 64)) || (n === -1 && b & 64)) { o.push(b); return o; } o.push(b | 128); } };
const sec = (id, body) => [id, ...leb(body.length), ...body];
const vec = (items) => [...leb(items.length), ...items.flat()];
const name = (s) => [...leb(s.length), ...new TextEncoder().encode(s)];
const segments = [
  { offset: 16, bytes: [1, 2, 3, 4, 5] },
  { offset: 100000, bytes: Array.from({ length: 300 }, (_, i) => i & 255) },
];
const module = Uint8Array.from([
  0, 0x61, 0x73, 0x6d, 1, 0, 0, 0,
  ...sec(1, vec([[0x60, 1, 0x7f, 1, 0x7f]])), // (i32) -> i32
  ...sec(3, vec([[0]])),
  ...sec(5, vec([[0, 2]])), // memory of 2 pages
  ...sec(7, vec([[...name("mem"), 2, 0], [...name("peek"), 0, 0]])),
  ...sec(12, leb(segments.length)),
  ...sec(10, vec([(() => { const code = [0, 0x20, 0, 0x2d, 0, 0, 0x0b]; return [...leb(code.length), ...code]; })()])),
  ...sec(11, vec(segments.map(({ offset, bytes }) => [0, 0x41, ...sleb(offset), 0x0b, ...leb(bytes.length), ...bytes]))),
  ...sec(0, name("custom")),
]);

// stream hands the bytes over in chunks of the given size
const stream = (bytes, size) => new ReadableStream({
  start(ctl) {
    for (let i = 0; i < bytes.length; i += size) ctl.enqueue(bytes.slice(i, i + size));
    ctl.close();
  },
});

test("splitData: the segments come out, the rest compiles", async () => {
  for (const size of [1, 3, 7, 64, module.length]) {
    const { code, segments: got } = splitData(stream(module, size));
    const bytes = new Uint8Array(await new Response(code).arrayBuffer());
    assert.ok(WebAssembly.validate(bytes), `chunks of ${size}`);
    const list = await got;
    assert.deepEqual(list.map((s) => s.offset), segments.map((s) => s.offset));
    for (let i = 0; i < list.length; i++) {
      assert.deepEqual([...list[i].parts.flatMap((p) => [...p])], segments[i].bytes);
    }
  }
});

test("instantiate: the memory holds the data segments", async () => {
  for (const size of [5, module.length]) {
    const go = { importObject: {} };
    const instance = await instantiate(stream(module, size), go);
    const mem = new Uint8Array(instance.exports.mem.buffer);
    for (const { offset, bytes } of segments) assert.deepEqual([...mem.subarray(offset, offset + bytes.length)], bytes);
    assert.equal(instance.exports.peek(17), 2);
    assert.equal(go.snapshot, undefined);
  }
});

test("splitData: a truncated binary fails", async () => {
  const { code, segments: got } = splitData(stream(module.subarray(0, module.length - 20), 16));
  await assert.rejects(new Response(code).arrayBuffer());
  await assert.rejects(got);
});
