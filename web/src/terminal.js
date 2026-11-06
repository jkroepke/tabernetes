// Browser terminal for kwasm: boots the control plane and runs a shell.
// The shell (mvdan.cc/sh) and kubectl live in the wasm binary. This file
// only does the line editing and the builtins that need the browser.
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { decompress } from "fzstd";
import { installFS } from "./fs.js";

const DIM = (s) => `\x1b[2m${s}\x1b[0m`;

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
  term.writeln(`Try ${"\x1b[1m"}kubectl get nodes${"\x1b[0m"}, ${"\x1b[1m"}kubectl get pods -A | grep dns${"\x1b[0m"} or ${"\x1b[1m"}help${"\x1b[0m"}.`);

  // ---- shell ----
  const history = [];
  let hpos = 0, line = "", cursor = 0, busy = false, pending = "";

  const builtins = {
    help(args, { write }) {
      // Wrap a list of names to the terminal width, indented.
      const list = (names) => {
        const out = [];
        let row = " ";
        for (const n of names) {
          if (row.length + n.length + 1 > Math.max(40, term.cols - 2)) { out.push(row); row = " "; }
          row += " " + n;
        }
        return [...out, row];
      };
      const { tools, builtins: shell } = globalThis.kwasm.commands();
      const terminal = Object.keys(builtins).filter((n) => n !== "help").sort();
      write([
        "A POSIX shell (mvdan.cc/sh) inside the wasm binary: pipes, variables, loops,",
        "functions, $(...), redirects and here-documents work. All commands:",
        "",
        "Kubernetes",
        "  kubectl, k               kubectl against the in-browser cluster",
        "",
        "Terminal",
        "  join                     connect a real node via WebRTC (needs docker)",
        "  accept <answer>          finish a join with the answer of the node",
        "  logs [n]                 the last n lines of the control plane logs",
        "  clear                    clear the screen",
        ...terminal.filter((n) => !["join", "accept", "logs", "clear"].includes(n)).map((n) => `  ${n}`),
        "",
        "Tools (small versions, the common flags)",
        ...list(tools),
        "",
        "Shell builtins (help <name> for details)",
        ...list(shell),
        "",
        "Example: kubectl get pods -A -o name | grep dns | xargs kubectl describe",
        "Keys: Ctrl-C cancels a running command, Ctrl-L clears, ↑/↓ history.",
        "",
      ].join("\n"));
    },
    clear() { term.clear(); },
    logs(args, { write }) {
      const n = Number(args[0]) || 40;
      write(logs.join("").split("\n").slice(-n - 1).join("\n"));
    },
    ...(opts.builtins || {}),
  };
  // Builtins get (args, {write, stdin}) from the shell, plus the terminal.
  const shellBuiltins = Object.fromEntries(Object.entries(builtins).map(
    ([name, fn]) => [name, (args, ctx) => fn(args, { term, exec, write: ctx.write, stdin: ctx.stdin })]));

  // exec runs a command line in the shell and resolves to its exit status.
  const exec = (src) => globalThis.kwasm.shell(src, (s) => term.write(s), shellBuiltins);

  const prompt = () => {
    if (pending) return "> ";
    const dir = globalThis.kwasm.cwd().replace(/^\/root(?=\/|$)/, "~");
    return `\x1b[1;34mkwasm\x1b[0m:\x1b[1;32m${dir}\x1b[0m$ `;
  };
  // redraw renders prompt and line, also when the line wraps over several rows.
  let row = 0; // row of the cursor, relative to the prompt
  const redraw = () => {
    const p = prompt(), plen = p.replace(/\x1b\[[0-9;]*m/g, "").length, cols = term.cols;
    let out = (row > 0 ? `\x1b[${row}A` : "") + "\r\x1b[J" + p + line;
    const end = plen + line.length, at = plen + cursor;
    if (end > 0 && end % cols === 0) out += "\r\n"; // leave the pending wrap state
    const up = Math.floor(end / cols) - Math.floor(at / cols);
    out += (up > 0 ? `\x1b[${up}A` : "") + "\r" + (at % cols ? `\x1b[${at % cols}C` : "");
    row = Math.floor(at / cols);
    term.write(out);
  };
  const newline = (s = "") => { term.write(s + "\r\n"); row = 0; };

  async function submit() {
    const input = line;
    cursor = line.length; redraw(); // the cursor goes behind the line first
    line = ""; cursor = 0;
    newline();
    if (!pending && input.trim()) history.push(input);
    hpos = history.length;
    const src = pending ? pending + "\n" + input : input;
    // Open quotes, here-documents, loops without "done": read more lines.
    if (globalThis.kwasm.incomplete(src)) {
      pending = src;
      redraw();
      return;
    }
    pending = "";
    if (src.trim()) {
      busy = true;
      try { await exec(src); } catch (e) { term.writeln(`kwasm: ${e.message}`); }
      busy = false;
      // Output without a final newline: start the prompt on a new line.
      await new Promise((r) => term.write("", r));
      if (term.buffer.active.cursorX > 0) term.write("\r\n");
    }
    row = 0;
    redraw();
  }

  redraw();
  term.onData((data) => {
    if (!busy) return feed(data);
    if (data === "\x03") {
      globalThis.kwasm.cancel();
      term.write("^C\r\n");
    }
  });
  function feed(data) {
    for (let i = 0; i < data.length; i++) {
      let c = data[i];
      if (c === "\r" || c === "\n") {
        // A paste with several lines (a here-document, a script) runs line by line.
        const rest = data.slice(i + 1).replace(/^\n/, "");
        submit().then(() => rest && feed(rest));
        return;
      }
      if (c === "\x7f") { if (cursor > 0) { line = line.slice(0, cursor - 1) + line.slice(cursor); cursor--; redraw(); } continue; }
      if (c === "\x03") { cursor = line.length; redraw(); newline("^C"); line = ""; cursor = 0; pending = ""; redraw(); continue; }
      if (c === "\x0c") { term.clear(); row = 0; redraw(); continue; }
      if (c === "\x15") { line = line.slice(cursor); cursor = 0; redraw(); continue; }
      if (c === "\x01") { cursor = 0; redraw(); continue; }
      if (c === "\x05") { cursor = line.length; redraw(); continue; }
      if (c === "\x1b") {
        const seq = data.slice(i, i + 3);
        i += 2;
        if (seq === "\x1b[A" && hpos > 0) { line = history[--hpos]; cursor = line.length; redraw(); }
        else if (seq === "\x1b[B") { hpos = Math.min(history.length, hpos + 1); line = history[hpos] || ""; cursor = line.length; redraw(); }
        else if (seq === "\x1b[D" && cursor > 0) { cursor--; redraw(); }
        else if (seq === "\x1b[C" && cursor < line.length) { cursor++; redraw(); }
        continue;
      }
      if (c === "\t") c = " ";
      if (c >= " ") { line = line.slice(0, cursor) + c + line.slice(cursor); cursor++; redraw(); }
    }
  }
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
