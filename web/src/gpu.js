// The GPU side of the tab node (k8s/tabernetes/tabnode.go, webllm.go), as
// globalThis.tabernetesGPU. Every webllm pod gets its own worker with WebLLM
// in it (gpuworker.js). Stopping the pod ends the worker, which frees the GPU
// memory for sure.

export function gpuAPI(workerURL) {
  return {
    // adapter resolves to null without WebGPU.
    async adapter() {
      if (!navigator.gpu) return null;
      const a = await navigator.gpu.requestAdapter().catch(() => null);
      if (!a) return null;
      const i = a.info || {};
      return {
        description: i.description || "",
        vendor: i.vendor || "",
        architecture: i.architecture || "",
        f16: a.features.has("shader-f16"),
      };
    },

    // start loads model. onProgress gets a line about once a second.
    start(model, onProgress) {
      const worker = new Worker(workerURL);
      const pending = new Map();
      let next = 1;
      let loaded, failed;
      const ready = new Promise((resolve, reject) => { loaded = resolve; failed = reject; });
      ready.catch(() => {});
      const fail = (err) => {
        failed(err);
        for (const p of pending.values()) p.reject(err);
        pending.clear();
      };
      let last = 0;
      worker.onmessage = ({ data: m }) => {
        switch (m.type) {
          case "progress": {
            const now = Date.now();
            if (m.progress === 1 || now - last >= 1000) { last = now; onProgress(m.text); }
            break;
          }
          case "loaded": loaded(); break;
          case "chunk": pending.get(m.id)?.onChunk(m.data); break;
          case "done": { const p = pending.get(m.id); pending.delete(m.id); p?.resolve(m.data); break; }
          case "error": {
            const err = new Error(m.message);
            if (!m.id) { fail(err); break; }
            const p = pending.get(m.id);
            pending.delete(m.id);
            p?.reject(err);
          }
        }
      };
      worker.onerror = (e) => { e.preventDefault(); fail(new Error(e.message || "the GPU worker failed")); };
      worker.postMessage({ type: "load", model });
      return {
        ready,
        // chat sends an OpenAI chat request (JSON). With "stream": true,
        // onChunk gets the chunks and it resolves to "", else to the answer.
        chat(request, onChunk) {
          const id = next++;
          return new Promise((resolve, reject) => {
            pending.set(id, { resolve, reject, onChunk });
            worker.postMessage({ type: "chat", id, request });
          });
        },
        interrupt() { worker.postMessage({ type: "interrupt" }); },
        stop() { worker.terminate(); fail(new Error("stopped")); },
      };
    },
  };
}
