// The tab side of the shared control plane (cluster.js, k8s/tabernetes/tabs.go).
// connectCluster returns null where the browser has no SharedWorker or no Web
// Locks (Chrome on Android): the tab then runs the control plane itself.

export async function connectCluster(url, tries = 0) {
  if (typeof SharedWorker !== "function" || !navigator.locks?.request) return null;
  const id = crypto.randomUUID();
  const lock = `tabernetes-tab-${id}`;
  // Held until the tab closes. The worker waits for it to learn that.
  await new Promise((held) => navigator.locks.request(lock, () => { held(); return new Promise(() => {}); }));

  const worker = new SharedWorker(url, { name: "tabernetes" });
  const port = worker.port;
  const calls = new Map();
  const listeners = { status: [], failed: [], exited: [], mesh: [] };
  const dataChannels = new Map(); // docker node name -> open(label) of join.js
  let seq = 0;
  let hello;
  let ended = null; // the message of the control plane that failed or exited
  const gone = () => new Error(ended.type === "failed" ? ended.error : `the control plane exited${ended.why ? `: ${ended.why}` : ""}`);
  const greeted = new Promise((resolve, reject) => {
    hello = resolve;
    worker.onerror = (e) => reject(new Error(`the shared worker failed: ${e.message || "no details"}`));
    setTimeout(() => reject(new Error("the shared worker does not answer")), 10000);
  });

  port.onmessage = ({ data }) => {
    switch (data.type) {
      case "hello": hello(data); break;
      case "status": case "failed": case "exited": case "mesh":
        if (data.type === "failed" || data.type === "exited") {
          // The worker closes in a moment: nothing answers a call anymore.
          ended = data;
          for (const call of calls.values()) call.reject(gone());
          calls.clear();
        }
        listeners[data.type].forEach((fn) => fn(data));
        break;
      case "rpc": {
        const call = calls.get(data.seq);
        if (!call) break;
        calls.delete(data.seq);
        data.error ? call.reject(new Error(data.error)) : call.resolve(data.result);
        break;
      }
      case "connect":
        if (data.dc !== undefined) {
          // The kube-apiserver to the kubelet of a docker node: a data channel.
          const openDC = dataChannels.get(data.dc);
          if (!openDC) { data.port.postMessage(null); break; }
          openDC(data.label).then((dc) => pipe(dc, data.port), () => data.port.postMessage(null));
        } else {
          // The kube-apiserver to the kubelet of this tab.
          globalThis.tabernetes.attachPort(data.port, data.label);
        }
        break;
    }
  };
  port.start();
  port.postMessage({ type: "hello", id, lock });
  const h = await greeted;
  // A control plane that failed or exited: its worker closes in a moment
  // (cluster.js), and the next one boots a new cluster.
  if ((h.state === "failed" || h.state === "exited") && tries < 3) {
    port.close();
    await new Promise((r) => setTimeout(r, 1500));
    return connectCluster(url, tries + 1);
  }

  return {
    // needBinary: this tab is the first one and hands the download over.
    needBinary: h.needBinary,
    status: h.status,
    state: h.state,
    failure: h.failure,
    on(type, fn) { listeners[type].push(fn); },
    // sendBinary hands the download to the worker as it arrives, in one
    // message per chunk: not every browser can transfer a ReadableStream
    // (Safari).
    async sendBinary(stream, size) {
      port.postMessage({ type: "binary", size });
      try {
        for (const r = stream.getReader(); ;) {
          const { value, done } = await r.read();
          if (done) break;
          // a copy: the other branch of the tee reads the same chunk
          const b = value.slice().buffer;
          port.postMessage({ type: "chunk", data: b }, [b]);
        }
        port.postMessage({ type: "chunk", done: true });
      } catch (e) {
        port.postMessage({ type: "chunk", error: String(e?.message || e) });
      }
    },
    // ended: the last lines of the logs of a control plane that is gone.
    get ended() { return ended; },
    rpc(method, ...args) {
      return new Promise((resolve, reject) => {
        if (ended) { reject(gone()); return; }
        calls.set(++seq, { resolve, reject });
        port.postMessage({ type: "rpc", seq, method, args });
      });
    },
    // open opens a connection from this tab to the control plane.
    open(label) {
      const ch = new MessageChannel();
      port.postMessage({ type: "connect", label, port: ch.port2 }, [ch.port2]);
      return Promise.resolve(ch.port1);
    },
    // bridge hands a data channel that a docker node opened to the control plane.
    bridge(dc) {
      const ch = new MessageChannel();
      port.postMessage({ type: "connect", label: dc.label, port: ch.port2 }, [ch.port2]);
      pipe(dc, ch.port1);
    },
    dataChannels,
  };
}

// pipe connects a data channel and a message port: ArrayBuffers, null ends
// the connection (k8s/tabernetes/tunnel.go, portConn).
function pipe(dc, port) {
  dc.binaryType = "arraybuffer";
  const queue = [];
  let closing = false;
  const flush = () => {
    while (queue.length && dc.readyState === "open" && dc.bufferedAmount < 4 << 20) dc.send(queue.shift());
    if (closing && !queue.length) dc.close();
  };
  dc.bufferedAmountLowThreshold = 1 << 20;
  dc.onbufferedamountlow = flush;
  port.onmessage = ({ data }) => {
    if (data == null) { closing = true; port.close(); } else queue.push(data);
    flush();
  };
  dc.onmessage = ({ data }) => {
    const b = typeof data === "string" ? new TextEncoder().encode(data).buffer : data;
    port.postMessage(b, [b]);
  };
  dc.onclose = () => { port.postMessage(null); port.close(); };
  if (dc.readyState === "closed") dc.onclose();
}
