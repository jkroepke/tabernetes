import assert from "node:assert/strict";
import test from "node:test";
import { openStore } from "./layers.js";

async function collect(t, sizes) {
  const oldNavigator = Object.getOwnPropertyDescriptor(globalThis, "navigator");
  const oldHandle = Object.getOwnPropertyDescriptor(globalThis, "FileSystemFileHandle");
  t.after(() => {
    if (oldNavigator) Object.defineProperty(globalThis, "navigator", oldNavigator);
    else delete globalThis.navigator;
    if (oldHandle) Object.defineProperty(globalThis, "FileSystemFileHandle", oldHandle);
    else delete globalThis.FileSystemFileHandle;
  });
  const removed = [];
  // One clock reading for all layers: layer i is i ms older than layer 0,
  // also when the runner is slow.
  const now = Date.now();
  const dir = {
    async getDirectoryHandle() { return this; },
    async *entries() {
      for (const [i, size] of sizes.entries()) {
        yield [`sha256-layer${i}.tar`, { kind: "file", getFile: async () => ({ size, lastModified: now - 180000 - i }) }];
      }
    },
    async removeEntry(name) { removed.push(name); },
  };
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: {
    storage: { getDirectory: async () => dir, estimate: async () => ({ quota: 16 * 2 ** 30 }) },
    locks: { request: async (_name, _options, fn) => fn({}) },
  } });
  class FileHandle { createSyncAccessHandle() {} }
  Object.defineProperty(globalThis, "FileSystemFileHandle", { configurable: true, value: FileHandle });
  const store = await openStore();
  assert.equal(store.persistent, true);
  return { freed: await store.gc(), removed };
}

test("image GC keeps unused layers below the 4 GiB budget", async (t) => {
  const result = await collect(t, [2 ** 20]);
  assert.equal(result.freed, 0);
  assert.deepEqual(result.removed, []);
});

test("image GC still frees old layers above the high threshold", async (t) => {
  const result = await collect(t, [2 * 2 ** 30, 2 * 2 ** 30]);
  assert.equal(result.freed, 2 * 2 ** 30);
  assert.ok(result.removed.includes("sha256-layer1.tar"));
});
