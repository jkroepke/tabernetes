import "./wasm_exec.js";
import { startTerminal } from "./terminal.js";
import { joinBuiltins } from "./join.js";

const el = document.getElementById("kwasm");
startTerminal(el, {
  wasmURL: el.dataset.wasm,
  wasmSize: Number(el.dataset.size) || 0,
  builtins: joinBuiltins({ image: el.dataset.image || KWASM_NODE_IMAGE, stun: el.dataset.stun ?? undefined }),
}).then((t) => (globalThis.kwasmTerminal = t)).catch((e) => {
  el.textContent = "kwasm failed to start: " + e;
  console.error(e);
});
