// SharedWorker: the control plane that all Tabernetes tabs of the page share
// (k8s/tabernetes/tabs.go). It lives as long as one of the tabs is open, so
// any tab may close. The first tab hands over the binary it downloads, every
// tab joins as a node (tab-1, tab-2, ...). Each connection between a tab and
// the control plane is a MessagePort of its own.
import "./wasm_exec.js";
import { installFS } from "./fs.js";
import { clusterState, forget } from "./state.js";
import { zstdFrames, instantiate } from "./wasm.js";
import { decompress } from "fzstd";

const tabs = new Map(); // tab id -> MessagePort
// The docker nodes from join: every tab connects to each of them over WebRTC
// (web/src/join.js), and the node lives as long as one of these connections.
const peers = new Map(); // node name -> Set of tab ids with a connected peer
const statusLines = [];
let binary = null; // the tab that hands over the binary, once
let feed = null; // the controller of the stream its chunks go to, while they arrive
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
// The logs of the control plane: the console of this worker
// (chrome://inspect/#workers), and "logs" in the terminal of a tab.
const logs = [];
const crash = [];
let crashing = false;
installFS((fd, s) => {
  if (crashing || /^(panic:|fatal error:)/m.test(s)) {
    crashing = true;
    console.error(s);
    if (crash.length < 2000) crash.push(s);
  } else console.debug(s.replace(/\n$/, ""));
  // Expected in a single-threaded control plane: a controller found its
  // informer a few versions behind its own write and retries.
  if (s.includes("is not as new as written version")) return;
  logs.push(s);
  if (logs.length > 5000) logs.splice(0, logs.length - 5000);
});

// The cluster survives a reload: the control plane saves it (state.js).
const saved = clusterState();
globalThis.tabernetesState = saved;

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
    // A saved cluster that does not boot would not boot after a reload either.
    await globalThis.tabernetes.ready.catch(async (e) => { await saved.drop(); throw e; });
    if (state !== "booting") return; // it exited meanwhile
    state = "ready";
    resolveReady();
  } catch (e) {
    end("failed", e.message);
  }
}

function exited(why) {
  end("exited", why);
}

// end is the end of this control plane: it did not start, or it exited. The
// tabs say so. A worker cannot boot a second time, so it closes: the next tab
// that loads the page gets a new worker, and with it a new cluster. Without
// that, a reload would find the dead one for as long as another tab is open.
function end(how, why) {
  if (state === "failed" || state === "exited") return;
  state = how;
  failure = why;
  broadcast(how === "failed" ? { type: "failed", error: why } : { type: "exited", why, crash: crash.join("") });
  rejectReady(new Error(how === "failed" ? why : `the control plane exited${why ? `: ${why}` : ""}`));
  // after the answers to the tabs that wait in joinTab
  setTimeout(() => self.close(), 1000);
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
    const tab = tabs.get(id);
    if (tab) tab.nodes.push(info.name);
    else globalThis.tabernetes.dropNode(info.name); // closed meanwhile
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
  // forget: no more saves, and the saved cluster is gone.
  forget() {
    return forget();
  },
  // logs: the last n lines of the control plane logs, for "logs" in a tab.
  logs(id, n) {
    return logs.join("").split("\n").slice(-n - 1).join("\n");
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
          // It was to hand over the binary and did not get to the end of it:
          // the other tabs would wait for this control plane forever.
          const gone = "the tab that downloaded the control plane was closed";
          if (id !== binary || !(feed || state === "new")) return;
          end("failed", gone);
          feed?.error(new Error(gone));
          feed = null;
        });
        const needBinary = binary === null;
        if (needBinary) binary = id;
        port.postMessage({ type: "hello", needBinary, status: statusLines, state, failure });
        break;
      }
      case "binary": {
        // The tab sends the download as it arrives (compressed, about 30 MB),
        // in one message per chunk: not every browser can transfer a
        // ReadableStream (Safari).
        if (id !== binary || state !== "new") break;
        boot(new ReadableStream({ start(ctl) { feed = ctl; } }), data.size);
        break;
      }
      case "chunk": {
        if (id !== binary || !feed) break;
        const ctl = feed;
        if (data.done || data.error) feed = null;
        if (data.error) ctl.error(new Error(data.error));
        else if (data.done) ctl.close();
        else ctl.enqueue(new Uint8Array(data.data));
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
