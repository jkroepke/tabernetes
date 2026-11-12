// Tests of vm/init on the host (Linux only): init runs as a normal process
// with TABERNETES_INIT_TEST, so it mounts nothing and takes the config from
// a file. The tests talk the exec protocol of the kubelet
// (k8s/tabernetes/v86exec.go) to it over TCP.
//
//   node --test vm/test
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

const linux = process.platform === "linux";
const dir = linux ? fs.mkdtempSync(path.join(os.tmpdir(), "tabernetes-init-")) : "";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const te = new TextEncoder();
const td = new TextDecoder();

before(() => {
  if (!linux) return;
  const src = new URL("../init/init.c", import.meta.url).pathname;
  execFileSync(process.env.CC || "cc", [...(process.env.CFLAGS || "-O2").split(" "), "-Wall", "-Wextra", "-Werror", "-o", path.join(dir, "init"), src], { stdio: "inherit" });
});
after(() => linux && fs.rmSync(dir, { recursive: true, force: true }));

let ports = 40000 + (process.pid % 10000);

// start runs init with the container config c and waits for "started".
async function start(c) {
  const port = ports++;
  const conf = path.join(dir, `config-${port}.json`);
  fs.writeFileSync(conf, JSON.stringify({ env: ["PATH=/usr/local/bin:/usr/bin:/bin"], ...c }));
  const p = spawn(path.join(dir, "init"), [], {
    env: { TABERNETES_INIT_TEST: conf, TABERNETES_INIT_PORT: String(port) },
    stdio: ["ignore", "pipe", "inherit"],
  });
  const lines = [];
  let rest = "";
  p.stdout.on("data", (b) => {
    rest += b;
    const l = rest.split("\n");
    rest = l.pop();
    lines.push(...l.map((s) => s.replace(/\r$/, "")));
  });
  const vm = { p, port, lines, stop: () => p.kill("SIGKILL") };
  await until(() => lines.length > 0);
  return vm;
}

async function until(pred, ms = 5000) {
  const t = Date.now();
  while (!pred()) {
    if (Date.now() - t > ms) throw new Error("timeout");
    await sleep(10);
  }
}

function frame(t, b = new Uint8Array()) {
  if (typeof b === "string") b = te.encode(b);
  const f = Buffer.alloc(5 + b.length);
  f[0] = t;
  f.writeUInt32BE(b.length, 1);
  f.set(b, 5);
  return f;
}

// session connects to init and sends the request line.
function session(vm, req) {
  return new Promise((resolve, reject) => {
    const c = net.connect(vm.port, "127.0.0.1");
    const s = { c, out: "", err: "", exit: null, error: null, acks: 0, closed: false };
    let buf = Buffer.alloc(0);
    s.send = (t, b) => c.write(frame(t, b));
    s.resize = (rows, cols) => {
      const b = Buffer.alloc(4);
      b.writeUInt16BE(rows, 0);
      b.writeUInt16BE(cols, 2);
      s.send(2, b);
    };
    s.wait = (pred, ms) => until(() => pred(s), ms).catch(() => {
      throw new Error("timeout: " + JSON.stringify({ out: s.out, err: s.err, exit: s.exit, error: s.error, acks: s.acks, closed: s.closed }));
    });
    s.done = () => s.wait((s) => s.closed);
    c.on("connect", () => {
      c.write(JSON.stringify(req) + "\n");
      resolve(s);
    });
    c.on("error", reject);
    c.on("data", (d) => {
      buf = Buffer.concat([buf, d]);
      while (buf.length >= 5) {
        const n = buf.readUInt32BE(1);
        if (buf.length < 5 + n) break;
        const t = buf[0], p = buf.subarray(5, 5 + n);
        buf = buf.subarray(5 + n);
        if (t === 1) s.out += td.decode(p);
        else if (t === 2) s.err += td.decode(p);
        else if (t === 3) s.exit = p.readInt32BE(0);
        else if (t === 4) s.error = td.decode(p);
        else if (t === 5) s.acks++;
      }
    });
    c.on("close", () => (s.closed = true));
  });
}

async function run(vm, req, fn) {
  const s = await session(vm, req);
  if (fn) await fn(s);
  await s.done();
  return s;
}

const alive = (pid) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};

