// Browser terminal for kwasm: boots the control plane and runs a shell.
// The shell (mvdan.cc/sh) and kubectl live in the wasm binary. This file
// only does the line editing and the builtins that need the browser.
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
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
  // Windows and Linux copy and paste with Ctrl, which xterm.js sends to the
  // shell as ^C and ^V. Like Windows Terminal: Ctrl-C copies while text is
  // selected and cancels otherwise, Ctrl-V (and Ctrl-Shift-C/V) leave the
  // clipboard to the browser. macOS has Cmd-C and Cmd-V for that.
  if (!/Mac|iPhone|iPad/.test(navigator.platform)) {
    term.attachCustomKeyEventHandler((e) => {
      if (e.type !== "keydown" || !e.ctrlKey || e.altKey || e.metaKey) return true;
      const key = e.key.toLowerCase();
      if (key === "c" && (e.shiftKey || term.hasSelection())) {
        if (term.hasSelection()) navigator.clipboard?.writeText(term.getSelection()).catch(() => {});
        term.clearSelection();
        e.preventDefault();
        return false;
      }
      // The browser then fires a paste event, which xterm.js reads.
      return key !== "v";
    });
  }
  new ResizeObserver(() => fit.fit()).observe(el);

  const logs = [];
  globalThis.kwasmLogs = logs;
  // A crash trace is kept apart from the rotating log buffer, which would drop
  // its beginning while the runtime prints thousands of goroutines.
  const crash = [];
  globalThis.kwasmCrash = crash;
  let crashing = false;
  installFS((fd, s) => {
    if (crashing || /^(panic:|fatal error:)/m.test(s)) {
      crashing = true;
      console.error(s);
      if (crash.length < 2000) crash.push(s);
    }
    // Control plane logs: visible with the "Verbose" level in the DevTools console.
    else console.debug(s.replace(/\n$/, ""));
    // Expected in a single-threaded control plane: a controller found its
    // informer a few versions behind its own write and retries. Kept in the
    // console, left out of "logs".
    if (s.includes("is not as new as written version")) return;
    logs.push(s);
    if (logs.length > 5000) logs.splice(0, logs.length - 5000);
  });

  term.writeln("\x1b[1mkwasm\x1b[0m: a real Kubernetes control plane, compiled to WebAssembly, running in your browser.");
  term.writeln(DIM("kube-apiserver, kube-controller-manager, kube-scheduler and kubectl are the unmodified upstream"));
  term.writeln(DIM("components. etcd is replaced by a tiny in-memory implementation. Nothing leaves your browser."));
  term.writeln("");

  // ---- download + boot ----
  // Download, decompression (in a worker) and compilation run at the same time.
  const t0 = performance.now();
  const since = () => ((performance.now() - t0) / 1000).toFixed(1).padStart(5);
  globalThis.kwasmStatus = (s) => term.writeln(`${DIM(`[${since()}s]`)} ${s}`);
  const dl = await download(opts.wasmURL);
  const { body, cached } = dl;
  // The page knows the exact size of the file (data-size), the header only maybe.
  const total = opts.wasmSize || dl.total;
  let loaded = 0;
  // The line must fit into one row: once it wraps, \r only returns to the
  // start of the last row and every update adds a new line.
  const progress = (n, t) => {
    const mb = `${(n / 1048576).toFixed(1)}${t ? "/" + (t / 1048576).toFixed(1) : ""} MB`;
    let what = cached ? "compiling from the browser cache" : "downloading and compiling";
    const room = () => term.cols - 1 - what.length - mb.length - 4; // spaces and brackets
    if (room() < 10) what = cached ? "compiling" : "downloading";
    const w = Math.max(0, Math.min(30, room()));
    const f = t ? Math.min(w, Math.round((n / t) * w)) : 0;
    const line = `${DIM(what)} ${w ? `[${"#".repeat(f)}${" ".repeat(w - f)}] ` : ""}${mb}`;
    return `\r\x1b[K${line}`;
  };
  const counted = body.pipeThrough(new TransformStream({
    transform(chunk, ctl) {
      loaded += chunk.length;
      term.write(progress(loaded, total));
      ctl.enqueue(chunk);
    },
  }));
  const go = new globalThis.Go();
  go.env = { HOME: "/root", USER: "root", PATH: "/usr/bin" };
  const instance = await instantiate(unpack(opts.wasmURL, counted, total, opts.workerURL), go.importObject);
  term.writeln("");
  globalThis.kwasmStatus(cached ? "compiled WebAssembly from the browser cache" : "downloaded and compiled WebAssembly");
  // The "free" command reads the size of the linear memory from here.
  globalThis.kwasmMemory = instance.exports.mem;
  const exited = (why) => {
    term.writeln(`\r\n\x1b[31mthe control plane exited${why ? `: ${why}` : ""}\x1b[0m`);
    if (crash.length) {
      // The first lines name the cause and the crashing goroutine.
      term.writeln(crash.join("").split("\n").slice(0, 30).join("\n"));
      term.writeln(DIM("The full trace is in kwasmCrash in the DevTools console."));
    }
  };
  go.run(instance).then(() => exited(), (e) => exited(e?.message ?? String(e)));
  try {
    await globalThis.kwasm.ready;
  } catch (e) {
    term.writeln(`\r\n\x1b[31mthe cluster did not start: ${e.message}\x1b[0m`);
    term.writeln(DIM("The control plane logs are in kwasmLogs in the DevTools console. Reload to try again."));
    return { term };
  }
  term.writeln("");
  term.writeln(`Try ${"\x1b[1m"}k version${"\x1b[0m"}, ${"\x1b[1m"}join${"\x1b[0m"} or ${"\x1b[1m"}help${"\x1b[0m"}.`);

  // ---- shell ----
  globalThis.kwasm.resize(term.cols, term.rows);
  term.onResize(({ cols, rows }) => globalThis.kwasm.resize(cols, rows));
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
        "  kubectl edit ...         opens vi: :wq applies, :q! cancels",
        "",
        "Terminal",
        "  join                     connect a real node via WebRTC (needs docker)",
        "  accept <answer>          finish a join later (join asks for it)",
        "  proxy <port>             open localhost:<port> (kubectl port-forward) in a new tab",
        "  logs [n]                 the last n lines of the control plane logs",
        "  clear                    clear the screen",
        ...terminal.filter((n) => !["join", "accept", "proxy", "logs", "clear"].includes(n)).map((n) => `  ${n}`),
        "",
        "Tools (small versions, the common flags)",
        ...list(tools),
        "",
        "Shell builtins (help <name> for details)",
        ...list(shell),
        "",
        "Example: kubectl get pods -A -o name | grep dns | xargs kubectl describe",
        "Keys: Tab completes (kubectl too), Ctrl-C cancels a running command, Ctrl-L clears,",
        "↑/↓ history, Ctrl-W/Alt-⌫ delete a word, Ctrl-K/Ctrl-U delete to the end/start.",
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
  // ask reads one line from the keyboard while a builtin runs (join waits for
  // the accept line). Ctrl-C rejects.
  let reader = null;
  const ask = (text) => new Promise((resolve, reject) => {
    reader = { line: "", resolve, reject };
    term.write(text);
  });
  function readerFeed(data) {
    for (const c of data) {
      if (!reader) return;
      if (c === "\r" || c === "\n") {
        const r = reader;
        reader = null;
        term.write("\r\n");
        r.resolve(r.line);
      } else if (c === "\x03") {
        const r = reader;
        reader = null;
        term.write("^C\r\n");
        r.reject(new Error("cancelled"));
      } else if (c === "\x7f") {
        if (reader.line) { reader.line = reader.line.slice(0, -1); term.write("\b \b"); }
      } else if (c >= " ") {
        reader.line += c;
        term.write(c);
      }
    }
  }

  // Builtins get (args, {write, stdin}) from the shell, plus the terminal.
  // Ctrl-C ends the command line, but a JS builtin runs on: active() tells it
  // whether it still belongs to the running command line, and ask() rejects
  // when it does not, so a canceled join cannot take the keys later.
  let run = 0;
  const shellBuiltins = Object.fromEntries(Object.entries(builtins).map(([name, fn]) => [name, (args, ctx) => {
    const id = run, active = () => busy && id === run;
    const askActive = (text) => (active() ? ask(text) : Promise.reject(new Error("cancelled")));
    return fn(args, { term, exec, ask: askActive, active, write: ctx.write, stdin: ctx.stdin });
  }]));

  // exec runs a command line in the shell and resolves to its exit status.
  const exec = (src) => globalThis.kwasm.shell(src, (s) => term.write(s), shellBuiltins);

  const prompt = () => {
    if (pending) return "> ";
    const dir = globalThis.kwasm.cwd().replace(/^\/root(?=\/|$)/, "~");
    return `\x1b[1;34mjkroepke.de\x1b[0m:\x1b[1;32m${dir}\x1b[0m$ `;
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
      run++;
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
    // A builtin that asks for a line (join) gets the keys first.
    if (reader) return readerFeed(data);
    // Keys typed while a completion is running wait for it.
    if (completing) { queued += data; return; }
    if (!busy) return feed(data);
    // A full screen program (vi, kubectl edit) gets every key.
    if (globalThis.kwasm.rawMode()) return globalThis.kwasm.input(data);
    if (data === "\x03") {
      run++;
      globalThis.kwasm.cancel();
      term.write("^C\r\n");
    }
  });
  // edit applies a change to the line and redraws it.
  const edit = (newLine, newCursor) => { line = newLine; cursor = newCursor; redraw(); };
  const wordStart = () => { let i = cursor; while (i > 0 && line[i - 1] === " ") i--; while (i > 0 && line[i - 1] !== " ") i--; return i; };
  const wordEnd = () => { let i = cursor; while (i < line.length && line[i] === " ") i++; while (i < line.length && line[i] !== " ") i++; return i; };
  // Escape sequences of the keys, also the variants of other terminals and of
  // the application cursor mode (ESC O).
  const keys = {
    "\x1b[A": "up", "\x1bOA": "up", "\x1b[B": "down", "\x1bOB": "down",
    "\x1b[C": "right", "\x1bOC": "right", "\x1b[D": "left", "\x1bOD": "left",
    "\x1b[H": "home", "\x1bOH": "home", "\x1b[1~": "home", "\x1b[7~": "home",
    "\x1b[F": "end", "\x1bOF": "end", "\x1b[4~": "end", "\x1b[8~": "end",
    "\x1b[3~": "delete",
    "\x1b[1;5D": "wordLeft", "\x1b[1;3D": "wordLeft", "\x1bb": "wordLeft",
    "\x1b[1;5C": "wordRight", "\x1b[1;3C": "wordRight", "\x1bf": "wordRight",
    "\x1b\x7f": "deleteWord", "\x1bd": "deleteWordRight",
  };
  const actions = {
    up() { if (hpos > 0) { const l = history[--hpos]; edit(l, l.length); } },
    down() { hpos = Math.min(history.length, hpos + 1); const l = history[hpos] || ""; edit(l, l.length); },
    left() { if (cursor > 0) edit(line, cursor - 1); },
    right() { if (cursor < line.length) edit(line, cursor + 1); },
    home() { edit(line, 0); },
    end() { edit(line, line.length); },
    delete() { if (cursor < line.length) edit(line.slice(0, cursor) + line.slice(cursor + 1), cursor); },
    wordLeft() { edit(line, wordStart()); },
    wordRight() { edit(line, wordEnd()); },
    deleteWord() { const i = wordStart(); edit(line.slice(0, i) + line.slice(cursor), i); },
    deleteWordRight() { edit(line.slice(0, cursor) + line.slice(wordEnd()), cursor); },
  };

  // complete asks the shell for the candidates of the word before the cursor.
  // One candidate replaces the word, several extend it to their common
  // prefix, or are listed when there is nothing to extend.
  let completing = false, queued = "";
  async function complete() {
    completing = true;
    try {
      const { word, items, noSpace } = await globalThis.kwasm.complete(line.slice(0, cursor), Object.keys(shellBuiltins));
      if (!items.length) return term.write("\x07");
      let insert = items[0];
      if (items.length === 1) insert += noSpace ? "" : " ";
      else for (const it of items) while (!it.startsWith(insert)) insert = insert.slice(0, -1);
      if (items.length === 1 || insert.length > word.length) {
        const start = cursor - word.length;
        return edit(line.slice(0, start) + insert + line.slice(cursor), start + insert.length);
      }
      // List the candidates in columns below the line, then the line again.
      const at = cursor;
      edit(line, line.length);
      const shown = items.slice(0, 200), width = Math.max(...shown.map((it) => it.length)) + 2;
      const perRow = Math.max(1, Math.floor(term.cols / width)), rows = [];
      for (let i = 0; i < shown.length; i += perRow) rows.push(shown.slice(i, i + perRow).map((it) => it.padEnd(width)).join("").trimEnd());
      if (items.length > shown.length) rows.push(DIM(`... ${items.length - shown.length} more`));
      newline();
      term.write(rows.join("\r\n") + "\r\n");
      cursor = at;
      redraw();
    } catch (e) {
      console.debug("completion failed", e);
    } finally {
      completing = false;
      const rest = queued;
      queued = "";
      if (rest) feed(rest);
    }
  }

  function feed(data) {
    for (let i = 0; i < data.length; i++) {
      const c = data[i];
      if (c === "\r" || c === "\n") {
        // A paste with several lines (a here-document, a script) runs line by line.
        const rest = data.slice(i + 1).replace(/^\n/, "");
        submit().then(() => rest && feed(rest));
        return;
      }
      if (c === "\t") {
        queued += data.slice(i + 1);
        complete();
        return;
      }
      if (c === "\x1b") {
        // CSI (ESC [ params final), SS3 (ESC O x) or Alt+key (ESC x). Unknown
        // sequences are skipped whole, so their tail does not land in the line.
        const seq = /^\x1b(?:\[[0-9;?]*[@-~]|O.|[^]?)/.exec(data.slice(i))[0];
        i += seq.length - 1;
        actions[keys[seq]]?.();
        continue;
      }
      switch (c) {
        case "\x7f": case "\b": if (cursor > 0) edit(line.slice(0, cursor - 1) + line.slice(cursor), cursor - 1); break;
        case "\x03": cursor = line.length; redraw(); newline("^C"); line = ""; cursor = 0; pending = ""; redraw(); break;
        case "\x0c": term.clear(); row = 0; redraw(); break;
        case "\x15": edit(line.slice(cursor), 0); break; // Ctrl-U
        case "\x0b": edit(line.slice(0, cursor), cursor); break; // Ctrl-K
        case "\x17": actions.deleteWord(); break; // Ctrl-W
        case "\x04": actions.delete(); break; // Ctrl-D
        case "\x01": actions.home(); break; // Ctrl-A
        case "\x05": actions.end(); break; // Ctrl-E
        case "\x02": actions.left(); break; // Ctrl-B
        case "\x06": actions.right(); break; // Ctrl-F
        case "\x10": actions.up(); break; // Ctrl-P
        case "\x0e": actions.down(); break; // Ctrl-N
        default:
          if (c >= " ") edit(line.slice(0, cursor) + c + line.slice(cursor), cursor + 1);
      }
    }
  }
  term.focus();
  return { term, exec };
}

