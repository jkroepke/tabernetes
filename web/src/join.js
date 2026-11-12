// "join" and "accept": connect a real kubelet to the in-browser control plane
// using WebRTC data channels. Signaling is copy & paste, no server involved.
// "join --keep" pairs the node instead: it stays in the cluster, and the tabs
// reach it again after a reload through a relay (relay.js). "unjoin" ends
// the pairing.
import { DEFAULT_RELAY, exchange } from "./relay.js";

const B = (s) => `\x1b[1m${s}\x1b[0m`;
const DIM = (s) => `\x1b[2m${s}\x1b[0m`;

// Instead of --privileged. See tabernetes/README.md for why each flag is needed.
const NODE_FLAGS = "--pull always --memory 2g --pids-limit 4096 --security-opt seccomp=unconfined --security-opt systempaths=unconfined --security-opt apparmor=unconfined --device /dev/net/tun";

async function deflate(str) {
  const s = new Blob([str]).stream().pipeThrough(new CompressionStream("deflate-raw"));
  return new Uint8Array(await new Response(s).arrayBuffer());
}
async function inflate(bytes) {
  const s = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("deflate-raw"));
  return await new Response(s).text();
}
const b64url = (u8) => btoa(String.fromCharCode(...u8)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const unb64url = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));

export async function encodeToken(obj) { return b64url(await deflate(JSON.stringify(obj))); }
export async function decodeToken(s) { return JSON.parse(await inflate(unb64url(s.trim()))); }

function gatheringComplete(pc, timeout) {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const t = setTimeout(resolve, timeout);
    pc.addEventListener("icegatheringstatechange", () => {
      if (pc.iceGatheringState === "complete") { clearTimeout(t); resolve(); }
    });
  });
}

const STATES = {
  new: ["waiting for accept", "#d29922"],
  connecting: ["connecting", "#d29922"],
  connected: ["connected", "#3fb950"],
  disconnected: ["disconnected", "#d29922"],
  failed: ["failed", "#f85149"],
  closed: ["closed", "#8b949e"],
};

