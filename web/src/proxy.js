// proxy <port> opens what listens on localhost:<port> of the in-browser
// cluster (kubectl port-forward) in a new tab. A service worker
// (web/proxy-sw.js, served as /tabernetes-proxy/sw.js) catches the requests of that tab and posts them
// here; Go runs them against its in-process network.
const B = (s) => `\x1b[1m${s}\x1b[0m`;
const DIM = (s) => `\x1b[2m${s}\x1b[0m`;

export function proxyBuiltins({ worker = "/tabernetes-proxy/sw.js", scope = "/tabernetes-proxy/", status } = {}) {
  const sw = navigator.serviceWorker;
  if (sw) {
    sw.addEventListener("message", async (e) => {
      if (e.data?.type !== "tabernetes-proxy") return;
      const [reply] = e.ports;
      const { port, method, path, headers, body } = e.data;
      try {
        const r = await globalThis.tabernetes.proxy(port, method, path, headers, body ? new Uint8Array(body) : null);
        reply.postMessage(r, [r.body.buffer]);
      } catch (err) {
        reply.postMessage({ error: err.message });
      }
    });
    sw.startMessages();
  }

  // The worker only serves the terminal tabs of this page. Every tab that
  // used it says goodbye, and the worker unregisters when the last one is
  // gone. Tabs it still controls keep it until they close.
  let last = null;
  let host = null;
  addEventListener("pagehide", () => {
    last?.postMessage({ type: "tabernetes-bye" });
    last = null;
  });
  // Register each time: the worker may have unregistered in the meantime.
  async function activeWorker() {
    const reg = await sw.register(worker, { scope });
    if (!reg.active) {
      const w = reg.installing || reg.waiting;
      await new Promise((resolve) => w.addEventListener("statechange", () => w.state === "activated" && resolve()));
    }
    return (last = reg.active);
  }
  // The browser's client ID stays valid when the service worker restarts.
  async function hello() {
    const worker = await activeWorker();
    host = await new Promise((resolve, reject) => {
      const { port1, port2 } = new MessageChannel();
      const timer = setTimeout(() => {
        port1.close();
        reject(new Error("the proxy worker does not answer"));
      }, 10000);
      port1.onmessage = ({ data }) => {
        clearTimeout(timer);
        port1.close();
        resolve(data.id);
      };
      worker.postMessage({ type: "tabernetes-host" }, [port2]);
    });
  }

  const urlOf = (port) => new URL(`${scope}${encodeURIComponent(host)}/${port}/`, location.href).href;

  // openTab opens localhost:<port> in a new tab. window.open needs the click
  // or key press that started it: open the tab first, the worker is ready
  // before the tab sends its request.
  async function openTab(port) {
    const tab = window.open("about:blank", "_blank");
    try {
      await hello();
    } catch (err) {
      tab?.close();
      throw err;
    }
    if (tab) tab.location.href = urlOf(port);
    return tab;
  }

  // The status line links every port of "kubectl port-forward" while it
  // listens. Go calls this when the list changes (tabernetes/forwards.go).
  let render = 0;
  globalThis.tabernetesForwards = async (list) => {
    if (!status) return;
    const current = ++render;
    if (list.length && sw) {
      try { await hello(); } catch (err) { console.error("tabernetes proxy:", err); return; }
    }
    if (current !== render) return;
    status.replaceChildren(...list.map(({ port, target }) => {
      const label = `\u21C4 localhost:${port} \u2192 ${target}`;
      if (!sw) {
        const el = document.createElement("span");
        el.className = "tabernetes-port";
        el.textContent = label;
        return el;
      }
      const a = document.createElement("a");
      a.className = "tabernetes-port";
      a.href = urlOf(port);
      a.target = "_blank";
      a.textContent = label;
      a.title = `Open localhost:${port} of this tab in a new tab (kubectl port-forward to ${target})`;
      a.addEventListener("click", (e) => {
        e.preventDefault();
        openTab(port).catch((err) => console.error("tabernetes proxy:", err));
      });
      return a;
    }));
  };

  return {
    async proxy(args, { term }) {
      const port = Number(args[0]);
      if (args.length !== 1 || !Number.isInteger(port) || port < 1 || port > 65535) {
        term.writeln("usage: proxy <port>");
        term.writeln("Opens what listens on localhost:<port> in a new tab, for example:");
        term.writeln(`  ${B("kubectl port-forward deploy/nginx 8080:80 &")}`);
        term.writeln(`  ${B("proxy 8080")}`);
        return 2;
      }
      if (!sw) {
        term.writeln("proxy: this browser offers no service workers here (private mode?)");
        return 1;
      }
      let tab;
      try {
        tab = await openTab(port);
      } catch (err) {
        term.writeln(`proxy: ${err.message}`);
        return 1;
      }
      const url = urlOf(port);
      term.writeln(`${tab ? "Opened" : "Open"} ${url}`);
      term.writeln(DIM(`It shows localhost:${port} of this tab and works while this tab is open.`));
      return 0;
    },
  };
}