// download fetches the binary, from the Cache Storage when it is there. The
// file name has a hash of the content (Hugo fingerprint), so a cached copy is
// always current. GitHub Pages sends max-age=600 and nothing longer.
async function download(url) {
  let cache = null;
  try { cache = await caches.open("kwasm"); } catch { /* no Cache Storage (http, private mode) */ }
  const hit = await cache?.match(url).catch(() => null);
  if (hit) return { body: hit.body, total: plainLength(hit), cached: true };
  const resp = await fetch(url);
  if (!resp.ok) throw new Error(`download failed: ${resp.status}`);
  if (cache) {
    const href = new URL(url, location.href).href;
    cache.put(url, resp.clone())
      .then(() => cache.keys())
      .then((keys) => Promise.all(keys.filter((k) => k.url !== href).map((k) => cache.delete(k))))
      .catch((e) => console.warn("kwasm: caching the binary failed:", e));
  }
  return { body: resp.body, total: plainLength(resp), cached: false };
}

// plainLength is the Content-Length when it counts the bytes the body stream
// yields. With a Content-Encoding (a CDN may gzip the file on the way), it
// counts the encoded bytes and the progress would end above 100 %.
function plainLength(resp) {
  const enc = resp.headers.get("content-encoding");
  return enc && enc !== "identity" ? 0 : Number(resp.headers.get("content-length")) || 0;
}

