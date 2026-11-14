// jit.mjs makes jit.bin, the JIT modules of the kernel of the pods
// (src/jit.js explains why), and vmstate.bin, the state of the booted VM
// (src/vmstate.js). It boots the kernel like vmworker.js, saves the VM when
// init waits for the pod, starts a
// pod with a generic start (a busybox shell: processes, files over 9p,
// pipes, TCP over loopback) and a low JIT threshold, and exports the most
// used modules of the kernel code. They fit only this v86 build, kernel and
// init: the file names their build (vmstate.js), and v86 also checks its own
// build and the bytes of every page before it installs a module.
//
//   node jit.mjs <dir with vmlinux and init> <rootfs.tar of a linux/386 alpine> <jit.bin> <vmstate.bin>
import fs from "node:fs";
import zlib from "node:zlib";
import { V86 } from "v86";
import { initramfs } from "./src/initramfs.js";
import { bootKernel } from "./src/kernelboot.js";
import { Tree, LayerStorage, modes } from "./src/rootfs.js";
import { buildID, packState } from "./src/vmstate.js";
import { packJIT } from "./src/jit.js";

const [vmDir, rootTar, out, stateOut] = process.argv.slice(2);
// as in vmworker.js
const KERNEL_CMDLINE = "console=ttyS0 quiet loglevel=3 tsc=reliable highres=off";
const MEMORY = 256 << 20;
const IP = "10.0.0.2", GATEWAY = "10.0.0.1";
// v86 compiles a page after this many instructions in the interpreter (750k
// by default). 5k, 20k and 100k made the same pod starts within the noise.
const THRESHOLD = 20_000;
// The most entered modules only. V8 optimises them with TurboFan (~1.5 us
// per byte, on other threads) once per tab; the tab does that at the prompt
// (warm in src/jit.js), else it cost the first pod. Without the warm-up,
// measured 2026-11-14 with nginx in Node.js
// on a busy 4-core machine, 4 alternating runs of a fresh process: the first
// pod / a later pod, wall time against no modules: 25 modules +5 % / -4 %,
// 50 +19 % / -18 %, 100 +47 % / -19 %, all (~200) +40 % / -30 %. Process
// CPU of the first pod +0.2, +0.4, +0.8 and +1.4 s; later pods -9 % with 50.
// With the warm-up, and the worker of the first pod made at the prompt
// (vm.js), headless Chromium, nginx, start of the pod -> HTTP, 8 alternating
// rounds, 50 / 100 / all modules: first pod of a fresh visitor 661 / 569 /
// 516 ms, of a return visit 609 / 564 / 485 ms, later pods ~580 / ~480 /
// ~450 ms. But with all modules the image pull of a fresh visitor took 0.35 s
// longer (V8 still optimised modules then), so the whole first pod took
// 2449 / 2439 / 2682 ms. 100 modules cost the first pod nothing. All
// modules with a warm-up of only the first 100 or 50 were worse: V8 then
// optimised the others during the first pod (start -> HTTP 578 -> 741 and
// 848 ms for a fresh visitor).
const MODULES = 100;
const SCRIPT = "httpd -p 127.0.0.1:8080 -h /etc; for i in 1 2 3 4 5 6 7 8; do wget -q -O /tmp/p http://127.0.0.1:8080/passwd; ls -l /usr/bin /lib > /dev/null; cat /etc/* 2>/dev/null | gzip | wc -c > /dev/null; done; find / -xdev > /tmp/b; echo done";

// the entries of a tar file in the form of the index of layers.js
function tarEntries(buf) {
  const entries = [];
  const str = (o, n) => { let s = ""; for (let i = 0; i < n && buf[o + i]; i++) s += String.fromCharCode(buf[o + i]); return s; };
  const oct = (o, n) => parseInt(str(o, n).trim() || "0", 8);
  let off = 0, longName = null;
  while (off + 512 <= buf.length && buf[off] !== 0) {
    let name = str(off, 100);
    const prefix = str(off + 345, 155);
    if (prefix) name = prefix + "/" + name;
    const size = oct(off + 124, 12), type = String.fromCharCode(buf[off + 156] || 48), data = off + 512;
    const next = data + Math.ceil(size / 512) * 512;
    if (type === "L") { longName = str(data, size); off = next; continue; }
    if (type === "x" || type === "g") {
      for (const l of new TextDecoder().decode(buf.subarray(data, data + size)).split("\n")) {
        const m = l.match(/^\d+ path=(.*)$/);
        if (m) longName = m[1];
      }
      off = next;
      continue;
    }
    if (longName) { name = longName; longName = null; }
    entries.push([name, type, oct(off + 100, 8) & 0o7777, oct(off + 108, 8), oct(off + 116, 8), oct(off + 136, 12), size, data, str(off + 157, 100)]);
    off = next;
  }
  return entries;
}

