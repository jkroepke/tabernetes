import { test } from "node:test";
import assert from "node:assert/strict";
import { packJIT, parseJIT, importJIT } from "./jit.js";

// an export of v86 (jit_export) without modules
const empty = () => new Uint32Array([0x4a363876, 1, 1234, 0]).buffer;

test("jit.bin: the build in front of the export of v86", async () => {
  const jit = await parseJIT(packJIT("277cf9991db21026", empty()).buffer);
  assert.deepEqual(jit, { build: "277cf9991db21026", layout: 1234, modules: [] });
});

test("jit.bin: another file is none", async () => {
  assert.equal(await parseJIT(new TextEncoder().encode("<!doctype html><html>not found</html>").buffer), null);
  assert.equal(await parseJIT(new ArrayBuffer(0)), null);
  const other = new Uint32Array(empty());
  other[1] = 2; // a later format of v86
  assert.equal(await parseJIT(packJIT("x", other.buffer).buffer), null);
});

test("jit.bin: only for its build", () => {
  let got = null;
  const e = { jit_import: (j) => { got = j; return 7; } };
  const jit = { build: "a", modules: [] };
  assert.equal(importJIT(e, jit, "b"), 0);
  assert.equal(got, null);
  assert.equal(importJIT(e, jit, "a"), 7);
  assert.equal(importJIT({}, jit, "a"), 0); // a v86 without jit_import
  assert.equal(importJIT(e, null, "a"), 0);
});
