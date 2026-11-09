// SharedWorker: the control plane that all Tabernetes tabs of the page share
// (k8s/tabernetes/tabs.go). It lives as long as one of the tabs is open, so
// any tab may close. The first tab hands over the binary it downloads, every
// tab joins as a node (tab-1, tab-2, ...). Each connection between a tab and
// the control plane is a MessagePort of its own.
import "./wasm_exec.js";
import { installFS } from "./fs.js";
import { zstdFrames, instantiate } from "./wasm.js";
import { decompress } from "fzstd";

const tabs = new Map(); // tab id -> MessagePort
const statusLines = [];
let binary = null; // a tab hands over the binary once
let state = "new"; // new, booting, ready, failed, exited
let failure = "";
let resolveReady, rejectReady;
const ready = new Promise((res, rej) => { resolveReady = res; rejectReady = rej; });
ready.catch(() => {});

const broadcast = (msg) => { for (const port of tabs.values()) port.postMessage(msg); };

globalThis.tabernetesStatus = (text) => {
  statusLines.push(text);
  broadcast({ type: "status", text });
};
// The logs of the control plane: the console of this worker (chrome://inspect/#workers).
const crash = [];
let crashing = false;
installFS((fd, s) => {
  if (crashing || /^(panic:|fatal error:)/m.test(s)) {
    crashing = true;
    console.error(s);
    if (crash.length < 2000) crash.push(s);
  } else console.debug(s.replace(/\n$/, ""));
});

// boot compiles the binary while the tab sends it, then starts the control plane.
async function boot(stream, size) {
  state = "booting";
  try {
    const go = new globalThis.Go();
    go.env = { HOME: "/root", USER: "root", PATH: "/usr/bin" };
    // No workers in a shared worker: the frames unpack one after the other.
    const unpacked = new ReadableStream({
      async start(ctl) {
        try {
          for await (const frame of zstdFrames(stream, size)) ctl.enqueue(decompress(frame));
          ctl.close();
        } catch (e) { ctl.error(e); }
      },
    });
    const instance = await instantiate(unpacked, go.importObject);
    globalThis.tabernetesMemory = instance.exports.mem;
    go.run(instance).then(() => exited(""), (e) => exited(e?.message ?? String(e)));
    await globalThis.tabernetes.ready;
    state = "ready";
    resolveReady();
  } catch (e) {
    state = "failed";
    failure = e.message;
    broadcast({ type: "failed", error: failure });
    rejectReady(e);
  }
}

function exited(why) {
  state = "exited";
  failure = why;
  broadcast({ type: "exited", why, crash: crash.join("").split("\n").slice(0, 30).join("\n") });
  rejectReady(new Error("the control plane exited"));
}

// open opens a connection to a tab. extra.dc names a node that joined with
// docker: the tab opens a WebRTC data channel to it (web/src/join.js).
function open(id, label, extra = {}) {
  const port = tabs.get(id);
  if (!port) return Promise.reject(new Error("the tab is closed"));
  const ch = new MessageChannel();
  port.postMessage({ type: "connect", label, ...extra, port: ch.port2 }, [ch.port2]);
  return Promise.resolve(ch.port1);
}

const methods = {
  // joinTab adds the tab as a node and removes it once the tab is closed.
  async joinTab(id) {
    await ready;
    const info = await globalThis.tabernetes.prepareTab((label) => open(id, label));
    tabs.get(id)?.nodes.push(info.name);
    return info;
  },
  // prepareJoin is "join" (docker) in a tab: the WebRTC connection lives in
  // that tab.
  async prepareJoin(id) {
    await ready;
    let name = "";
    const info = await globalThis.tabernetes.prepareJoin((label) => open(id, label, { dc: name }));
    name = info.name;
    tabs.get(id)?.nodes.push(name);
    return info;
  },
};

self.onconnect = (e) => {
  const port = e.ports[0];
  let id = null;
  port.onmessage = async ({ data }) => {
    switch (data.type) {
      case "hello": {
        id = data.id;
        port.nodes = [];
        tabs.set(id, port);
        // The tab holds this lock while it is open.
        navigator.locks.request(data.lock, () => {
          tabs.delete(id);
          // Its node, and the nodes from join: their WebRTC connections ended with it.
          for (const name of port.nodes) globalThis.tabernetes?.dropNode(name);
        });
        const needBinary = binary === null;
        if (needBinary) binary = id;
        port.postMessage({ type: "hello", needBinary, status: statusLines, state, failure });
        break;
      }
      case "binary": {
        // The tab sends the download as it arrives (compressed, about 30 MB).
        if (data.stream) boot(data.stream, data.size);
        break;
      }
      case "connect":
        // A connection from the tab: "api" to the kube-apiserver.
        if (state === "ready") globalThis.tabernetes.attachPort(data.port, data.label);
        else data.port.postMessage(null);
        break;
      case "rpc": {
        try {
          const result = await methods[data.method](id, ...(data.args || []));
          port.postMessage({ type: "rpc", seq: data.seq, result });
        } catch (err) {
          port.postMessage({ type: "rpc", seq: data.seq, error: err.message });
        }
        break;
      }
    }
  };
};
