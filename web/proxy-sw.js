// Service worker behind the "proxy <port>" command of the kwasm terminal.
//
// A tab on /kwasm-proxy/<port>/... shows what listens on localhost:<port>
// inside the WebAssembly of the terminal tab, typically kubectl
// port-forward. A web page cannot open a TCP port, so this worker hands each
// request to the terminal tab, which runs it against its in-process network
// and posts the response back.
const scope = new URL(self.registration.scope).pathname; // "/kwasm-proxy/"
const TIMEOUT = 60_000;

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

// The terminal tab says hello when it runs "proxy". The worker may be
// stopped and started again, so this is only the first guess.
let host = null;
self.addEventListener("message", (e) => {
  if (e.data?.type === "kwasm-host") host = e.source.id;
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
    // Relative links need the trailing slash: /kwasm-proxy/80 -> /kwasm-proxy/80/
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
  const msg = { type: "kwasm-proxy", port, method: req.method, path, headers: [...req.headers], body };
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

// ask sends the request to the terminal tab. Without a known one, every
// window of the site gets it and the first kwasm terminal answers.
async function ask(msg) {
  let tabs = [];
  const known = host && (await self.clients.get(host));
  if (known) tabs = [known];
  else {
    const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    tabs = all.filter((c) => !new URL(c.url).pathname.startsWith(scope));
  }
  if (!tabs.length) throw new Error("the kwasm terminal tab is closed");
  const replies = tabs.map((tab) => new Promise((resolve) => {
    const { port1, port2 } = new MessageChannel();
    port1.onmessage = (m) => {
      if (tab.id !== host && !m.data.error) host = tab.id;
      resolve(m.data);
    };
    tab.postMessage(msg, [port2]);
  }));
  const timeout = new Promise((_, reject) => setTimeout(() => reject(new Error(
    "no answer from the kwasm terminal tab (is the cluster running?)")), TIMEOUT));
  return Promise.race([...replies, timeout]);
}

function text(status, s) {
  return new Response(s + "\n", { status, headers: { "content-type": "text/plain; charset=utf-8" } });
}
