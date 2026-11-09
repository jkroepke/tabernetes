// The worker of a webllm pod (gpu.js): WebLLM runs one model here. The
// weights come from Hugging Face, WebLLM keeps them in the Cache Storage.
import { MLCEngine } from "@mlc-ai/web-llm";

let engine;
const post = (m) => self.postMessage(m);

self.onmessage = async ({ data: m }) => {
  if (m.type === "interrupt") {
    engine?.interruptGenerate();
    return;
  }
  try {
    if (m.type === "load") {
      engine = new MLCEngine({ initProgressCallback: (r) => post({ type: "progress", text: r.text, progress: r.progress }) });
      await engine.reload(m.model);
      post({ type: "loaded" });
    } else if (m.type === "chat") {
      const req = JSON.parse(m.request);
      if (req.stream) {
        for await (const chunk of await engine.chat.completions.create(req)) {
          post({ type: "chunk", id: m.id, data: JSON.stringify(chunk) });
        }
        post({ type: "done", id: m.id, data: "" });
      } else {
        post({ type: "done", id: m.id, data: JSON.stringify(await engine.chat.completions.create(req)) });
      }
    }
  } catch (e) {
    post({ type: "error", id: m.id, message: String(e?.message || e) });
  }
};
