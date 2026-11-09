// vnc <port> [password] shows the VNC server on localhost:<port> of the
// in-browser cluster (kubectl port-forward) on top of the terminal. noVNC
// speaks RFB, and tabernetes.dial (k8s/tabernetes/dial.go) carries the bytes:
// page -> in-process network -> port-forward -> WebRTC -> node -> pod.
// doom (k8s/tabernetes/doom.go) uses it for KubeDoom.
import RFB from "@novnc/novnc";

const B = (s) => `\x1b[1m${s}\x1b[0m`;

// TCPChannel looks like a WebSocket to noVNC, which accepts any object with
// these properties instead of a URL.
class TCPChannel {
  constructor() {
    this.binaryType = "arraybuffer";
    this.protocol = "";
    this.readyState = "connecting";
    this.onopen = null;
    this.onmessage = null;
    this.onerror = null;
    this.onclose = null;
    this.conn = null;
  }

  // connect dials after noVNC attached its handlers.
  connect(port) {
    const closed = (code, reason) => {
      if (this.readyState === "closed") return;
      this.readyState = "closed";
      this.onclose?.({ code, reason, wasClean: code === 1000 });
    };
    // Go may call back while noVNC is inside send or close: deliver after
    // the current JS call, like a real WebSocket, so noVNC is never re-entered.
    const later = (fn) => (...a) => queueMicrotask(() => fn(...a));
    globalThis.tabernetes.dial(port, later((data) => this.onmessage?.({ data: data.buffer })), later(() => closed(1000, "")))
      .then((conn) => {
        this.conn = conn;
        if (this.readyState !== "connecting") return conn.close();
        this.readyState = "open";
        this.onopen?.();
      })
      .catch((err) => {
        this.onerror?.(err);
        closed(1006, err.message);
      });
  }

  send(data) {
    if (this.readyState !== "open") return;
    this.conn.send(data instanceof ArrayBuffer ? new Uint8Array(data) : new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
  }

  close() {
    if (this.readyState === "closed") return;
    this.readyState = "closing";
    this.conn?.close();
  }
}

// view shows the screen in an overlay of box and resolves when it closes,
// also when Ctrl-C ends the command line before the screen took the keys.
function view(box, port, password, write, active) {
  return new Promise((resolve) => {
    const overlay = document.createElement("div");
    overlay.className = "tabernetes-vnc";
    const screen = document.createElement("div");
    screen.className = "tabernetes-vnc-screen";
    const close = document.createElement("button");
    close.type = "button";
    close.className = "tabernetes-vnc-close";
    close.textContent = "Close";
    close.title = `Close the VNC viewer (localhost:${port})`;
    overlay.append(screen, close);
    box.append(overlay);

    const channel = new TCPChannel();
    const rfb = new RFB(screen, channel, { credentials: { password } });
    rfb.scaleViewport = true;
    rfb.background = "#000";
    let reason = "";
    rfb.addEventListener("connect", () => rfb.focus());
    rfb.addEventListener("credentialsrequired", () => {
      if (password) rfb.sendCredentials({ password });
      else { reason = "the server wants a password: vnc <port> <password>"; rfb.disconnect(); }
    });
    rfb.addEventListener("securityfailure", (e) => { reason = `security failure: ${e.detail.reason || e.detail.status}`; });
    const watch = setInterval(() => active() || rfb.disconnect(), 250);
    rfb.addEventListener("disconnect", (e) => {
      clearInterval(watch);
      overlay.remove();
      if (!e.detail.clean && !reason) reason = "the connection broke";
      if (reason) write(`vnc: ${reason}\n`);
      resolve(reason ? 1 : 0);
    });
    close.addEventListener("click", () => rfb.disconnect());
    channel.connect(port);
  });
}

export function vncBuiltins({ box }) {
  return {
    async vnc(args, { write, term, active }) {
      const port = Number(args[0]);
      if (args.length < 1 || args.length > 2 || !Number.isInteger(port) || port < 1 || port > 65535) {
        write("usage: vnc <port> [password]\n");
        write("Shows the VNC server on localhost:<port> on top of the terminal, for example:\n");
        write(`  ${B("kubectl port-forward pod/desktop 5900:5900 &")}\n`);
        write(`  ${B("vnc 5900")}\n`);
        return 2;
      }
      const code = await view(box, port, args[1] || "", write, active);
      term.focus();
      return code;
    },
  };
}