// GitHub Pages sends no Content-Encoding for the binary, so the page unpacks
// it. DecompressionStream has no zstd (Chromium 153), so fzstd does it in
// JavaScript. The file is a row of independent zstd frames of 32 MB each
// (kwasm/mise.toml). Workers unpack each frame as soon as it is downloaded,
// several at once, and the unpacked bytes go on to the compiler in order.
function unpack(url, stream, size, workerURL) {
  const path = new URL(url, location.href).pathname;
  if (path.endsWith(".gz")) return stream.pipeThrough(new DecompressionStream("gzip"));
  if (!path.endsWith(".zst")) return stream;
  const workers = Array.from({ length: Math.min(4, navigator.hardwareConcurrency || 2) }, () => {
    const w = new Worker(workerURL), queue = []; // a worker answers in order
    w.onmessage = ({ data }) => { const q = queue.shift(); data.error ? q.reject(new Error(data.error)) : q.resolve(data.out); };
    w.onerror = (e) => { e.preventDefault(); queue.splice(0).forEach((q) => q.reject(new Error(`zstd worker: ${e.message}`))); };
    w.unpack = (frame) => new Promise((resolve, reject) => { queue.push({ resolve, reject }); w.postMessage(frame, [frame.buffer]); });
    return w;
  });
  return new ReadableStream({
    async start(ctl) {
      try {
        let n = 0, last = Promise.resolve();
        for await (const frame of zstdFrames(stream, size)) {
          const out = workers[n++ % workers.length].unpack(frame);
          last = last.then(() => out).then((b) => ctl.enqueue(b));
        }
        await last;
        ctl.close();
      } catch (e) {
        ctl.error(e);
      } finally {
        workers.forEach((w) => w.terminate());
      }
    },
  });
}