function lines(e, port, cb) {
  let line = "";
  e.add_listener(`serial${port}-output-byte`, (b) => {
    if (b === 13) return;
    if (b !== 10) { line += String.fromCharCode(b); return; }
    cb(line);
    line = "";
  });
}

const e = new V86({
  wasm_path: new URL(import.meta.resolve("v86/build/v86.wasm")).pathname,
  filesystem: {},
  memory_size: MEMORY,
  uart1: true,
  autostart: false,
  disable_keyboard: true,
  disable_mouse: true,
  disable_speaker: true,
  acpi: true,
  vga_memory_size: 256 << 10,
  net_device: { type: "virtio", relay_url: "fetch", vm_ip: IP, router_ip: GATEWAY },
});
await new Promise((r) => e.add_listener("emulator-loaded", r));
let ready, exited, console_ = "";
const control = new Promise((r) => { ready = r; });
const done = new Promise((r) => { exited = r; });
lines(e, 1, (l) => { if (l === "ready") ready(); if (l.startsWith("exit ")) exited(l); });
lines(e, 0, (l) => { console_ += l + "\n"; });
bootKernel(e.v86.cpu, {
  kernel: new Uint8Array(fs.readFileSync(`${vmDir}/vmlinux`)),
  initrd: initramfs(new Uint8Array(fs.readFileSync(`${vmDir}/init`))),
  cmdline: KERNEL_CMDLINE,
});
e.run();
await control;
// init waits for the pod: the state that every pod with this memory size
// restores (vmworker.js saves the same at this point)
await e.stop();
const state = await e.save_state();
e.run();

// the pod: the image and its config, as vmworker.js builds them
const buf = new Uint8Array(fs.readFileSync(rootTar));
const tree = new Tree();
tree.addLayer("L", tarEntries(buf));
tree.layer = 1;
const conf = { args: ["/bin/sh", "-c", SCRIPT], env: ["PATH=/usr/sbin:/usr/bin:/sbin:/bin"], cwd: "/", user: "", hostname: "jit", ip: IP, gateway: GATEWAY, stdin: false, stdinOnce: false, tty: false };
const mem = new Map([["mem:0", new TextEncoder().encode(JSON.stringify(conf))]]);
tree.set(".tabernetes/config.json", { uid: 0, gid: 0, mtime: 0, mode: modes.S_IFREG | 0o644, size: mem.get("mem:0").length, key: "mem:0" });
for (const d of ["dev", "proc", "sys", "tmp"]) if (!tree.files.has(d)) tree.set(d, { uid: 0, gid: 0, mtime: 0, mode: modes.S_IFDIR | 0o1777 });
e.fs9p.storage = new LayerStorage(new Map([["L", { read: async (o, n) => buf.subarray(o, o + n) }]]), mem);
e.fs9p.load_from_json(tree.v86());

e.jit_record_start();
e.v86.cpu.set_jit_config(4, THRESHOLD);
e.serial_send_bytes(1, new TextEncoder().encode(`go ${Date.now()} ${"ab".repeat(32)}\n`));
const status = await done;
if (status !== "exit 0" || !console_.includes("done")) {
  console.error(console_);
  throw new Error(`the start for the JIT failed: ${status}`);
}
const blob = e.jit_export(MODULES);
await e.destroy();
// the ID of the saved states of this v86, kernel and init (vmworker.js)
const { version } = JSON.parse(fs.readFileSync(new URL(import.meta.resolve("v86/package.json"))));
const build = await buildID(version, new Uint8Array(fs.readFileSync(`${vmDir}/vmlinux`)), new Uint8Array(fs.readFileSync(`${vmDir}/init`)));
fs.writeFileSync(out, packJIT(build, blob));
console.log(`${out}: build ${build}, ${(blob.byteLength / 2 ** 20).toFixed(2)} MiB`);
if (stateOut) {
  // zstd: 10.2 -> 2.4 MB (gzip -9: 3.0 MB); restore_state of v86 unpacks it
  const packed = zlib.zstdCompressSync(new Uint8Array(state), { params: { [zlib.constants.ZSTD_c_compressionLevel]: 19 } });
  fs.writeFileSync(stateOut, packState(build, MEMORY, packed));
  console.log(`${stateOut}: ${(state.byteLength / 2 ** 20).toFixed(1)} MiB, zstd ${(packed.byteLength / 2 ** 20).toFixed(1)} MiB`);
}
process.exit(0);
