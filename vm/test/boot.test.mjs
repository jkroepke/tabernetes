// Boot test of the pod VM: v86 boots the kernel and init of dist/vm (mise
// run vm) in Node.js without a BIOS, the way web/src/vmworker.js does in
// the browser (web/src/kernelboot.js). It checks the parts of the kernel that a pod needs: the
// initramfs, ttyS1 for init, virtio-net, the 9p root file system, devtmpfs,
// devpts and an ELF program as the container. It also saves the booted VM
// and starts the pod from the restored state, like the page does.
//
//   node --test vm/test
//
// TABERNETES_VM sets another directory with vmlinux and init, TABERNETES_JIT
// another jit.bin and TABERNETES_STATE another vmstate.bin (web/jit.mjs;
// default dist/jit.bin and dist/vmstate.bin).
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { V86 } from "../../web/node_modules/v86/build/libv86.mjs";
import { initramfs } from "../../web/src/initramfs.js";
import { bootKernel } from "../../web/src/kernelboot.js";
import { Tree, LayerStorage, modes } from "../../web/src/rootfs.js";
import { buildID, parseState } from "../../web/src/vmstate.js";
import { parseJIT, importJIT } from "../../web/src/jit.js";

const root = new URL("../..", import.meta.url).pathname;
const dir = process.env.TABERNETES_VM || path.join(root, "dist/vm");
const files = ["vmlinux", "init"];
const missing = files.filter((f) => !fs.existsSync(path.join(dir, f)));
// CI builds the VM before the tests: there a missing file is an error.
const skip = missing.length && !process.env.CI ? `no ${missing.join(", ")} in ${dir} (mise run vm)` : false;
const read = (f) => new Uint8Array(fs.readFileSync(path.join(dir, f)));
const jitFile = process.env.TABERNETES_JIT || path.join(root, "dist/jit.bin");
const stateFile = process.env.TABERNETES_STATE || path.join(root, "dist/vmstate.bin");

// The VM of a pod with the size that most pods get.
const MEMORY = 256 << 20;
const IP = "192.168.86.100";
const GATEWAY = "192.168.86.1";

function options(extra) {
  return {
    wasm_path: path.join(root, "web/node_modules/v86/build/v86.wasm"),
    filesystem: {},
    memory_size: MEMORY,
    uart1: true,
    autostart: false,
    disable_keyboard: true,
    disable_mouse: true,
    disable_speaker: true,
    // like web/src/vmworker.js: a small VGA memory keeps the state small
    vga_memory_size: 256 << 10,
    acpi: true,
    preserve_mac_from_state_image: true,
    net_device: { type: "virtio", relay_url: "fetch", vm_ip: IP, router_ip: GATEWAY },
    ...extra,
  };
}

// lines collects the lines that the VM writes to a serial port.
function lines(e, port) {
  const got = [];
  let line = "";
  e.add_listener(`serial${port}-output-byte`, (b) => {
    if (b === 13) return;
    if (b !== 10) { line += String.fromCharCode(b); return; }
    got.push(line);
    line = "";
  });
  return got;
}