// zstdFrames yields each zstd frame of the stream once all of it is there.
async function* zstdFrames(stream, size) {
  let buf = new Uint8Array(size || 32 << 20), len = 0, start = 0;
  const r = stream.getReader();
  for (;;) {
    const { value, done } = await r.read();
    if (value) {
      if (len + value.length > buf.length) {
        const b = new Uint8Array(Math.max(2 * buf.length, len + value.length));
        b.set(buf.subarray(0, len));
        buf = b;
      }
      buf.set(value, len);
      len += value.length;
    }
    for (let end; (end = frameEnd(buf.subarray(start, len))); start += end) yield buf.slice(start, start + end);
    if (done) {
      if (start < len) throw new Error("zstd: the download is truncated");
      return;
    }
  }
}

// frameEnd returns the length of the zstd frame at the start of b, or 0 when
// b does not hold all of it yet. See RFC 8878, section 3.1.1.
function frameEnd(b) {
  if (b.length < 6) return 0;
  if (((b[0] | b[1] << 8 | b[2] << 16 | b[3] << 24) >>> 0) !== 0xfd2fb528) throw new Error("zstd: bad frame magic");
  const fhd = b[4], single = (fhd >> 5) & 1;
  let p = 5 + (single ? 0 : 1) + [0, 1, 2, 4][fhd & 3] + [single, 2, 4, 8][fhd >> 6];
  for (let last = 0; !last;) {
    if (p + 3 > b.length) return 0;
    const h = b[p] | b[p + 1] << 8 | b[p + 2] << 16;
    last = h & 1;
    p += 3 + (((h >> 1) & 3) === 1 ? 1 : h >>> 3); // an RLE block has one byte
  }
  p += fhd & 4 ? 4 : 0; // checksum
  return p <= b.length ? p : 0;
}

// instantiate compiles while the bytes still arrive.
async function instantiate(stream, imports) {
  if (WebAssembly.instantiateStreaming) {
    const resp = new Response(stream, { headers: { "Content-Type": "application/wasm" } });
    return (await WebAssembly.instantiateStreaming(resp, imports)).instance;
  }
  const bytes = await new Response(stream).arrayBuffer();
  return (await WebAssembly.instantiate(bytes, imports)).instance;
}