// joinBuiltins returns the join and accept commands. status is an element
// that shows every node and the state of its WebRTC connection.
export function joinBuiltins({ image, stun = "stun:stun.l.google.com:19302", relay = DEFAULT_RELAY, status, cluster }) {
  const nodes = [];
  let pending = null;
  let seq = 0;
  // The paired nodes this tab dials (join --keep): name -> {p, delay, timer}.
  const paired = new Map();

  const render = () => {
    if (!status) return;
    if (!nodes.length) {
      // With a shared worker, every tab is a node already (tabs.js).
      status.textContent = cluster ? "This tab is a node. Type join to connect a Docker node." : "No nodes. Type join to connect one.";
      return;
    }
    status.replaceChildren(...nodes.map(({ info, pc }) => {
      const [text, color] = !pc ? ["waiting for the node", "#d29922"]
        : STATES[pc.connectionState] || [pc.connectionState, "#8b949e"];
      const el = document.createElement("span");
      el.className = "tabernetes-node";
      const dot = document.createElement("span");
      dot.textContent = "\u25CF ";
      dot.style.color = color;
      el.append(dot, `${info.name} ${text}`);
      return el;
    }));
  };
  render();

  // channels returns open(label) for the data channels a tab opens to a node.
  const channels = (pc) => (label) => new Promise((resolve, reject) => {
    const dc = pc.createDataChannel(label);
    const t = setTimeout(() => reject(new Error("data channel timeout")), 15000);
    dc.onopen = () => { clearTimeout(t); resolve(dc); };
    dc.onerror = (e) => { clearTimeout(t); reject(e.error || new Error("data channel error")); };
    dc.onclose = () => { clearTimeout(t); reject(new Error("data channel closed")); };
  });

  // watch follows the connection of a node. With the control plane in the
  // shared worker, every tab connects to every docker node; the worker learns
  // which tabs it can reach the node through.
  const watch = (pc, name) => {
    let up = false;
    pc.onconnectionstatechange = () => {
      render();
      if (!cluster) {
        // A failed connection never recovers: free it and its data channels.
        if (pc.connectionState === "failed") pc.close();
        return;
      }
      if (pc.connectionState === "connected" && !up) {
        up = true;
        cluster.rpc("meshUp", name);
      } else if (pc.connectionState === "failed") {
        if (up) cluster.rpc("meshDown", name);
        up = false;
        pc.close();
        if (cluster.dataChannels.get(name)?.pc === pc) cluster.dataChannels.delete(name);
      }
    };
  };

  // meshConnect connects this tab to a docker node that joined through
  // another tab: the offer goes there through the shared worker.
  const meshConnect = async (name) => {
    if (nodes.some((n) => n.info.name === name && alive(n.pc))) return;
    drop(name);
    const pc = new RTCPeerConnection({ iceServers: stun ? [{ urls: stun }] : [] });
    const entry = { pc, info: { name } };
    nodes.push(entry);
    pc.createDataChannel("ctl");
    pc.ondatachannel = (e) => cluster.bridge(e.channel);
    const open = channels(pc);
    open.pc = pc;
    cluster.dataChannels.set(name, open);
    watch(pc, name);
    render();
    try {
      await pc.setLocalDescription(await pc.createOffer());
      await gatheringComplete(pc, 3000);
      const sdp = await cluster.rpc("meshOffer", name, pc.localDescription.sdp);
      await pc.setRemoteDescription({ type: "answer", sdp });
    } catch (e) {
      console.warn(`tabernetes: connecting to ${name}:`, e);
      pc.close();
      nodes.splice(nodes.indexOf(entry), 1);
      if (cluster.dataChannels.get(name) === open) cluster.dataChannels.delete(name);
      render();
    }
  };
  if (cluster) {
    // poweroff and reboot: this cluster is gone, and the next boot makes new
    // builtins. These stop dialing and close their connections.
    cluster.on("closed", () => {
      for (const s of paired.values()) clearTimeout(s.timer);
      paired.clear();
      for (const n of nodes) n.pc?.close();
      nodes.length = 0;
    });
    cluster.on("mesh", ({ name }) => meshConnect(name));
    cluster.rpc("meshNodes").then((names) => names.forEach(meshConnect), () => {});
    // The control plane is gone (poweroff, reboot, a crash): close the
    // connections, so that the docker nodes end like after the last tab.
    const down = () => {
      for (const { pc } of nodes) pc.close();
      pending = null;
      render();
    };
    cluster.on("exited", down);
    cluster.on("failed", down);
  }

  // A connection that may still come up, or is up.
  const alive = (pc) => pc && !["failed", "closed"].includes(pc.connectionState);
  // drop removes the entries of a node whose connections ended.
  const drop = (name) => {
    for (let i = nodes.length - 1; i >= 0; i--) if (nodes[i].info.name === name && !alive(nodes[i].pc) && nodes[i] !== pending) nodes.splice(i, 1);
  };

  // dial connects this tab to the paired node p through the relay. It
  // resolves true once the node is connected, or when it was already.
  const dial = async (p) => {
    // A disconnected peer may come back, but a restarted node does not:
    // dial it anew. The old peer would fail only after half a minute.
    const cur = nodes.find((n) => n.info.name === p.name && alive(n.pc));
    if (cur && cur.pc.connectionState !== "disconnected") return true;
    cur?.pc.close();
    drop(p.name);
    const pc = new RTCPeerConnection({ iceServers: stun ? [{ urls: stun }] : [] });
    const entry = { pc, info: { name: p.name } };
    nodes.push(entry);
    pc.createDataChannel("ctl");
    pc.ondatachannel = (e) => cluster ? cluster.bridge(e.channel) : globalThis.tabernetes.attachChannel(e.channel);
    const open = channels(pc);
    open.pc = pc;
    watch(pc, p.name);
    render();
    try {
      await pc.setLocalDescription(await pc.createOffer());
      await gatheringComplete(pc, 3000);
      const sdp = await exchange(p, pc.localDescription.sdp);
      await pc.setRemoteDescription({ type: "answer", sdp });
      if (cluster) cluster.dataChannels.set(p.name, open);
      else {
        const err = globalThis.tabernetes.restoreNode(p.name, open);
        if (err) throw err;
      }
      await new Promise((resolve, reject) => {
        const check = () => {
          if (pc.connectionState === "connected") resolve();
          else if (!alive(pc)) reject(new Error(`the connection to ${p.name} failed`));
          else return;
          pc.removeEventListener("connectionstatechange", check);
        };
        pc.addEventListener("connectionstatechange", check);
        check();
      });
      return true;
    } catch (e) {
      pc.close();
      render();
      throw e;
    }
  };

  // keep dials the paired node p until this tab is connected to it, and
  // again whenever the connection ends. A node that does not answer is
  // asked less often, at most once a minute.
  // eager: join --keep waits for the node to start, try often.
  const keep = (p, eager = false) => {
    if (paired.has(p.name)) return;
    const s = { p, delay: 5000, timer: 0, eager };
    paired.set(p.name, s);
    if (!nodes.some((n) => n.info.name === p.name)) nodes.push({ pc: null, info: { name: p.name } });
    render();
    const attempt = async () => {
      if (paired.get(p.name) !== s || cluster?.ended) return;
      let ok = false;
      try { ok = await dial(p); } catch (e) { console.debug(`tabernetes: ${p.name}:`, e.message); }
      if (paired.get(p.name) !== s) return;
      if (ok) s.eager = false;
      s.delay = ok ? 5000 : Math.min(s.delay * 2, 60000);
      s.timer = setTimeout(attempt, ok ? 5000 : s.eager ? 2000 : s.delay);
    };
    attempt();
  };
  const unkeep = (name) => {
    const s = paired.get(name);
    if (!s) return;
    clearTimeout(s.timer);
    paired.delete(name);
    for (const n of nodes) if (n.info.name === name) n.pc?.close();
    for (let i = nodes.length - 1; i >= 0; i--) if (nodes[i].info.name === name) nodes.splice(i, 1);
    render();
  };
  // The paired nodes of the cluster: this tab dials them from the start.
  (async () => {
    let list;
    if (cluster) list = await cluster.rpc("pairedNodes");
    else {
      while (!globalThis.tabernetes?.ready) await new Promise((r) => setTimeout(r, 500));
      await globalThis.tabernetes.ready;
      list = globalThis.tabernetes.pairedNodes();
    }
    list.forEach(keep);
  })().catch((e) => console.debug("tabernetes: paired nodes:", e));

  // finish hands the answer of the node to the pending connection.
  const finish = async (answer, term) => {
    let ans;
    try { ans = await decodeToken(answer.trim().replace(/^accept\s+/, "")); } catch { ans = null; }
    if (typeof ans?.sdp !== "string") {
      term.writeln(`That is not the ${B("accept ...")} line of the node.`);
      return false;
    }
    await pending.pc.setRemoteDescription({ type: "answer", sdp: ans.sdp });
    term.writeln(`Connecting ${pending.info.name}... the status line below the terminal shows the connection.`);
    pending = null;
    render();
    return true;
  };

  // joinPaired is join --keep: the token has no offer but the relay, the
  // topic and the key of the node. The container starts the node at once
  // and waits for offers; this tab sends one until the node answers.
  const joinPaired = async (via, { term, active }, kubectl = false) => {
    let url;
    try { url = new URL(via); } catch { url = null; }
    if (url?.protocol !== "https:" && !/^(localhost|127\.0\.0\.1|\[::1\])$/.test(url?.hostname ?? "")) {
      term.writeln(`The relay must be an https:// URL of an ntfy server.`);
      return;
    }
    const noOpen = () => Promise.reject(new Error("no tab is connected to the node"));
    const href = url.href.replace(/\/+$/, "");
    let info;
    if (kubectl) info = cluster ? await cluster.rpc("prepareKubectl", href) : await globalThis.tabernetes.prepareKubectl(href);
    else if (cluster) info = await cluster.rpc("prepareJoin", href);
    else info = await globalThis.tabernetes.prepareJoin(noOpen, href);
    const p = info.pairing;
    // kubectl-<n> gets the ports above the nodes: 59000 + n
    const port = kubectl ? 59000 + (Number(info.name.replace(/^\D+/, "")) % 1000)
      : 50000 + ((Number(info.name.replace(/^\D+/, "")) || ++seq) % 10000);
    const token = await encodeToken({
      v: 1, n: info.name, ip: info.ip, t: info.token, ca: info.ca, p: port, ru: p.relay, rt: p.topic, k: p.key,
      ...(kubectl ? { kind: "kubectl" } : {}),
    });
    globalThis.tabernetesLastJoin = { token, port, name: info.name, paired: true };
    if (kubectl) return showKubectl(info, p, port, token, { term, active });
    const run = (ports) => `docker run -d --restart unless-stopped ${ports} --name tabernetes-${info.name}`;
    term.writeln(`Start the node on a machine with docker. It stays in the cluster: when this page`);
    term.writeln(`reloads, the tabs find it again through ${p.relay} (end to end encrypted).`);
    term.writeln("");
    term.writeln(B(`${run(`-p ${port}:${port}/udp`)} \\`));
    term.writeln(B(`  ${NODE_FLAGS} \\`));
    term.writeln(B(`  ${image} \\`));
    term.writeln(B(`  ${token}`));
    term.writeln("");
    term.writeln(`On the machine of this browser, publish the port on ${B(`127.0.0.1:${port}:${port}/udp`)} only.`);
    term.writeln(`In your network, add ${B("--advertise <LAN IP of that machine>")} before the token.`);
    term.writeln(`${B(`unjoin ${info.name}`)} removes the node again.`);
    term.writeln("");
    if (navigator.clipboard?.writeText) {
      const cmd = `${run(`-p ${port}:${port}/udp`)} ${NODE_FLAGS} ${image} ${token}`;
      await navigator.clipboard.writeText(cmd).then(() => term.writeln(DIM("(the docker command was copied to your clipboard)")), () => {});
    }
    keep(p, true);
    term.writeln(`Waiting for ${info.name}... Ctrl-C stops waiting, the tab keeps trying.`);
    while (active()) {
      if (nodes.some((n) => n.info.name === info.name && n.pc?.connectionState === "connected")) {
        term.writeln(`${info.name} is connected. ${B("kubectl get nodes")} shows it once its kubelet has registered.`);
        return;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
  };

  const help = (term) => {
    for (const l of [
      `usage: join [--keep | --kubectl] [--relay <ntfy URL>]`,
      "",
      `${B("join")}            connect a docker node for this visit. Copy and paste: the`,
      `                docker run carries the offer, and join asks for the ${B("accept ...")} line`,
      `                the node prints. The node ends with the last tab of this page.`,
      `${B("join --keep")}     a node that stays: it reconnects after a reload and after its`,
      `                container restarts, and nothing is pasted back. The tabs find it`,
      `                through a relay (ntfy), end to end encrypted. A server needs no`,
      `                open port. ${B("unjoin <node>")} removes it.`,
      `${B("join --kubectl")}  run kubectl outside the browser: the same node image forwards`,
      `                127.0.0.1:6443 to the cluster and prints a kubeconfig.`,
      `                ${B("unjoin kubectl-<n>")} takes the access back.`,
      `${B("--relay <URL>")}   the ntfy server for --keep and --kubectl (default ${relay}).`,
      "",
      `Docker on another machine: publish the UDP port on all interfaces (${B("-p <port>:<port>/udp")}).`,
      `A server with a public IP needs nothing else. In your network, add`,
      `${B("--advertise <LAN IP>")} before the token.`,
    ]) term.writeln(l);
  };

  // showKubectl prints the docker run of join --kubectl and waits for it.
  const showKubectl = async (info, p, port, token, { term, active }) => {
    const cmd = `docker run -d --restart unless-stopped -p 127.0.0.1:6443:6443 -p 127.0.0.1:${port}:${port}/udp --name tabernetes-${info.name} ${image} kubectl ${token}`;
    term.writeln(`Run kubectl on this machine against the cluster in this browser. With docker:`);
    term.writeln("");
    term.writeln(B(`docker run -d --restart unless-stopped -p 127.0.0.1:6443:6443 \\`));
    term.writeln(B(`  -p 127.0.0.1:${port}:${port}/udp --name tabernetes-${info.name} \\`));
    term.writeln(B(`  ${image} kubectl \\`));
    term.writeln(B(`  ${token}`));
    term.writeln(B(`docker logs tabernetes-${info.name} 2>/dev/null > tabernetes.kubeconfig`));
    term.writeln(B(`KUBECONFIG=tabernetes.kubeconfig kubectl get nodes`));
    term.writeln("");
    term.writeln(`The container prints the kubeconfig: https://127.0.0.1:6443, the CA and a token`);
    term.writeln(`of the ServiceAccount kube-system/tabernetes-${info.name} (${B("cluster-admin")}, valid for a year).`);
    term.writeln(`Keep it like a password. It reconnects through ${p.relay} after a reload.`);
    term.writeln(`${B(`unjoin ${info.name}`)} takes the access back.`);
    term.writeln("");
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(cmd).then(() => term.writeln(DIM("(the docker command was copied to your clipboard)")), () => {});
    }
    keep(p, true);
    term.writeln(`Waiting for ${info.name}... Ctrl-C stops waiting, the tab keeps trying.`);
    while (active()) {
      if (nodes.some((n) => n.info.name === info.name && n.pc?.connectionState === "connected")) {
        term.writeln(`${info.name} is connected.`);
        return;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
  };

  return {
    async join(args, { term, ask, active }) {
      let keepIt = false, kubectl = false, via = relay;
      for (let i = 0; i < args.length; i++) {
        if (args[i] === "--help" || args[i] === "-h") { help(term); return; }
        if (args[i] === "--keep") keepIt = true;
        else if (args[i] === "--kubectl") kubectl = true;
        else if (args[i] === "--relay" && args[i + 1]) { keepIt = true; via = args[++i]; }
        else if (args[i].startsWith("--relay=")) { keepIt = true; via = args[i].slice(8); }
        else {
          term.writeln(`usage: join [--keep | --kubectl] [--relay <ntfy URL>], or join --help`);
          return;
        }
      }
      if (kubectl) return joinPaired(via, { term, active }, true);
      if (keepIt) return joinPaired(via, { term, active });
      if (pending) {
        // The node never answered (docker run failed, accept line lost): start over.
        term.writeln(DIM(`dropping the pending join for ${pending.info.name}`));
        pending.pc.close();
        nodes.splice(nodes.indexOf(pending), 1);
        pending = null;
        render();
      }
      const pc = new RTCPeerConnection({ iceServers: stun ? [{ urls: stun }] : [] });
      pc.createDataChannel("ctl");
      // With the control plane in the shared worker, the data channels go on there.
      pc.ondatachannel = (e) => cluster ? cluster.bridge(e.channel) : globalThis.tabernetes.attachChannel(e.channel);
      const open = channels(pc);
      open.pc = pc;
      let info;
      if (cluster) {
        info = await cluster.rpc("prepareJoin");
        cluster.dataChannels.set(info.name, open);
      } else info = await globalThis.tabernetes.prepareJoin(open);
      await pc.setLocalDescription(await pc.createOffer());
      await gatheringComplete(pc, 3000);
      if (!active()) { pc.close(); return; } // Ctrl-C while preparing
      // One port per node: the names are unique in the cluster, a counter of
      // this tab is not, and two containers cannot publish the same port.
      const port = 50000 + ((Number(info.name.replace(/^\D+/, "")) || ++seq) % 10000);
      const token = await encodeToken({
        v: 1, n: info.name, ip: info.ip, t: info.token, ca: info.ca, sdp: pc.localDescription.sdp, p: port,
      });
      watch(pc, info.name);
      pending = { pc, info };
      globalThis.tabernetesLastJoin = { token, port, name: info.name };
      nodes.push(pending);
      render();

      term.writeln(`Start a node on a machine with docker (Linux, macOS or Windows):`);
      term.writeln("");
      term.writeln(B(`docker run --rm -it -p 127.0.0.1:${port}:${port}/udp --name tabernetes-${info.name} \\`));
      term.writeln(B(`  ${NODE_FLAGS} \\`));
      term.writeln(B(`  ${image} \\`));
      term.writeln(B(`  ${token}`));
      term.writeln("");
      term.writeln(`Docker on another machine? Publish the port on all interfaces: ${B(`-p ${port}:${port}/udp`)}.`);
      term.writeln(`- A server with a public IP needs nothing else, not even a firewall rule: the node`);
      term.writeln(`  sends to the browser too, so the firewall of the server lets the browser in.`);
      term.writeln(`- In your network, add ${B("--advertise <LAN IP of that machine>")} before the token:`);
      term.writeln(DIM(`  docker run ... -p ${port}:${port}/udp ... ${image} --advertise 192.168.x.y <token>`));
      term.writeln("");
      if (navigator.clipboard?.writeText) {
        const cmd = `docker run --rm -it -p 127.0.0.1:${port}:${port}/udp --name tabernetes-${info.name} ${NODE_FLAGS} ${image} ${token}`;
        await navigator.clipboard.writeText(cmd).then(() => term.writeln(DIM("(the docker command was copied to your clipboard)")), () => {});
      }
      term.writeln(`The node prints an ${B("accept ...")} line. Paste it here, or press Ctrl-C and run ${B("accept ...")} later.`);
      const me = pending;
      for (;;) {
        let answer;
        try { answer = await ask("accept line: "); } catch { return; }
        if (pending !== me) return; // another join replaced this one
        if (answer.trim() && await finish(answer, term)) return;
      }
    },

    async unjoin(args, { term }) {
      if (args[0] === "--help" || args[0] === "-h") {
        term.writeln(`usage: unjoin <name>   remove a node of join --keep, or the access of join --kubectl`);
        return;
      }
      const name = args[0];
      if (!name) { term.writeln(`usage: unjoin <node>`); return; }
      try {
        if (cluster) await cluster.rpc("unjoin", name);
        else await globalThis.tabernetes.unjoin(name);
      } catch (e) {
        term.writeln(e.message);
        return;
      }
      unkeep(name);
      if (name.startsWith("kubectl-")) {
        term.writeln(`The token of ${name} is void. Remove its container: ${B(`docker rm -f tabernetes-${name}`)}`);
        return;
      }
      term.writeln(`${name} is gone from the cluster. Its container still waits for the browser: ${B(`docker rm -f tabernetes-${name}`)}`);
    },

    async accept(args, { term }) {
      if (!pending) { term.writeln(`Nothing to accept. Run ${B("join")} first.`); return; }
      if (!args[0]) { term.writeln(`usage: accept <answer>`); return; }
      await finish(args[0], term);
    },
  };
}