async function until(what, pred, ms = 60_000) {
  const t = Date.now();
  while (!pred()) {
    if (Date.now() - t > ms) throw new Error(`timeout: ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

// hello is the smallest static i386 Linux program: it writes a line with
// write(2) and ends with exit code 3. The images of pods are bigger, but
// they need the same from the kernel.
function hello(text) {
  const base = 0x08048000, hdr = 52 + 32;
  const msg = new TextEncoder().encode(text);
  const code = [];
  const le = (n) => [n & 255, (n >> 8) & 255, (n >> 16) & 255, (n >>> 24) & 255];
  const codeLen = 5 * 6 + 2 * 2;
  code.push(0xb8, ...le(4)); // mov eax, 4 (write)
  code.push(0xbb, ...le(1)); // mov ebx, 1
  code.push(0xb9, ...le(base + hdr + codeLen)); // mov ecx, msg
  code.push(0xba, ...le(msg.length)); // mov edx, len
  code.push(0xcd, 0x80); // int 0x80
  code.push(0xb8, ...le(1)); // mov eax, 1 (exit)
  code.push(0xbb, ...le(3)); // mov ebx, 3
  code.push(0xcd, 0x80);
  assert.equal(code.length, codeLen);
  const size = hdr + code.length + msg.length;
  const b = new Uint8Array(size);
  const v = new DataView(b.buffer);
  b.set([0x7f, 0x45, 0x4c, 0x46, 1, 1, 1]); // ELF, 32 bit, little endian
  v.setUint16(16, 2, true); // ET_EXEC
  v.setUint16(18, 3, true); // EM_386
  v.setUint32(20, 1, true);
  v.setUint32(24, base + hdr, true); // entry
  v.setUint32(28, 52, true); // program headers
  v.setUint16(40, 52, true);
  v.setUint16(42, 32, true);
  v.setUint16(44, 1, true);
  v.setUint32(52, 1, true); // PT_LOAD
  v.setUint32(56, 0, true);
  v.setUint32(60, base, true);
  v.setUint32(64, base, true);
  v.setUint32(68, size, true);
  v.setUint32(72, size, true);
  v.setUint32(76, 5, true); // R+X
  v.setUint32(80, 0x1000, true);
  b.set(code, hdr);
  b.set(msg, hdr + code.length);
  return b;
}

// rootfs is the 9p file system of the pod, built like vmworker.js does: the
// program as the image, the config of init on top.
function rootfs(text) {
  const now = Math.floor(Date.now() / 1000);
  const conf = {
    hostname: "boot-test", ip: IP, gateway: GATEWAY, start: ["0"], tmpfs: [],
    containers: [{ name: "hello", root: "/", args: ["/hello"], env: ["PATH=/bin"], cwd: "/", user: "", console: true, stdin: false, stdinOnce: false, tty: false, mounts: [] }],
  };
  const mem = new Map([
    ["mem:0", hello(text)],
    ["mem:1", new TextEncoder().encode(JSON.stringify(conf))],
  ]);
  const tree = new Tree();
  tree.set("hello", { uid: 0, gid: 0, mtime: now, mode: modes.S_IFREG | 0o755, size: mem.get("mem:0").length, key: "mem:0" });
  tree.set(".tabernetes/config.json", { uid: 0, gid: 0, mtime: now, mode: modes.S_IFREG | 0o644, size: mem.get("mem:1").length, key: "mem:1" });
  for (const d of ["dev", "proc", "sys", "tmp", "etc"]) tree.set(d, { uid: 0, gid: 0, mtime: now, mode: modes.S_IFDIR | 0o755 });
  return { json: tree.v86(), storage: new LayerStorage(new Map(), mem) };
}

// nestedRootfs is a pod with two containers, each in its own root under
// .tabernetes/c, and a volume that both mount, like vmworker.js builds it.
function nestedRootfs(texts) {
  const now = Math.floor(Date.now() / 1000);
  const tree = new Tree();
  const mem = new Map();
  const file = (path, data, mode) => {
    const key = `mem:${mem.size}`;
    mem.set(key, data);
    tree.set(path, { uid: 0, gid: 0, mtime: now, mode: modes.S_IFREG | mode, size: data.length, key });
  };
  const dir = (path, mode = 0o755) => tree.set(path, { uid: 0, gid: 0, mtime: now, mode: modes.S_IFDIR | mode });
  const containers = texts.map((text, i) => {
    const root = `.tabernetes/c/${i}`;
    for (const d of ["dev", "proc", "sys", "tmp", "etc", "data"]) dir(`${root}/${d}`);
    file(`${root}/hello`, hello(text), 0o755);
    return {
      name: `c${i}`, root: "/" + root, args: ["/hello"], env: ["PATH=/bin"], cwd: "/", user: "", console: i === 0,
      stdin: false, stdinOnce: false, tty: false, mounts: [{ src: "/.tabernetes/v/data", dst: "/data", ro: i === 1 }],
    };
  });
  dir(".tabernetes/v/data", 0o777);
  file(".tabernetes/v/data/greeting", new TextEncoder().encode("hi\n"), 0o644);
  for (const d of ["dev", "proc", "sys", "tmp"]) dir(d);
  const conf = { hostname: "boot-test", ip: IP, gateway: GATEWAY, start: ["0"], tmpfs: [{ path: "/.tabernetes/v/mem", options: "size=1048576" }], containers };
  dir(".tabernetes/v/mem", 0o777);
  file(".tabernetes/config.json", new TextEncoder().encode(JSON.stringify(conf)), 0o644);
  return { json: tree.v86(), storage: new LayerStorage(new Map(), mem) };
}

const done = (control, i) => control.some((l) => l.startsWith(`exit ${i} `));

// startPod gives the waiting init its root file system and the "go" line,
// and waits for the program to end.
async function startPod(e, control, t) {
  const console = lines(e, 0);
  const text = "hello from the pod";
  const fsys = rootfs(text + "\n");
  e.fs9p.storage = fsys.storage;
  e.fs9p.load_from_json(fsys.json);
  const t0 = Date.now();
  e.run();
  e.serial_send_bytes(1, new TextEncoder().encode(`go ${Date.now()} ${"ab".repeat(32)}\n`));
  await until("started", () => control.includes("started 0") || done(control, 0) || control.some((l) => l.startsWith("fail ")));
  t.diagnostic(`started after ${Date.now() - t0} ms`);
  await until("exit", () => done(control, 0) || control.some((l) => l.startsWith("fail ")));
  assert.deepEqual(control.filter((l) => l !== "ready"), ["started 0", "exit 0 3"], `init: ${control.join(" | ")}`);
  await until("console", () => console.includes(text), 5000).catch(() => {});
  assert.ok(console.includes(text), `console: ${console.join(" | ")}`);
}

// startNested starts a pod of two containers: the first at once, the second
// on command after the first ended, as the kubelet does with an init
// container. The second one logs on the control port.
async function startNested(e, control, t) {
  const console = lines(e, 0);
  const fsys = nestedRootfs(["first\n", "second\n"]);
  e.fs9p.storage = fsys.storage;
  e.fs9p.load_from_json(fsys.json);
  e.run();
  const send = (l) => e.serial_send_bytes(1, new TextEncoder().encode(l + "\n"));
  send(`go ${Date.now()} ${"ab".repeat(32)}`);
  const failed = () => control.some((l) => l.startsWith("fail "));
  await until("exit 0", () => done(control, 0) || failed());
  assert.deepEqual(control.filter((l) => l !== "ready"), ["started 0", "exit 0 3"], `init: ${control.join(" | ")}`);
  const t0 = Date.now();
  send("start 1");
  await until("exit 1", () => done(control, 1) || failed());
  t.diagnostic(`second container ran in ${Date.now() - t0} ms`);
  const got = control.filter((l) => l !== "ready").slice(2);
  assert.ok(got.includes("started 1") && got.includes("exit 1 3"), `init: ${control.join(" | ")}`);
  const logs = got.filter((l) => l.startsWith("log 1 ")).map((l) => Buffer.from(l.slice(6), "base64").toString()).join("");
  assert.equal(logs, "second\n");
  await until("console", () => console.includes("first"), 5000).catch(() => {});
  assert.ok(console.includes("first") && !console.includes("second"), `console: ${console.join(" | ")}`);
}

test("the kernel boots, init starts a pod, the saved VM restores", { skip, timeout: 300_000 }, async (t) => {
  const kernel = read("vmlinux");
  const init = read("init");

  // boot: init waits for the pod in the initramfs and says "ready"
  const e = new V86(options({}));
  const boot = lines(e, 0);
  const control = lines(e, 1);
  await new Promise((r) => e.add_listener("emulator-loaded", r));
  const t0 = Date.now();
  bootKernel(e.v86.cpu, { kernel, initrd: initramfs(init), cmdline: "console=ttyS0 quiet loglevel=3 tsc=reliable highres=off" });
  e.run();
  try {
    await until("ready", () => control.includes("ready") || control.some((l) => l.startsWith("exit ")));
  } catch (err) {
    err.message += `\nconsole:\n${boot.join("\n")}`;
    throw err;
  }
  assert.deepEqual(control, ["ready"], `init: ${control.join(" | ")}\nconsole:\n${boot.join("\n")}`);
  t.diagnostic(`ready after ${Date.now() - t0} ms`);

  await e.stop();
  const state = await e.save_state();
  t.diagnostic(`saved state: ${(state.byteLength / 2 ** 20).toFixed(1)} MiB`);

  await t.test("a pod starts on the booted VM", async (t) => {
    await startPod(e, control, t);
  });
  await e.destroy();

  await t.test("a pod of two containers with a volume starts on the restored VM", async (t) => {
    const r = new V86(options({}));
    await new Promise((res) => r.add_listener("emulator-loaded", res));
    await r.restore_state(state);
    const control = lines(r, 1);
    try {
      await startNested(r, control, t);
    } finally {
      await r.destroy();
    }
  });

  await t.test("a pod starts on the restored VM", async (t) => {
    const r = new V86(options({}));
    await new Promise((res) => r.add_listener("emulator-loaded", res));
    await r.restore_state(state);
    const control = lines(r, 1);
    try {
      await startPod(r, control, t);
    } finally {
      await r.destroy();
    }
  });

  // "warm" (vmworker.js sends it to a VM that it throws away): init runs
  // its warm-up and waits for the pod again
  await t.test("init warms up on the restored VM, then starts a pod", async (t) => {
    const r = new V86(options({}));
    await new Promise((res) => r.add_listener("emulator-loaded", res));
    await r.restore_state(state);
    const control = lines(r, 1);
    try {
      const i0 = r.v86.cpu.instruction_counter[0];
      const t0 = Date.now();
      r.run();
      r.serial_send_bytes(1, new TextEncoder().encode("warm\n"));
      await until("warmed", () => control.includes("warmed") || control.some((l) => l.startsWith("exit ")));
      t.diagnostic(`warmed after ${Date.now() - t0} ms, ${(((r.v86.cpu.instruction_counter[0] - i0) >>> 0) / 1e6).toFixed(1)} million instructions`);
      assert.deepEqual(control, ["warmed"], `init: ${control.join(" | ")}`);
      control.length = 0;
      await startPod(r, control, t);
    } finally {
      await r.destroy();
    }
  });

  // like vmworker.js: the JIT modules of the kernel after the restore
  const jitSkip = !fs.existsSync(jitFile) && !process.env.CI ? `no ${jitFile} (mise run web)` : false;
  await t.test("a pod starts on the restored VM with the JIT modules of the kernel", { skip: jitSkip }, async (t) => {
    const buf = fs.readFileSync(jitFile);
    const jit = await parseJIT(buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength));
    assert.ok(jit, "jit.bin does not parse");
    const { version } = JSON.parse(fs.readFileSync(path.join(root, "web/node_modules/v86/package.json"), "utf8"));
    const build = await buildID(version, kernel, init);
    const r = new V86(options({}));
    await new Promise((res) => r.add_listener("emulator-loaded", res));
    await r.restore_state(state);
    // another build gets nothing
    assert.equal(importJIT(r, jit, "0000000000000000"), 0);
    const n = importJIT(r, jit, build);
    t.diagnostic(`installed ${n} of ${jit.modules.length} modules`);
    assert.ok(n > jit.modules.length / 2, `installed ${n} of ${jit.modules.length} modules (build ${jit.build}, this one ${build})`);
    const control = lines(r, 1);
    try {
      await startPod(r, control, t);
    } finally {
      await r.destroy();
    }
  });

  // like vmworker.js: the first pod of a visitor restores the state that the
  // build ships, then installs the JIT modules of the same build
  const stateSkip = !fs.existsSync(stateFile) && !process.env.CI ? `no ${stateFile} (mise run web)` : false;
  await t.test("a pod starts on the state of the build", { skip: stateSkip }, async (t) => {
    const buf = fs.readFileSync(stateFile);
    const shipped = parseState(buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength));
    assert.ok(shipped, "vmstate.bin does not parse");
    const { version } = JSON.parse(fs.readFileSync(path.join(root, "web/node_modules/v86/package.json"), "utf8"));
    assert.equal(shipped.build, await buildID(version, kernel, init), "vmstate.bin is of another build");
    assert.equal(shipped.memory, MEMORY);
    const r = new V86(options({}));
    await new Promise((res) => r.add_listener("emulator-loaded", res));
    const t0 = Date.now();
    await r.restore_state(shipped.state);
    t.diagnostic(`restored in ${Date.now() - t0} ms`);
    if (fs.existsSync(jitFile)) {
      const j = fs.readFileSync(jitFile);
      const jit = await parseJIT(j.buffer.slice(j.byteOffset, j.byteOffset + j.byteLength));
      t.diagnostic(`installed ${importJIT(r, jit, shipped.build)} JIT modules`);
    }
    const control = lines(r, 1);
    try {
      await startPod(r, control, t);
    } finally {
      await r.destroy();
    }
  });
});
