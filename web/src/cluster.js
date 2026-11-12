// SharedWorker: the control plane that all Tabernetes tabs of the page share
// (k8s/tabernetes/tabs.go). It lives as long as one of the tabs is open, so
// any tab may close, and a short time longer (extendedLifetime, tabs.js), so
// that a reload finds it. The first tab hands over the binary it downloads, every
// tab joins as a node (tab-1, tab-2, ...). The shell, kubectl and the
// kubelet of every tab run here too, in a session of the tab
// (k8s/tabernetes/session.go, bridge.js): a tab runs no Go itself.
import "./wasm_exec.js";
import { installFS } from "./fs.js";
import { capture, pack, resume, trackFuncs, unpack } from "./hibernate.js";
import { clusterState, dropHibernated, forget, loadHome, saveHibernated, saveHome, takeHibernated } from "./state.js";
import { instantiate, runGo } from "./wasm.js";
import { serveTab } from "./bridge.js";

const tabs = new Map(); // tab id -> MessagePort
// The docker nodes from join: every tab connects to each of them over WebRTC
// (web/src/join.js), and the node lives as long as one of these connections.
const peers = new Map(); // node name -> Set of tab ids with a connected peer
const statusLines = [];
let binary = null; // the tab that hands over the binary, once
let feed = null; // the controller of the stream its chunks go to, while they arrive
let state = "new"; // new, booting, ready, failed, exited
let started = false; // the control plane runs, or it did
let failure = "";
let forgotten = false; // forget ran: no reload may find this cluster
let go = null; // the Go of wasm_exec.js that runs the control plane
let resolveReady, rejectReady;
const ready = new Promise((res, rej) => { resolveReady = res; rejectReady = rej; });
ready.catch(() => {});

const broadcast = (msg) => { for (const port of tabs.values()) port.postMessage(msg); };

globalThis.tabernetesStatus = (text) => {
  statusLines.push(text);
  broadcast({ type: "status", text });
};
// The ports of kubectl port-forward (forwards.go): localhost is the one of
// this worker, so every tab links them.
let forwards = [];
globalThis.tabernetesForwards = (list) => {
  forwards = list;
  broadcast({ type: "forwards", list });
};
// The logs of the control plane: the console of this worker
// (chrome://inspect/#workers), and "logs" in the terminal of a tab.
const logs = [];
const crash = [];
let crashing = false;
// The files of /root from the last visit (state.js): the shells of all tabs
// share them.
const home = loadHome().catch(() => []);
let fsys = null; // the in-memory file system of Go (fs.js)
const output = (fd, s) => {
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
};

// The cluster survives a reload: the control plane saves it (state.js).
const saved = clusterState();
globalThis.tabernetesState = saved;
// The control plane of the last visit, hibernated when its last tab closed
// (hibernate.js). It unpacks while the tab sends the binary.
const hibernated = takeHibernated().then(async (image) => {
  if (image) image.data = await unpack(image.data);
  return image;
}).catch((e) => {
  console.warn("tabernetes: the hibernated control plane:", e);
  return null;
});
let resumed = false; // this control plane was hibernated, it did not boot
// hibernation counts the hibernations that started: a tab that joins, or
// forget, makes a running one void. imaged: this worker saved an image.
let hibernation = 0, imaged = false;

// boot compiles the binary while the tab sends it, then starts the control plane.
async function boot(stream) {
  state = "booting";
  try {
    fsys = installFS(output, { records: await home, save: saveHome });
    go = new globalThis.Go();
    go.env = { HOME: "/root", USER: "root", PATH: "/usr/bin" };
    trackFuncs(go);
    // The tab sends the binary unpacked: a shared worker cannot start
    // workers, so it would unpack the zstd frames one after the other.
    const instance = await instantiate(stream, go);
    globalThis.tabernetesMemory = instance.exports.mem;
    // "uname -r" reads the release (TABERNETES_VERSION of the build).
    globalThis.tabernetesVersion = typeof TABERNETES_VERSION === "string" ? TABERNETES_VERSION : "dev";
    started = true;
    // A hibernated control plane runs only in the binary it was taken from.
    const image = await hibernated;
    let running;
    if (image?.build && image.build === go.snapshot?.build) {
      globalThis.tabernetesStatus(`resuming the control plane of your last visit (hibernated ${ago(image.at)})`);
      resumed = true;
      running = resume(go, instance, image, fsys);
    } else running = runGo(go, instance);
    running.then(() => exited(""), (e) => exited(e?.message ?? String(e)));
    await globalThis.tabernetes.ready;
    if (state !== "booting") return; // it exited meanwhile
    // The kube-apiserver reaches the kubelets of the paired nodes through
    // the tabs that dial them. After a resume, the functions that did so
    // belonged to the worker that hibernated: they are new ones now.
    for (const { name } of globalThis.tabernetes.pairedNodes()) {
      const err = globalThis.tabernetes.restoreNode(name, (label) => openNode(name, label));
      if (err) console.warn(`tabernetes: ${name}:`, err);
    }
    state = "ready";
    resolveReady();
  } catch (e) {
    end("failed", e.message);
  }
}

const ago = (t) => {
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  return s < 120 ? `${s} s ago` : s < 7200 ? `${Math.round(s / 60)} min ago` : `${Math.round(s / 3600)} h ago`;
};

