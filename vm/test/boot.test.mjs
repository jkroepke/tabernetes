// Boot test of the pod VM: v86 boots the kernel and init of dist/vm (mise
// run vm) in Node.js without a BIOS, the way web/src/vmworker.js does in
// the browser (web/src/kernelboot.js). It checks the parts of the kernel that a pod needs: the
// initramfs, ttyS1 for init, virtio-net, the 9p root file system, devtmpfs,
// devpts and an ELF program as the container. It also saves the booted VM
// and starts the pod from the restored state, like the page does.
//
//   node --test vm/test
//
// TABERNETES_VM sets another directory with vmlinux and init.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { V86 } from "../../web/node_modules/v86/build/libv86.mjs";
import { initramfs } from "../../web/src/initramfs.js";
import { bootKernel } from "../../web/src/kernelboot.js";
import { Tree, LayerStorage, modes } from "../../web/src/rootfs.js";

const root = new URL("../..", import.meta.url).pathname;
const dir = process.env.TABERNETES_VM || path.join(root, "dist/vm");
const files = ["vmlinux", "init"];
const missing = files.filter((f) => !fs.existsSync(path.join(dir, f)));
// CI builds the VM before the tests: there a missing file is an error.
const skip = missing.length && !process.env.CI ? `no ${missing.join(", ")} in ${dir} (mise run vm)` : false;
const read = (f) => new Uint8Array(fs.readFileSync(path.join(dir, f)));

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
  const conf = { args: ["/hello"], env: ["PATH=/bin"], cwd: "/", user: "", hostname: "boot-test", ip: IP, gateway: GATEWAY, stdin: false, stdinOnce: false, tty: false };
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
  await until("started", () => control.includes("started") || control.some((l) => l.startsWith("exit ")));
  t.diagnostic(`started after ${Date.now() - t0} ms`);
  await until("exit", () => control.some((l) => l.startsWith("exit ")));
  assert.deepEqual(control.filter((l) => l !== "ready"), ["started", "exit 3"], `init: ${control.join(" | ")}`);
  await until("console", () => console.includes(text), 5000).catch(() => {});
  assert.ok(console.includes(text), `console: ${console.join(" | ")}`);
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
  bootKernel(e.v86.cpu, { kernel, initrd: initramfs(init), cmdline: "console=ttyS0 quiet loglevel=3" });
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
});
