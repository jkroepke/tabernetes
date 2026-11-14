// The session of a tab in the shared worker (k8s/tabernetes/session.go):
// the shell, kubectl and the kubelet of the tab run in the Go of the
// control plane, the tab only shows the terminal. One MessagePort per tab
// carries both directions:
//
//   tab -> worker   the calls of the terminal (shell, complete, key, ...),
//                   the output of the builtins of the terminal, dial
//   worker -> tab   the output of the shell, calls of the builtins of the
//                   terminal, status lines, and the workers of the pods
//
// A shared worker cannot start workers, so the VMs (vmworker.js) and the
// language models (gpuworker.js) of the pods of a tab run in workers of the
// tab. The worker asks the tab to start one ("spawn") and talks to it
// through a MessagePort that the tab relays. vm.js and gpu.js run in the
// shared worker unchanged, with spawn in place of new Worker.

import { vmAPI, postStart } from "./vm.js";
import { gpuAPI } from "./gpu.js";

// ---- the shared worker ----

// serveTab opens the session of a tab on port. cfg is what the tab can run:
// cfg.vm (the options of vmAPI, with absolute URLs) and cfg.gpu (the URL of
// gpuworker.js), both optional. It resolves to the session (session.go).
export async function serveTab(port, cfg) {
  let seq = 0;
  const calls = new Map(); // calls to the tab
  const builtinCalls = new Map(); // builtins of the terminal that run
  let next = 0;
  const call = (m, ...a) => new Promise((resolve, reject) => {
    calls.set(++seq, { resolve, reject });
    port.postMessage({ t: "hcall", seq, m, a });
  });
  const spawn = (url) => remoteWorker(port, url);
  const host = { status: (s) => port.postMessage({ t: "status", s }) };
  if (cfg?.vm) host.vm = vmAPI({ ...cfg.vm, spawn });
  if (cfg?.gpu) host.gpu = gpuAPI(cfg.gpu, { spawn, adapter: () => call("gpuAdapter") });
  const session = await globalThis.tabernetes.openTab(host);

  const ret = (seq, v, transfer = []) => port.postMessage({ t: "ret", seq, v }, transfer);
  const fail = (seq, e) => port.postMessage({ t: "ret", seq, e: String(e?.message ?? e) });
  port.onmessage = async ({ data: d }) => {
    switch (d.t) {
      case "call": {
        try {
          if (d.m === "proxy") {
            const r = await globalThis.tabernetes.proxy(...d.a);
            ret(d.seq, r, r.body ? [r.body.buffer] : []);
          } else ret(d.seq, await session[d.m](...d.a));
        } catch (e) { fail(d.seq, e); }
        break;
      }
      case "shell": {
        // The builtins of the terminal run in the tab: a stub for each.
        const builtins = {};
        for (const name of d.names) {
          builtins[name] = (args, ctx) => new Promise((resolve, reject) => {
            const id = ++next;
            builtinCalls.set(id, { resolve, reject, write: ctx.write });
            port.postMessage({ t: "bi", id, name, args: [...args], stdin: ctx.stdin });
          });
        }
        session.shell(d.src, (s) => port.postMessage({ t: "out", s }), builtins).then((v) => ret(d.seq, v), (e) => fail(d.seq, e));
        break;
      }
      case "biw": builtinCalls.get(d.id)?.write(d.s); break;
      case "bir": {
        const c = builtinCalls.get(d.id);
        builtinCalls.delete(d.id);
        if (c) "e" in d ? c.reject(new Error(d.e)) : c.resolve(d.v);
        break;
      }
      case "dial": {
        // a TCP connection to localhost:<port> of the Go (dial.go), for vnc
        const p = d.p;
        globalThis.tabernetes.dial(d.port, (b) => p.postMessage(b, [b.buffer]), () => { p.postMessage(null); p.close(); }).then((conn) => {
          p.onmessage = ({ data }) => (data == null ? conn.close() : conn.send(new Uint8Array(data)));
          ret(d.seq, true);
        }, (e) => { p.close(); fail(d.seq, e); });
        break;
      }
      case "hret": {
        const c = calls.get(d.seq);
        calls.delete(d.seq);
        if (c) "e" in d ? c.reject(new Error(d.e)) : c.resolve(d.v);
        break;
      }
    }
  };
  return session;
}

// remoteWorker is a Worker of the tab, as far as vm.js and gpu.js use one:
// postMessage, onmessage, onerror and terminate.
function remoteWorker(port, url) {
  const ch = new MessageChannel();
  port.postMessage({ t: "spawn", url, p: ch.port2 }, [ch.port2]);
  const p = ch.port1;
  const w = {
    onmessage: null,
    onerror: null,
    postMessage(m, transfer = []) { p.postMessage({ m, x: transfer }, transfer); },
    terminate() { p.postMessage({ end: true }); p.close(); },
  };
  p.onmessage = ({ data }) => {
    if ("error" in data) w.onerror?.({ message: data.error, preventDefault() {} });
    else w.onmessage?.({ data: data.m });
  };
  return w;
}