// hibernate saves the running control plane once its last tab is gone, so
// that the next visit resumes it instead of booting it (hibernate.js). The
// worker lives about 30 s after the last tab (extendedLifetime, Chrome 148+);
// browsers that end it at once get no image and boot as before.
//
// The etcd snapshot (state.go) stops at the same moment: the image is the
// newer cluster, and a cluster that goes on without tabs only changes what
// the next visit drops anyway (nodes, leases, events).
async function hibernate() {
  const gen = ++hibernation;
  // dropNode runs in Go in the background: wait until the sessions of the
  // closed tabs are closed and their nodes deleted, so that the image holds
  // no half-deleted node.
  const api = globalThis.tabernetes;
  for (let i = 0; i < 100 && (api.sessions() || api.dropping()); i++) await new Promise((r) => setTimeout(r, 100));
  await new Promise((r) => setTimeout(r, 0)); // a task of its own: Go is paused
  if (tabs.size || forgotten || state !== "ready" || gen !== hibernation) return;
  try {
    const t0 = performance.now();
    const image = capture(go, fsys);
    saved.pause(true);
    image.build = go.snapshot?.build;
    image.data = await pack(image.data);
    if (tabs.size || forgotten || gen !== hibernation) return;
    imaged = true;
    await saveHibernated(image);
    if (gen !== hibernation) await dropHibernated();
    else console.info(`tabernetes: hibernated in ${Math.round(performance.now() - t0)} ms (Go paused for ${Math.round(image.ms)} ms), ${(image.data.size / 2 ** 20).toFixed(1)} MiB; tab objects left behind:`, image.dead);
  } catch (e) {
    console.warn("tabernetes: hibernating:", e);
  }
}

// unhibernate voids the image: a tab joins the control plane again and may
// change the cluster.
function unhibernate() {
  hibernation++;
  saved.pause(false);
  if (imaged) dropHibernated();
  imaged = false;
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
  // A saved cluster that does not boot, or that crashes the control plane
  // while it boots, would do so after every reload: the next one starts new.
  // A hibernated one that fails is not the fault of the saved cluster: the
  // image is gone already (takeHibernated), the next visit boots from it.
  const dropped = started && state === "booting" && !resumed ? saved.drop() : Promise.resolve();
  state = how;
  failure = why;
  // The tabs keep the last lines of the logs: "logs" cannot ask for them anymore.
  const last = logs.join("").split("\n").slice(-500).join("\n");
  broadcast(how === "failed" ? { type: "failed", error: why, logs: last } : { type: "exited", why, crash: crash.join(""), logs: last });
  rejectReady(new Error(how === "failed" ? why : `the control plane exited${why ? `: ${why}` : ""}`));
  // after the answers to the tabs that wait in joinTab, and after the drop
  const close = () => setTimeout(() => self.close(), 1000);
  Promise.race([dropped, new Promise((r) => setTimeout(r, 5000))]).then(close, close);
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
  // joinTab opens the session of the tab, with its node, on the port of the
  // "session" message. cfg: what the tab runs (bridge.js). The tab is
  // removed once it is closed.
  async joinTab(id, cfg) {
    await ready;
    const tab = tabs.get(id);
    if (!tab?.session) throw new Error("the tab has no session port");
    const session = await serveTab(tab.session, cfg);
    if (tabs.get(id)) tab.nodes.push(session.name);
    else globalThis.tabernetes.dropNode(session.name); // closed meanwhile
    return { name: session.name, ip: session.ip, forwards };
  },
  // prepareJoin is "join" (docker) in a tab: the WebRTC connections live in
  // the tabs, one per tab.
  // relay: join --keep, the node is paired (k8s/tabernetes/paired.go).
  async prepareJoin(id, relay) {
    await ready;
    let name = "";
    const info = await globalThis.tabernetes.prepareJoin((label) => openNode(name, label, id), relay);
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
  // prepareKubectl is join --kubectl: a paired kubectl outside the browser.
  async prepareKubectl(id, relay) {
    await ready;
    return globalThis.tabernetes.prepareKubectl(relay);
  },
  // pairedNodes: the paired docker nodes, every tab dials them (join.js).
  async pairedNodes() {
    await ready;
    return globalThis.tabernetes.pairedNodes();
  },
  // unjoin: remove a paired node.
  async unjoin(id, name) {
    await ready;
    await globalThis.tabernetes.unjoin(name);
    peers.delete(name);
  },
  // meshNodes: the docker nodes a new tab connects to.
  meshNodes() {
    return [...peers].filter(([, ids]) => ids.size).map(([name]) => name);
  },
  // meshOffer: the offer of the tab to the node, the answer comes back.
  meshOffer(id, name, sdp) {
    return signal(id, name, sdp);
  },
  // forget: no more saves, and the saved cluster is gone. The cluster runs
  // on until the last tab closes, but not beyond: the next visit starts new.
  forget() {
    forgotten = true;
    hibernation++;
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
        unhibernate();
        // The tab holds this lock while it is open.
        navigator.locks.request(data.lock, () => {
          tabs.delete(id);
          if (forgotten && !tabs.size) { self.close(); return; }
          // Its node, and the docker nodes that no other tab is connected to.
          for (const name of port.nodes) globalThis.tabernetes?.dropNode(name);
          leave(id, port.joined);
          if (!tabs.size && state === "ready") hibernate();
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
        // The tab sends the binary as it unpacks it (about 290 MB), in one
        // message per chunk: not every browser can transfer a
        // ReadableStream (Safari).
        if (id !== binary || state !== "new") break;
        boot(new ReadableStream({ start(ctl) { feed = ctl; } }));
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
      case "session":
        // the port of the session of the tab (bridge.js), before joinTab
        port.session = data.port;
        break;
      case "connect":
        // A data channel of a docker node, bridged by the tab: "api" to the
        // kube-apiserver.
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
