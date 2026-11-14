// The container side of the tab node (k8s/tabernetes/v86.go), as host.vm
// of the session of a tab. Every pod with a container image gets its own
// worker (vmworker.js) with an x86 emulator in it. Stopping the pod ends the
// worker, and with it the VM.

import { parseJIT } from "./jit.js";

// The tab compiles v86.wasm once and gives the WebAssembly.Module to the
// worker of every pod. A worker that compiles the file itself starts with
// baseline code, and V8 optimises the hot functions again for every pod
// that starts while no other pod runs (it shares the code only between
// modules that are alive). The Module of the tab keeps the optimised code
// alive. Measured 2026-11-13 in headless Chromium (nginx, one pod after the
// other, each in a new worker, 18 pods per variant on a busy machine): go to
// "started" 180 -> 70 ms, go to the HTTP answer -11 %, and ~0.4 s less CPU
// per pod. compileStreaming also lets the browser keep the code in its
// cache across page loads.
const modules = new Map(); // URL -> Promise<WebAssembly.Module | undefined>
function wasmModule(url) {
  if (!modules.has(url)) {
    modules.set(url, (async () => {
      try {
        return await WebAssembly.compileStreaming(fetch(url));
      } catch {
        // no streaming (the MIME type, an old browser): the worker loads the file
        return undefined;
      }
    })());
  }
  return modules.get(url);
}

// The JIT modules of the kernel (jit.js): compiled once per tab, like
// v86.wasm, and posted to every pod. Without the file, the pods start
// without them.
const jits = new Map(); // URL -> Promise<object | null>
function jitModules(url) {
  if (!url) return Promise.resolve(null);
  if (!jits.has(url)) {
    jits.set(url, (async () => {
      try {
        const res = await fetch(url);
        return res.ok ? await parseJIT(await res.arrayBuffer()) : null;
      } catch {
        return null;
      }
    })());
  }
  return jits.get(url);
}

// prefetchJIT loads, compiles and warms jit.bin in this tab ahead of time.
export function prefetchJIT(url) {
  setTimeout(() => jitModules(url), 0);
}

// postStart posts the start message m to the worker of a pod, with the
// Module of m.wasmURL and the modules of m.jitURL. Only a tab can do that:
// a shared worker cannot post a Module (messageerror), so there the tab
// adds them (relay in bridge.js).
export async function postStart(worker, m, transfer = []) {
  const [module, jit] = await Promise.all([wasmModule(m.wasmURL), jitModules(m.jitURL)]);
  try {
    worker.postMessage(module ? { ...m, wasmModule: module, jit } : m, transfer);
  } catch {
    // the Module does not clone (another agent cluster): without it
    worker.postMessage(m, transfer);
  }
}

// spawn starts the worker. In the shared worker it is a worker of the tab
// (bridge.js), since a shared worker cannot start workers.
export function vmAPI({ version, workerURL, wasmURL, kernelURL, initURL, jitURL, registry, spawn }) {
  const abs = (u) => new URL(u, location.href).href;
  // The ID of the saved VM states (vmstate.js) of this kernel and init, as
  // the worker of a pod computed it. Later pods look for a state with it
  // first and need not fetch and hash the kernel.
  let build = "";
  const newWorker = () => (spawn ? spawn(abs(workerURL)) : new Worker(abs(workerURL)));
  // A spare worker boots the VM of the next pod ahead of time (vmworker.js
  // "warm"): it is made 2 s after a pod of this tab has started, and the
  // next pod takes it. That saves the start of the worker and the restore
  // of the VM state.
  let spare = null;
  const warmUp = () => {
    const w = newWorker();
    w.onmessage = ({ data: m }) => { if (m.type === "build") build = m.build; };
    w.onerror = (e) => { e.preventDefault(); if (spare === w) spare = null; };
    const m = { type: "warm", version, build, wasmURL: abs(wasmURL), kernelURL: abs(kernelURL), initURL: abs(initURL), jitURL: jitURL && abs(jitURL) };
    if (spawn) w.postMessage(m);
    else postStart(w, m);
    spare = w;
  };
  // Not while the pod that just started needs the CPU most.
  let timer = 0;
  const warmSoon = () => {
    if (spare || timer) return;
    timer = setTimeout(() => { timer = 0; if (!spare) warmUp(); }, 2000);
  };
  return {
    version, // of v86, for the container runtime of the node
    registry,

    // start pulls the image and boots it. opts: image, container (command,
    // args, env as "NAME=value", workingDir), hostname, ip, gateway, memory
    // (bytes, 0 for automatic). on: pulling(), pulled(info), started(info),
    // console(text), exit(code, message: why the entrypoint did not start),
    // error(message, pull, never: not present with the pull policy Never).
    start(opts, on) {
      const worker = spare || newWorker();
      spare = null;
      const conns = new Map();
      let next = 1;
      worker.onmessage = ({ data: m }) => {
        switch (m.type) {
          case "pulling": on.pulling(); break;
          case "pulled": on.pulled(m); break;
          case "started": on.started(m); warmSoon(); break;
          case "console": on.console(m.data); break;
          case "exit": on.exit(m.code, m.message || ""); break;
          case "error": on.error(m.message, !!m.pull, !!m.never); break;
          case "build": build = m.build; break;
          case "conn": {
            const c = conns.get(m.id);
            if (!c) break;
            if (m.event === "connect") c.onConnect();
            else if (m.event === "data") c.onData(m.data);
            else { conns.delete(m.id); c.onClose(); }
          }
        }
      };
      worker.onerror = (e) => { e.preventDefault(); on.error(e.message || "the VM worker failed", false); };
      const start = {
        type: "start",
        ...opts,
        registry,
        version,
        build,
        wasmURL: abs(wasmURL),
        kernelURL: abs(kernelURL),
        initURL: abs(initURL),
        jitURL: jitURL && abs(jitURL),
      };
      // With spawn, the tab adds the Module when it passes the message on.
      // Without, this is the tab: what follows waits for the start message.
      let order = spawn ? Promise.resolve(worker.postMessage(start)) : postStart(worker, start);
      const send = (m, transfer = []) => { order = order.then(() => worker.postMessage(m, transfer)); };
      return {
        // connect opens a TCP connection to port of the pod.
        connect(port, onConnect, onData, onClose) {
          const id = next++;
          conns.set(id, { onConnect, onData, onClose });
          send({ type: "connect", id, port });
          return {
            write(data) {
              const b = data.slice();
              send({ type: "write", id, data: b }, [b.buffer]);
            },
            close() {
              if (conns.delete(id)) send({ type: "close", id });
            },
          };
        },
        stop() {
          worker.terminate();
          for (const c of conns.values()) c.onClose();
          conns.clear();
        },
      };
    },
  };
}