test("exec", { skip: !linux }, async (t) => {
  const vm = await start({ args: ["sleep", "1000"] });
  t.after(vm.stop);
  assert.deepEqual(vm.lines, ["started"]);

  await t.test("output and exit code", async () => {
    let s = await run(vm, { cmd: ["echo", "hi"] });
    assert.deepEqual([s.out, s.exit], ["hi\n", 0]);
    s = await run(vm, { cmd: ["sh", "-c", "echo out; echo err >&2; exit 3"] });
    assert.deepEqual([s.out, s.err, s.exit], ["out\n", "err\n", 3]);
  });

  await t.test("a command that does not start", async () => {
    let s = await run(vm, { cmd: ["does-not-exist"] });
    assert.deepEqual([s.error, s.exit], ['exec: "does-not-exist": executable file not found in $PATH', null]);
    s = await run(vm, { cmd: [path.join(dir, "config-0.json")] });
    assert.match(s.error, /no such file or directory$/);
  });

  await t.test("stdin is acknowledged after each frame", async () => {
    const s = await run(vm, { cmd: ["cat"], stdin: true }, async (s) => {
      s.send(0, "abc");
      await s.wait((s) => s.acks === 1);
      s.send(0, "def");
      await s.wait((s) => s.acks === 2);
      s.send(1);
    });
    assert.deepEqual([s.out, s.exit, s.acks], ["abcdef", 0, 2]);
  });

  await t.test("large streams", async () => {
    let s = await run(vm, { cmd: ["sh", "-c", "head -c 4194304 /dev/zero | tr '\\0' x"] });
    assert.deepEqual([s.out.length, s.exit], [4194304, 0]);
    s = await run(vm, { cmd: ["wc", "-c"], stdin: true }, async (s) => {
      for (let i = 0; i < 64; i++) {
        s.send(0, new Uint8Array(32 << 10));
        await s.wait((s) => s.acks === i + 1);
      }
      s.send(1);
    });
    assert.deepEqual([s.out.trim(), s.exit], [String(64 * (32 << 10)), 0]);
  });

  await t.test("a frame before the acknowledgement ends the session", async () => {
    // the command does not read stdin: the second frame comes too early
    const s = await session(vm, { cmd: ["sh", "-c", "echo $$; exec sleep 300"], stdin: true });
    await s.wait((s) => s.out.includes("\n"));
    const big = new Uint8Array(1 << 20);
    for (let i = 0; i < 3; i++) s.send(0, big);
    await s.done();
    await until(() => !alive(+s.out.trim()));
  });

  await t.test("tty, resize and ^D at the end of stdin", async () => {
    let s = await run(vm, { cmd: ["sh"], tty: true, stdin: true, rows: 30, cols: 100 }, async (s) => {
      s.send(0, "stty size; echo T=$TERM; tty\n");
      await s.wait((s) => s.out.includes("/dev/pts/"));
      s.resize(40, 120);
      await sleep(50);
      s.send(0, "stty size; exit 5\n");
    });
    assert.ok(s.out.includes("30 100"), s.out);
    assert.ok(s.out.includes("T=xterm"), s.out);
    assert.ok(s.out.includes("40 120"), s.out);
    assert.deepEqual([s.err, s.exit], ["", 5]);
    s = await run(vm, { cmd: ["sh"], tty: true, stdin: true }, async (s) => {
      s.send(0, "echo x\n");
      await s.wait((s) => s.out.includes("x\r\n"));
      s.send(1);
    });
    assert.equal(s.exit, 0);
  });

  await t.test("a dropped connection kills the process group", async () => {
    const s = await session(vm, { cmd: ["sh", "-c", "sleep 300 & echo $! $$; wait"], stdin: true });
    await s.wait((s) => s.out.includes("\n"));
    const pids = s.out.trim().split(" ").map(Number);
    s.c.destroy();
    await until(() => !pids.some(alive));
  });

  await t.test("exit after one second when a background process keeps the output open", async () => {
    const t0 = Date.now();
    const s = await run(vm, { cmd: ["sh", "-c", "(sleep 5; echo late) & echo early"] });
    assert.deepEqual([s.out, s.exit], ["early\n", 0]);
    assert.ok(Date.now() - t0 < 3000);
  });

  await t.test("parallel sessions", async () => {
    const all = await Promise.all(Array.from({ length: 8 }, (_, i) => run(vm, { cmd: ["sh", "-c", `echo ${i}`] })));
    assert.deepEqual(all.map((s) => s.out + s.exit), Array.from({ length: 8 }, (_, i) => `${i}\n0`));
  });
});

test("attach", { skip: !linux }, async (t) => {
  const main = ["sh", "-c", "while read l; do echo got:$l; done; echo bye; exit 4"];

  await t.test("output, stdin and a disconnect", async (t) => {
    const vm = await start({ args: main, stdin: true });
    t.after(vm.stop);
    const a = await session(vm, { attach: true, stdin: true });
    a.send(0, "hello\n");
    await a.wait((s) => s.acks === 1 && s.out.includes("got:hello"));
    const b = await session(vm, { attach: true, stdin: true });
    a.c.destroy();
    b.send(0, "two\n");
    await b.wait((s) => s.out.includes("got:two"));
    // the main process lives on after a disconnect
    await sleep(100);
    assert.deepEqual(vm.lines, ["started"]);
    b.c.destroy();
  });

  await t.test("stdinOnce: the end of stdin is EOF for the main process", async (t) => {
    const vm = await start({ args: main, stdin: true, stdinOnce: true });
    t.after(vm.stop);
    const a = await session(vm, { attach: true, stdin: true });
    a.send(0, "hello\n");
    await a.wait((s) => s.acks === 1 && s.out.includes("got:hello"));
    a.send(1);
    await a.done();
    assert.deepEqual([a.out, a.exit], ["got:hello\nbye\n", 4]);
    await until(() => vm.lines.includes("exit 4"));
  });
});

test("start errors", { skip: !linux }, async (t) => {
  const vm = await start({ args: ["does-not-exist"] });
  t.after(vm.stop);
  await until(() => vm.lines.length >= 2);
  assert.deepEqual(vm.lines, ['error exec: "does-not-exist": executable file not found in $PATH', "exit 128"]);
});

test("exit code of the main process", { skip: !linux }, async (t) => {
  const vm = await start({ args: ["sh", "-c", "exit 7"] });
  t.after(vm.stop);
  await until(() => vm.lines.length >= 2);
  assert.deepEqual(vm.lines, ["started", "exit 7"]);
});
