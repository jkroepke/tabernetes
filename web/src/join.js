// "join" and "accept": connect a real kubelet to the in-browser control plane
// using WebRTC data channels. Signaling is copy & paste, no server involved.

const B = (s) => `\x1b[1m${s}\x1b[0m`;
const DIM = (s) => `\x1b[2m${s}\x1b[0m`;

// Instead of --privileged. See kwasm/README.md for why each flag is needed.
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
export function joinBuiltins({ image, stun = "stun:stun.l.google.com:19302", status }) {
  const nodes = [];
  let pending = null;
  let seq = 0;

  const render = () => {
    if (!status) return;
    if (!nodes.length) {
      status.textContent = "No nodes. Type join to connect one.";
      return;
    }
    status.replaceChildren(...nodes.map(({ info, pc }) => {
      const [text, color] = STATES[pc.connectionState] || [pc.connectionState, "#8b949e"];
      const el = document.createElement("span");
      el.className = "kwasm-node";
      const dot = document.createElement("span");
      dot.textContent = "\u25CF ";
      dot.style.color = color;
      el.append(dot, `${info.name} ${text}`);
      return el;
    }));
  };
  render();

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

  return {
    async join(args, { term, ask, active }) {
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
      pc.ondatachannel = (e) => globalThis.kwasm.attachChannel(e.channel);
      const open = (label) => new Promise((resolve, reject) => {
        const dc = pc.createDataChannel(label);
        const t = setTimeout(() => reject(new Error("data channel timeout")), 15000);
        dc.onopen = () => { clearTimeout(t); resolve(dc); };
        dc.onerror = (e) => { clearTimeout(t); reject(e.error || new Error("data channel error")); };
      });
      const info = await globalThis.kwasm.prepareJoin(open);
      await pc.setLocalDescription(await pc.createOffer());
      await gatheringComplete(pc, 3000);
      if (!active()) { pc.close(); return; } // Ctrl-C while preparing
      const port = 50000 + ++seq;
      const token = await encodeToken({
        v: 1, n: info.name, ip: info.ip, t: info.token, ca: info.ca, sdp: pc.localDescription.sdp, p: port,
      });
      pc.onconnectionstatechange = () => {
        render();
        // A failed connection never recovers: free it and its data channels.
        if (pc.connectionState === "failed") pc.close();
      };
      pending = { pc, info };
      globalThis.kwasmLastJoin = { token, port, name: info.name };
      nodes.push(pending);
      render();

      term.writeln(`Start a node on a machine with docker (Linux, macOS or Windows):`);
      term.writeln("");
      term.writeln(B(`docker run --rm -it -p 127.0.0.1:${port}:${port}/udp --name kwasm-${info.name} \\`));
      term.writeln(B(`  ${NODE_FLAGS} \\`));
      term.writeln(B(`  ${image} \\`));
      term.writeln(B(`  ${token}`));
      term.writeln("");
      term.writeln(`Docker on another machine in your network? Publish the port on all interfaces and`);
      term.writeln(`add ${B("--advertise <LAN IP of that machine>")} before the token:`);
      term.writeln(DIM(`  docker run ... -p ${port}:${port}/udp ... ${image} --advertise 192.168.x.y <token>`));
      term.writeln("");
      if (navigator.clipboard?.writeText) {
        const cmd = `docker run --rm -it -p 127.0.0.1:${port}:${port}/udp --name kwasm-${info.name} ${NODE_FLAGS} ${image} ${token}`;
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

    async accept(args, { term }) {
      if (!pending) { term.writeln(`Nothing to accept. Run ${B("join")} first.`); return; }
      if (!args[0]) { term.writeln(`usage: accept <answer>`); return; }
      await finish(args[0], term);
    },
  };
}
