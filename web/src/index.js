import "./wasm_exec.js";
import { startTerminal } from "./terminal.js";
import { joinBuiltins } from "./join.js";
import { proxyBuiltins } from "./proxy.js";

const el = document.getElementById("kwasm");
// The status line under the terminal: the nodes, then the forwarded ports.
const status = document.getElementById("kwasm-status");
const nodes = document.createElement("span");
const ports = document.createElement("span");
status?.append(nodes, ports);
startTerminal(el, {
  wasmURL: el.dataset.wasm,
  workerURL: el.dataset.worker,
  wasmSize: Number(el.dataset.size) || 0,
  builtins: {
    ...joinBuiltins({
      image: el.dataset.image || KWASM_NODE_IMAGE,
      stun: el.dataset.stun ?? undefined,
      status: status && nodes,
    }),
    ...proxyBuiltins({ status: status && ports }),
  },
}).then((t) => (globalThis.kwasmTerminal = t)).catch((e) => {
  el.textContent = "kwasm failed to start: " + e;
  console.error(e);
});
