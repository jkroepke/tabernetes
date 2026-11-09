import "./wasm_exec.js";
import { startTerminal } from "./terminal.js";
import { joinBuiltins } from "./join.js";
import { proxyBuiltins } from "./proxy.js";
import { vncBuiltins } from "./vnc.js";
import { gpuAPI } from "./gpu.js";

const el = document.getElementById("tabernetes");
// llm: the tab node runs language models with WebGPU (k8s/tabernetes/webllm.go).
if (el.dataset.gpuWorker) globalThis.tabernetesGPU = gpuAPI(el.dataset.gpuWorker);
// The status line under the terminal: the nodes, then the forwarded ports.
const status = document.getElementById("tabernetes-status");
const nodes = document.createElement("span");
const ports = document.createElement("span");
status?.append(nodes, ports);
startTerminal(el, {
  wasmURL: el.dataset.wasm,
  workerURL: el.dataset.worker,
  wasmSize: Number(el.dataset.size) || 0,
  builtins: {
    ...joinBuiltins({
      image: el.dataset.image || TABERNETES_IMAGE,
      stun: el.dataset.stun ?? undefined,
      status: status && nodes,
    }),
    ...proxyBuiltins({ status: status && ports }),
    ...vncBuiltins({ box: el.parentElement }),
  },
}).then((t) => (globalThis.tabernetesTerminal = t)).catch((e) => {
  el.textContent = "Tabernetes failed to start: " + e;
  console.error(e);
});
