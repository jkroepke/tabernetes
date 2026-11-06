// Browser terminal for kwasm: boots the control plane and runs kubectl.
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { decompress } from "fzstd";
import { installFS } from "./fs.js";

const PROMPT = "\x1b[1;34mkwasm\x1b[0m:\x1b[1;32m~\x1b[0m$ ";
const DIM = (s) => `\x1b[2m${s}\x1b[0m`;

// Minimal POSIX-ish word splitting with single/double quotes and backslashes.
export function splitWords(line) {
  const out = [];
  let cur = "", quote = null, has = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (quote) {
      if (c === quote) quote = null;
      else if (c === "\\" && quote === '"' && i + 1 < line.length) cur += line[++i];
      else cur += c;
    } else if (c === "'" || c === '"') { quote = c; has = true; }
    else if (c === "\\" && i + 1 < line.length) { cur += line[++i]; has = true; }
    else if (/\s/.test(c)) { if (cur || has) out.push(cur); cur = ""; has = false; }
    else { cur += c; has = true; }
  }
  if (quote) throw new Error("unterminated quote");
  if (cur || has) out.push(cur);
  return out;
}

export async function startTerminal(el, opts) {
  const term = new Terminal({
    convertEol: true,
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
    fontSize: 14,
    theme: { background: "#0d1117", foreground: "#c9d1d9" },
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();
  new ResizeObserver(() => fit.fit()).observe(el);

  const logs = [];
  globalThis.kwasmLogs = logs;
  let crashing = false;
  installFS((fd, s) => {
    if (crashing || /^(panic:|fatal error:)/m.test(s)) { crashing = true; console.error(s); }
    logs.push(s);
    if (logs.length > 5000) logs.splice(0, logs.length - 5000);
  });

  term.writeln("\x1b[1mkwasm\x1b[0m: a real Kubernetes control plane, compiled to WebAssembly, running in your browser.");
  term.writeln(DIM("kube-apiserver, kube-controller-manager, kube-scheduler and kubectl are the unmodified upstream"));
  term.writeln(DIM("components. etcd is replaced by a tiny in-memory implementation. Nothing leaves your browser."));
  term.writeln("");

  // ---- download + boot ----
  const resp = await fetch(opts.wasmURL);
  if (!resp.ok) throw new Error(`download failed: ${resp.status}`);
  const total = Number(resp.headers.get("content-length")) || opts.wasmSize || 0;
  let loaded = 0;
  const bar = (n, t) => {
    const w = 30, f = t ? Math.min(w, Math.round((n / t) * w)) : 0;
    return `[${"#".repeat(f)}${" ".repeat(w - f)}] ${(n / 1048576).toFixed(1)}${t ? "/" + (t / 1048576).toFixed(1) : ""} MB`;
  };
  const counted = resp.body.pipeThrough(new TransformStream({
    transform(chunk, ctl) {
      loaded += chunk.length;
      term.write(`\r${DIM("downloading")} ${bar(loaded, total)}`);
      ctl.enqueue(chunk);
    },
  }));
  const bytes = await unpack(opts.wasmURL, counted);
  term.writeln("");

  const t0 = performance.now();
  const since = () => ((performance.now() - t0) / 1000).toFixed(1).padStart(5);
  globalThis.kwasmStatus = (s) => term.writeln(`${DIM(`[${since()}s]`)} ${s}`);
  globalThis.kwasmStatus("compiling WebAssembly");
  const go = new globalThis.Go();
  go.env = { HOME: "/root", USER: "root", PATH: "/usr/bin" };
  const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
  go.run(instance).then(() => term.writeln("\r\n\x1b[31mthe control plane exited\x1b[0m"));
  await globalThis.kwasm.ready;
  term.writeln("");
  term.writeln(`Try ${"\x1b[1m"}kubectl get nodes${"\x1b[0m"}, ${"\x1b[1m"}kubectl get pods -A${"\x1b[0m"} or ${"\x1b[1m"}help${"\x1b[0m"}.`);

  // ---- shell ----
  const history = [];
  let hpos = 0, line = "", cursor = 0, busy = null, heredoc = null;

  const builtins = {
    help() {
      term.writeln("Commands:");
      term.writeln("  kubectl ... (alias: k)   run kubectl against the in-browser cluster");
      term.writeln("  kubectl apply -f - <<EOF heredocs work, finish with a line containing EOF");
      term.writeln("  join                     connect a real node via WebRTC (needs docker)");
      term.writeln("  logs                     show control plane logs");
      term.writeln("  clear                    clear the screen");
      term.writeln("Keys: Ctrl-C cancels a running command, Ctrl-L clears, ↑/↓ history.");
    },
    clear() { term.clear(); },
    logs(args) {
      const n = Number(args[0]) || 40;
      term.write(logs.join("").split("\n").slice(-n - 1).join("\n"));
    },
    ...(opts.builtins || {}),
  };

  async function exec(input, stdin = "") {
    let words;
    try { words = splitWords(input); } catch (e) { term.writeln(`kwasm: ${e.message}`); return; }
    if (!words.length) return;
    const [cmd, ...args] = words;
    if (cmd === "kubectl" || cmd === "k") {
      const p = globalThis.kwasm.kubectl(args, (s) => term.write(s), stdin);
      busy = p;
      await Promise.race([p, new Promise((r) => (busy.cancel = r))]);
      busy = null;
    } else if (builtins[cmd]) {
      busy = Promise.resolve(builtins[cmd](args, { term, stdin, exec }));
      await busy;
      busy = null;
    } else {
      term.writeln(`kwasm: ${cmd}: command not found (try "help")`);
    }
  }

  const redraw = () => {
    term.write("\r\x1b[K" + (heredoc ? "> " : PROMPT) + line);
    const back = line.length - cursor;
    if (back > 0) term.write(`\x1b[${back}D`);
  };

  async function submit() {
    const input = line;
    line = ""; cursor = 0;
    term.write("\r\n");
    if (heredoc) {
      if (input.trim() === heredoc.marker) {
        const { cmd, body } = heredoc;
        heredoc = null;
        await exec(cmd, body);
      } else {
        heredoc.body += input + "\n";
        term.write("> ");
        return;
      }
    } else {
      if (input.trim()) { history.push(input); }
      hpos = history.length;
      const m = input.match(/^(.*?)\s*<<-?\s*['"]?(\w+)['"]?\s*$/);
      if (m) {
        heredoc = { cmd: m[1], marker: m[2], body: "" };
        term.write("> ");
        return;
      }
      await exec(input);
    }
    term.write(PROMPT);
  }

  term.write(PROMPT);
  term.onData((data) => {
    if (busy) {
      if (data === "\x03") {
        globalThis.kwasm.cancel();
        busy.cancel?.();
        term.write("^C\r\n");
      }
      return;
    }
    for (let i = 0; i < data.length; i++) {
      const c = data[i];
      if (c === "\r") { submit(); return; }
      if (c === "\x7f") { if (cursor > 0) { line = line.slice(0, cursor - 1) + line.slice(cursor); cursor--; redraw(); } continue; }
      if (c === "\x03") { term.write("^C\r\n"); line = ""; cursor = 0; heredoc = null; redraw(); continue; }
      if (c === "\x0c") { term.clear(); redraw(); continue; }
      if (c === "\x15") { line = line.slice(cursor); cursor = 0; redraw(); continue; }
      if (c === "\x1b") {
        const seq = data.slice(i, i + 3);
        i += 2;
        if (seq === "\x1b[A" && hpos > 0) { line = history[--hpos]; cursor = line.length; redraw(); }
        else if (seq === "\x1b[B") { hpos = Math.min(history.length, hpos + 1); line = history[hpos] || ""; cursor = line.length; redraw(); }
        else if (seq === "\x1b[D" && cursor > 0) { cursor--; term.write(seq); }
        else if (seq === "\x1b[C" && cursor < line.length) { cursor++; term.write(seq); }
        continue;
      }
      if (c >= " ") { line = line.slice(0, cursor) + c + line.slice(cursor); cursor++; redraw(); }
    }
  });
  term.focus();
  return { term, exec };
}

// GitHub Pages sends no Content-Encoding for the binary, so the page unpacks
// it. DecompressionStream has no zstd (Chromium 153), so fzstd decodes the
// 290 MB in JavaScript. That takes about 1.5 seconds.
async function unpack(url, stream) {
  if (url.endsWith(".zst")) return decompress(new Uint8Array(await new Response(stream).arrayBuffer()));
  if (url.endsWith(".gz")) stream = stream.pipeThrough(new DecompressionStream("gzip"));
  return new Response(stream).arrayBuffer();
}
