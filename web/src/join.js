// "join" and "accept": connect a real kubelet to the in-browser control plane
// using WebRTC data channels. Signaling is copy & paste, no server involved.

const B = (s) => `\x1b[1m${s}\x1b[0m`;
const DIM = (s) => `\x1b[2m${s}\x1b[0m`;

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

export function joinBuiltins({ image, stun = "stun:stun.l.google.com:19302" }) {
  const nodes = [];
  let pending = null;

  return {
    async join(args, { term }) {
      if (pending) {
        term.writeln(`There is already a pending join for ${pending.info.name}. Run ${B("accept <answer>")} first.`);
        return;
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
      const port = 50000 + nodes.length + 1;
      const token = await encodeToken({
        v: 1, n: info.name, ip: info.ip, t: info.token, ca: info.ca, sdp: pc.localDescription.sdp, p: port,
      });
      pc.onconnectionstatechange = () => {
        const s = pc.connectionState;
        term.writeln(`\r\n${DIM(`[webrtc]`)} ${info.name}: ${s}`);
      };
      pending = { pc, info };
      globalThis.kwasmLastJoin = { token, port, name: info.name };
      nodes.push(pending);

      term.writeln(`Start a node on a machine with docker (Linux, macOS or Windows):`);
      term.writeln("");
      term.writeln(B(`docker run --rm -it --privileged -p 127.0.0.1:${port}:${port}/udp --name kwasm-${info.name} \\`));
      term.writeln(B(`  ${image} \\`));
      term.writeln(B(`  ${token}`));
      term.writeln("");
      term.writeln(`It prints an ${B("accept ...")} line. Paste it here.`);
      if (navigator.clipboard?.writeText) {
        const cmd = `docker run --rm -it --privileged -p 127.0.0.1:${port}:${port}/udp --name kwasm-${info.name} ${image} ${token}`;
        navigator.clipboard.writeText(cmd).then(() => term.writeln(DIM("(the docker command was copied to your clipboard)")), () => {});
      }
    },

    async accept(args, { term }) {
      if (!pending) { term.writeln(`Nothing to accept. Run ${B("join")} first.`); return; }
      if (!args[0]) { term.writeln(`usage: accept <answer>`); return; }
      let ans;
      try { ans = await decodeToken(args[0]); } catch (e) { term.writeln(`invalid answer: ${e.message}`); return; }
      await pending.pc.setRemoteDescription({ type: "answer", sdp: ans.sdp });
      term.writeln(`Connecting ${pending.info.name}... watch it with ${B("kubectl get nodes -w")}`);
      pending = null;
    },
  };
}
