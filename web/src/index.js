import "./wasm_exec.js";
import { startTerminal } from "./terminal.js";
import { joinBuiltins } from "./join.js";
import { proxyBuiltins } from "./proxy.js";
import { vncBuiltins } from "./vnc.js";
import { connectCluster } from "./tabs.js";

const el = document.getElementById("tabernetes");
const abs = (u) => u && new URL(u, location.href).href;
// What the node of this tab runs. llm: language models with WebGPU
// (k8s/tabernetes/webllm.go). Every other image runs as a container in v86,
// an x86 emulator (k8s/tabernetes/v86.go), with the image from a mirror of
// Docker Hub. The workers of the pods run in this tab, also when the
// kubelet runs in the shared worker (bridge.js).
const d = el.dataset;
const pods = {
  gpu: abs(d.gpuWorker),
  vm: d.vmWorker ? {
    version: V86_VERSION,
    workerURL: abs(d.vmWorker),
    wasmURL: abs(d.vmWasm),
    kernelURL: abs(d.vmKernel),
    initURL: abs(d.vmInit),
    registry: d.registry,
  } : undefined,
};
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
  pods,
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