// ---- the tab ----

// tabSession is the session of this tab in the shared worker, over port.
// The methods are those of session.go, but all return Promises. host has
// status(text) and what runs the workers of the pods of this tab.
export function tabSession(port, { status }) {
  let seq = 0;
  const calls = new Map();
  let builtins = {}; // of the command line that runs
  let write = () => {};
  const workers = new Set(); // of the pods of this tab, started for the worker
  const call = (m, a, extra = {}, transfer = []) => new Promise((resolve, reject) => {
    calls.set(++seq, { resolve, reject });
    port.postMessage({ t: "call", seq, m, a, ...extra }, transfer);
  });
  const answer = (seq, p) => Promise.resolve(p).then(
    (v) => port.postMessage({ t: "hret", seq, v }),
    (e) => port.postMessage({ t: "hret", seq, e: String(e?.message ?? e) }),
  );
  port.onmessage = ({ data: d }) => {
    switch (d.t) {
      case "ret": {
        const c = calls.get(d.seq);
        calls.delete(d.seq);
        if (c) "e" in d ? c.reject(new Error(d.e)) : c.resolve(d.v);
        break;
      }
      case "out": write(d.s); break;
      case "status": status(d.s); break;
      case "bi": {
        const fn = builtins[d.name];
        const ctx = { stdin: d.stdin, write: (s) => port.postMessage({ t: "biw", id: d.id, s: String(s) }) };
        Promise.resolve().then(() => fn(d.args, ctx)).then(
          (v) => port.postMessage({ t: "bir", id: d.id, v: typeof v === "number" ? v : undefined }),
          (e) => port.postMessage({ t: "bir", id: d.id, e: String(e?.message ?? e) }),
        );
        break;
      }
      case "hcall":
        if (d.m === "gpuAdapter") answer(d.seq, gpuAdapter());
        else answer(d.seq, Promise.reject(new Error(`unknown call ${d.m}`)));
        break;
      case "spawn": relay(d.url, d.p, workers); break;
    }
  };
  const method = (m) => (...a) => call(m, a);
  return {
    shell(src, out, fns = {}) {
      builtins = fns;
      write = out;
      return new Promise((resolve, reject) => {
        calls.set(++seq, { resolve, reject });
        port.postMessage({ t: "shell", seq, src, names: Object.keys(fns) });
      });
    },
    incomplete: method("incomplete"),
    commands: method("commands"),
    key: method("key"),
    resize: method("resize"),
    complete: method("complete"),
    cwd: method("cwd"),
    cancel: method("cancel"),
    // stop ends the workers of the pods of this tab: the control plane is
    // gone (poweroff, reboot, a crash), and with it their kubelet.
    stop() {
      for (const w of workers) w.terminate();
      workers.clear();
    },
    proxy: (...a) => call("proxy", a),
    // dial(port, onData, onClose): Promise<{send, close}>, like dial.go
    async dial(p, onData, onClose) {
      const ch = new MessageChannel();
      const mine = ch.port1;
      mine.onmessage = ({ data }) => {
        if (data == null) { mine.close(); onClose(); } else onData(data);
      };
      const id = ++seq;
      await new Promise((resolve, reject) => {
        calls.set(id, { resolve, reject });
        port.postMessage({ t: "dial", seq: id, port: p, p: ch.port2 }, [ch.port2]);
      }).catch((e) => { mine.close(); throw e; });
      return {
        send(b) { const c = b.slice(); mine.postMessage(c.buffer, [c.buffer]); },
        close() { mine.postMessage(null); },
      };
    },
  };
}

// gpuAdapter is gpu.js adapter() in this tab: the shared worker may have no
// WebGPU.
function gpuAdapter() {
  return gpuAPI("", {}).adapter();
}

// relay starts a worker for the shared worker and passes the messages on.
// workers holds it while it runs.
function relay(url, p, workers) {
  const w = new Worker(url);
  workers.add(w);
  w.onmessage = ({ data }) => p.postMessage({ m: data }, buffers(data));
  w.onerror = (e) => { e.preventDefault(); p.postMessage({ error: e.message || "the worker failed" }); };
  // The start message of a pod gets the Module of v86.wasm of this tab
  // (postStart in vm.js). That takes a moment the first time: the messages
  // keep their order.
  let order = Promise.resolve();
  p.onmessage = ({ data }) => {
    if (data.end) { w.terminate(); workers.delete(w); p.close(); return; }
    const m = data.m;
    order = order.then(() => (m?.type === "start" && m.wasmURL ? postStart(w, m, data.x || []) : w.postMessage(m, data.x || [])));
  };
}

// buffers are the ArrayBuffers of a message from a worker of a pod: they
// move on to the shared worker instead of being copied.
function buffers(m) {
  const out = new Set();
  const add = (v) => {
    if (v instanceof ArrayBuffer) out.add(v);
    else if (ArrayBuffer.isView(v) && v.buffer instanceof ArrayBuffer) out.add(v.buffer);
  };
  if (m && typeof m === "object") Object.values(m).forEach(add);
  add(m);
  return [...out];
}
