// Service worker behind the "proxy <port>" command of the Tabernetes terminal.
//
// A tab on /tabernetes-proxy/<terminal>/<port>/... shows localhost:<port>
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

// The URL names the terminal's browser client ID. Different terminals may
// use the same port, and the route survives a restart of this worker.
const terminals = new Set(); // client ids of the terminal tabs
self.addEventListener("message", (e) => {
  const id = e.source?.id;
  if (!id) return;
  if (e.data?.type === "tabernetes-host") {
    terminals.add(id);
    e.ports[0]?.postMessage({ id });
  } else if (e.data?.type === "tabernetes-bye") {
    // The tab goes away. The worker goes with the last one.
    terminals.delete(id);
    if (!terminals.size) self.registration.unregister();
  }
});

// routeOf returns the terminal, port and remaining path below the scope.
const routeOf = (pathname) => {
  const m = pathname.startsWith(scope) && pathname.slice(scope.length).match(/^([^/]+)\/(\d{1,5})(?=\/|$)/);
  if (!m || Number(m[2]) < 1 || Number(m[2]) > 65535) return null;
  try {
    return { host: decodeURIComponent(m[1]), port: Number(m[2]), rest: pathname.slice(scope.length + m[0].length) };
  } catch { return null; }
};

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (url.origin !== self.location.origin) return;
  const route = routeOf(url.pathname);
  if (route) {
    // Relative links need the trailing slash.
    if (!route.rest) return e.respondWith(Response.redirect(new URL(`${url.pathname}/${url.search}`, url).href, 302));
    return e.respondWith(forward(e.request, route, route.rest + url.search));
  }
  // An absolute path ("/style.css") on a proxied page belongs to its port.
  if (e.clientId) {
    e.respondWith((async () => {
      const client = await self.clients.get(e.clientId);
      const route = client && routeOf(new URL(client.url).pathname);
      return route ? forward(e.request, route, url.pathname + url.search) : fetch(e.request);
    })());
  }
});

async function forward(req, { host, port }, path) {
  const body = req.method === "GET" || req.method === "HEAD" ? null : await req.arrayBuffer();
  const msg = { type: "tabernetes-proxy", port, method: req.method, path, headers: [...req.headers], body };
  let r;
  try {
    r = await ask(host, msg);
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
      headers.set("location", `${scope}${encodeURIComponent(host)}/${u.port || 80}${u.pathname}${u.search}${u.hash}`);
    }
  }
  const nullBody = [101, 204, 205, 304].includes(r.status);
  return new Response(nullBody ? null : r.body, { status: r.status, statusText: r.statusText, headers });
}

async function ask(host, msg) {
  const tab = await self.clients.get(host);
  if (!tab || new URL(tab.url).pathname.startsWith(scope)) throw new Error("the Tabernetes terminal tab is closed");
  terminals.add(host);
  return new Promise((resolve, reject) => {
    const { port1, port2 } = new MessageChannel();
    const timer = setTimeout(() => {
      port1.close();
      reject(new Error("no answer from the Tabernetes terminal tab (is the cluster running?)"));
    }, TIMEOUT);
    port1.onmessage = ({ data }) => {
      port1.close();
      clearTimeout(timer);
      resolve(data);
    };
    tab.postMessage(msg, [port2]);
  });
}

function text(status, s) {
  return new Response(s + "\n", { status, headers: { "content-type": "text/plain; charset=utf-8" } });
}
