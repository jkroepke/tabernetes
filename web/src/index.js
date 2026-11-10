import "./wasm_exec.js";
import { startTerminal } from "./terminal.js";
import { joinBuiltins } from "./join.js";
import { proxyBuiltins } from "./proxy.js";
import { vncBuiltins } from "./vnc.js";
import { gpuAPI } from "./gpu.js";
import { vmAPI } from "./vm.js";
import { connectCluster } from "./tabs.js";

const el = document.getElementById("tabernetes");
// llm: the tab node runs language models with WebGPU (k8s/tabernetes/webllm.go).
if (el.dataset.gpuWorker) globalThis.tabernetesGPU = gpuAPI(el.dataset.gpuWorker);
// Every other image runs as a container in v86, an x86 emulator
// (k8s/tabernetes/v86.go), with the image from a mirror of Docker Hub.
if (el.dataset.vmWorker) {
  const d = el.dataset;
  globalThis.tabernetesVM = vmAPI({
    version: V86_VERSION,
    workerURL: d.vmWorker,
    wasmURL: d.vmWasm,
    biosURL: d.vmBios,
    kernelURL: d.vmKernel,
    initURL: d.vmInit,
    registry: d.registry,
  });
}
// The status line under the terminal: the nodes, then the forwarded ports.
const status = document.getElementById("tabernetes-status");
const nodes = document.createElement("span");
const ports = document.createElement("span");
status?.append(nodes, ports);
// The control plane runs in a shared worker, so that every tab of the page
// is a node of the same cluster and any of them may close.
const cluster = el.dataset.cluster
  ? await connectCluster(el.dataset.cluster).catch((e) => { console.warn("tabernetes:", e); return null; })
  : null;
startTerminal(el, {
  cluster,
  wasmURL: el.dataset.wasm,
  workerURL: el.dataset.worker,
  wasmSize: Number(el.dataset.size) || 0,
  builtins: {
    ...joinBuiltins({
      image: el.dataset.image || TABERNETES_IMAGE,
      stun: el.dataset.stun ?? undefined,
      status: status && nodes,
      cluster,
    }),
    ...proxyBuiltins({ status: status && ports }),
    ...vncBuiltins({ box: el.parentElement }),
  },
}).then((t) => (globalThis.tabernetesTerminal = t)).catch((e) => {
  el.textContent = "Tabernetes failed to start: " + e;
  console.error(e);
});
