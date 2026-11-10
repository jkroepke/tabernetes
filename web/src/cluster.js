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
// The docker nodes from join: every tab connects to each of them over WebRTC
// (web/src/join.js), and the node lives as long as one of these connections.
const peers = new Map(); // node name -> Set of tab ids with a connected peer
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

// openNode opens a data channel to a docker node through one of the tabs that
// are connected to it, or else through the tab that ran join.
function openNode(name, label, fallback) {
  const id = [...(peers.get(name) || [])].find((id) => tabs.has(id)) ?? fallback;
  return open(id, label, { dc: name });
}

// signal hands the offer of a tab to a docker node and returns its answer,
// through a data channel "signal" of a tab that is connected to the node.
async function signal(from, name, sdp) {
  for (const id of peers.get(name) || []) {
    if (id === from || !tabs.has(id)) continue;
    try {
      const port = await open(id, "signal", { dc: name });
      return await new Promise((resolve, reject) => {
        const t = setTimeout(() => { port.close(); reject(new Error("the node does not answer")); }, 20000);
        port.onmessage = ({ data }) => {
          clearTimeout(t);
          port.postMessage(null);
          port.close();
          if (data == null) reject(new Error("the node closed the signal channel"));
          else resolve(JSON.parse(new TextDecoder().decode(data)).sdp);
        };
        const b = new TextEncoder().encode(JSON.stringify({ sdp })).buffer;
        port.postMessage(b, [b]);
      });
    } catch { /* the next tab */ }
  }
  throw new Error(`no tab is connected to ${name}`);
}

// leave forgets the peers of a closed tab. A docker node without a connected
// tab is gone: its agent ends.
function leave(id, joined) {
  for (const [name, ids] of peers) {
    if (ids.delete(id) && !ids.size) {
      peers.delete(name);
      globalThis.tabernetes?.dropNode(name);
    }
  }
  for (const name of joined) if (!peers.has(name)) globalThis.tabernetes?.dropNode(name);
}

const methods = {
  // joinTab adds the tab as a node and removes it once the tab is closed.
  async joinTab(id) {
    await ready;
    const info = await globalThis.tabernetes.prepareTab((label) => open(id, label));
    tabs.get(id)?.nodes.push(info.name);
    return info;
  },
  // prepareJoin is "join" (docker) in a tab: the WebRTC connections live in
  // the tabs, one per tab.
  async prepareJoin(id) {
    await ready;
    let name = "";
    const info = await globalThis.tabernetes.prepareJoin((label) => openNode(name, label, id));
    name = info.name;
    tabs.get(id)?.joined.push(name);
    return info;
  },
  // meshUp: the tab is connected to the docker node name. The other tabs
  // connect to it too.
  meshUp(id, name) {
    if (!peers.has(name)) peers.set(name, new Set());
    peers.get(name).add(id);
    for (const [other, port] of tabs) if (other !== id) port.postMessage({ type: "mesh", name });
  },
  // meshDown: the connection of the tab to the node failed.
  meshDown(id, name) {
    peers.get(name)?.delete(id);
  },
  // meshNodes: the docker nodes a new tab connects to.
  meshNodes() {
    return [...peers].filter(([, ids]) => ids.size).map(([name]) => name);
  },
  // meshOffer: the offer of the tab to the node, the answer comes back.
  meshOffer(id, name, sdp) {
    return signal(id, name, sdp);
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
        port.joined = [];
        tabs.set(id, port);
        // The tab holds this lock while it is open.
        navigator.locks.request(data.lock, () => {
          tabs.delete(id);
          // Its node, and the docker nodes that no other tab is connected to.
          for (const name of port.nodes) globalThis.tabernetes?.dropNode(name);
          leave(id, port.joined);
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
