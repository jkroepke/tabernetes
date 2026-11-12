// relay: the signaling of a paired docker node (join --keep). The tab posts
// its WebRTC offer to the topic of the node on an ntfy server, sealed with
// the key of the node (AES-GCM), and waits for the answer on a reply topic of
// its own. Tab and node need only outgoing HTTPS, so the node needs no open
// port and survives a reload. node/agent/relay.go is the other side.

export const DEFAULT_RELAY = "https://ntfy.sh";

const b64url = (u8) => btoa(String.fromCharCode(...u8)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const unb64url = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
const stream = (bytes, t) => new Response(new Blob([bytes]).stream().pipeThrough(t)).arrayBuffer();

const keys = new Map();
function importKey(key) {
  if (!keys.has(key)) keys.set(key, crypto.subtle.importKey("raw", unb64url(key), "AES-GCM", false, ["encrypt", "decrypt"]));
  return keys.get(key);
}

// seal deflates and encrypts obj: base64url(nonce | ciphertext), like seal
// in relay.go.
export async function seal(key, obj) {
  const plain = await stream(new TextEncoder().encode(JSON.stringify(obj)), new CompressionStream("deflate-raw"));
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv }, await importKey(key), plain));
  const out = new Uint8Array(iv.length + ct.length);
  out.set(iv);
  out.set(ct, iv.length);
  return b64url(out);
}

export async function unseal(key, s) {
  const b = unb64url(s.trim());
  const plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv: b.subarray(0, 12) }, await importKey(key), b.subarray(12));
  return JSON.parse(new TextDecoder().decode(await stream(plain, new DecompressionStream("deflate-raw"))));
}

// exchange sends the offer sdp to the paired node p ({relay, topic, key})
// and returns its answer. It subscribes to the reply topic before it posts
// the offer, so the answer cannot come too early.
export async function exchange(p, sdp, timeout = 20000) {
  const relay = p.relay.replace(/\/+$/, "");
  const reply = "tabernetes-" + b64url(crypto.getRandomValues(new Uint8Array(18)));
  const es = new EventSource(`${relay}/${reply}/sse`);
  let timer;
  try {
    const answer = new Promise((resolve, reject) => {
      timer = setTimeout(() => reject(new Error(`${p.name} did not answer through ${relay}`)), timeout);
      es.onmessage = async (e) => {
        try {
          const m = JSON.parse(e.data);
          if (m.event && m.event !== "message") return;
          const a = await unseal(p.key, m.message);
          if (typeof a.sdp === "string") resolve(a.sdp);
        } catch { /* not for us */ }
      };
    });
    answer.catch(() => {});
    const opened = new Promise((resolve, reject) => {
      es.onopen = resolve;
      // EventSource retries on its own; an error before the first open is
      // a relay that cannot be reached (offline, blocked by the CSP).
      es.onerror = () => reject(new Error(`cannot reach the relay ${relay}`));
    });
    await Promise.race([opened, answer]); // answer: only its timeout
    const r = await fetch(`${relay}/${p.topic}`, { method: "POST", body: await seal(p.key, { sdp, reply, t: Date.now() }) });
    if (!r.ok) throw new Error(`relay ${relay}: ${r.status} ${r.statusText}`);
    return await answer;
  } finally {
    clearTimeout(timer);
    es.close();
  }
}
