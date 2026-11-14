import { test } from "node:test";
import assert from "node:assert/strict";
import { packState, parseState } from "./vmstate.js";

// the start of a state of v86 (magic, version, total length) and of zstd
const raw = () => { const b = new Uint32Array([0x86768676, 6, 32, 0, 0, 0, 0, 0]); return b.buffer; };
const zstd = () => new Uint32Array([0xfd2fb528, 1, 2, 3]).buffer;

test("vmstate.bin: the build and the memory in front of the state", () => {
  for (const s of [raw(), zstd()]) {
    const got = parseState(packState("2f082f807d4e6f5a", 256 << 20, s).buffer);
    assert.equal(got.build, "2f082f807d4e6f5a");
    assert.equal(got.memory, 256 << 20);
    assert.deepEqual(new Uint8Array(got.state), new Uint8Array(s));
  }
});

test("vmstate.bin: another file is none", () => {
  assert.equal(parseState(new TextEncoder().encode("<!doctype html><html>not found</html>".padEnd(200)).buffer), null);
  assert.equal(parseState(new ArrayBuffer(0)), null);
  // a cut state
  const cut = packState("x", 256 << 20, raw()).slice(0, 64 + 20);
  assert.equal(parseState(cut.buffer), null);
});
