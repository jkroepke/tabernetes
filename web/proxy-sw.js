// Service worker behind the "proxy <port>" command of the Tabernetes terminal.
//
// A tab on /tabernetes-proxy/<port>/... shows what listens on localhost:<port>
// inside the WebAssembly of the terminal tab, typically kubectl
// port-forward. A web page cannot open a TCP port, so this worker hands each
// request to the terminal tab, which runs it against its in-process network
// and posts the response back.
const scope = new URL(self.registration.scope).pathname; // "/tabernetes-proxy/"
// A model in a tab may need minutes for an answer on the chat page of llm.
// The terminal tab gives up a little earlier (k8s/tabernetes/proxy.go).
const TIMEOUT = 300_000;

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

// A terminal tab says hello with the ports it answers for: when it runs
// "proxy", and when its kubectl port-forward listens. Every tab has its own
// localhost, so the port decides which tab gets a request. The browser may
// stop the worker and start it again, so this is only the first guess.
const hosts = new Map(); // port -> client id of the terminal tab
const terminals = new Set(); // client ids of the terminal tabs
self.addEventListener("message", (e) => {
  const id = e.source?.id;
  if (!id) return;
  if (e.data?.type === "tabernetes-host") {
    terminals.add(id);
    for (const p of e.data.ports || []) hosts.set(p, id);
  } else if (e.data?.type === "tabernetes-bye") {
    // The tab goes away. The worker goes with the last one.
    terminals.delete(id);
    for (const [p, h] of hosts) if (h === id) hosts.delete(p);
    if (!terminals.size) self.registration.unregister();
  }
});

// portOf returns the port of a path below the scope, or 0.
const portOf = (pathname) => {
  const m = pathname.startsWith(scope) && pathname.slice(scope.length).match(/^(\d{1,5})(?=\/|$)/);
  return m ? Number(m[1]) : 0;
};

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (url.origin !== self.location.origin) return;
  const port = portOf(url.pathname);
  if (port) {
    const rest = url.pathname.slice(scope.length + String(port).length);
    // Relative links need the trailing slash: /tabernetes-proxy/80 -> /tabernetes-proxy/80/
    if (!rest) return e.respondWith(Response.redirect(`${url.pathname}/${url.search}`, 302));
    return e.respondWith(forward(e.request, port, rest + url.search));
  }
  // An absolute path ("/style.css") on a proxied page belongs to its port.
  if (e.clientId) {
    e.respondWith((async () => {
      const client = await self.clients.get(e.clientId);
      const p = client && portOf(new URL(client.url).pathname);
      return p ? forward(e.request, p, url.pathname + url.search) : fetch(e.request);
    })());
  }
});

async function forward(req, port, path) {
  const body = req.method === "GET" || req.method === "HEAD" ? null : await req.arrayBuffer();
  const msg = { type: "tabernetes-proxy", port, method: req.method, path, headers: [...req.headers], body };
  let r;
  try {
    r = await ask(msg);
  } catch (err) {
    return text(502, `proxy ${port}: ${err.message}`);
  }
  if (r.error) return text(502, `proxy ${port}: ${r.error}`);
  const headers = new Headers(r.headers);
  const loc = headers.get("location");
  if (loc) {
    // Redirects to localhost stay in the proxy.
    const u = new URL(loc, `http://localhost:${port}${path}`);
    if (["localhost", "127.0.0.1", "[::1]"].includes(u.hostname)) {
      headers.set("location", `${scope}${u.port || 80}${u.pathname}${u.search}${u.hash}`);
    }
  }
  const nullBody = [101, 204, 205, 304].includes(r.status);
  return new Response(nullBody ? null : r.body, { status: r.status, statusText: r.statusText, headers });
}

// ask sends the request to the terminal tab of the port. Without a known
// one, or when nothing listens there anymore, every window of the site gets
// it: the first Tabernetes terminal with an answer wins.
async function ask(msg) {
  const id = hosts.get(msg.port);
  const known = id && (await self.clients.get(id));
  let first;
  if (known) {
    first = await askTabs([known], msg);
    if (!first.error) return first;
  }
  const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const tabs = all.filter((c) => c.id !== id && !new URL(c.url).pathname.startsWith(scope));
  if (!tabs.length) {
    if (first) return first;
    // The terminal tab is gone without a goodbye (crash, closed browser).
    self.registration.unregister();
    throw new Error("the Tabernetes terminal tab is closed");
  }
  const r = await askTabs(tabs, msg);
  return r.error && first ? first : r;
}

// askTabs resolves to the first answer without an error, or to the error
// when no tab has a better one.
function askTabs(tabs, msg) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(
      "no answer from the Tabernetes terminal tab (is the cluster running?)")), TIMEOUT);
    let open = tabs.length, error = null;
    for (const tab of tabs) {
      const { port1, port2 } = new MessageChannel();
      port1.onmessage = (m) => {
        port1.close();
        if (m.data.error) {
          error ||= m.data;
          // A page of the site without a terminal never answers: give the
          // others a moment, then report what the terminal said.
          if (--open > 0) return setTimeout(() => { clearTimeout(timer); resolve(error); }, 2000);
        } else {
          terminals.add(tab.id);
          hosts.set(msg.port, tab.id);
        }
        clearTimeout(timer);
        resolve(m.data);
      };
      // The body goes to every tab: no transfer.
      tab.postMessage(msg, [port2]);
    }
  });
}

function text(status, s) {
  return new Response(s + "\n", { status, headers: { "content-type": "text/plain; charset=utf-8" } });
}
