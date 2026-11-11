import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { MessageChannel } from "node:worker_threads";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./proxy-sw.js", import.meta.url), "utf8");
const origin = "https://example.test";
const scope = "/tabernetes-proxy/";

function worker(clients) {
  const handlers = new Map();
  vm.runInNewContext(source, {
    URL, Headers, Response, MessageChannel, setTimeout, clearTimeout,
    fetch() { throw new Error("unexpected network request"); },
    self: {
      location: { origin },
      registration: { scope: origin + scope, unregister() {} },
      clients: { get: async (id) => clients.get(id) },
      addEventListener(type, fn) { handlers.set(type, fn); },
    },
  });
  return {
    hello(id) {
      let reply;
      handlers.get("message")({ source: { id }, data: { type: "tabernetes-host" }, ports: [{ postMessage(data) { reply = data; } }] });
      assert.equal(reply.id, id);
    },
    async fetch(path, clientId = "") {
      let response;
      handlers.get("fetch")({ request: new Request(origin + path), clientId, respondWith(value) { response = value; } });
      assert.ok(response, "the worker must handle the request");
      return response;
    },
  };
}

function terminal(id, headers = []) {
  return {
    id, url: origin + "/tabernetes/",
    postMessage(message, [reply]) {
      reply.postMessage({ status: 200, statusText: "OK", headers, body: new TextEncoder().encode(id + ":" + message.path) });
      reply.close();
    },
  };
}

test("terminals can proxy the same port without changing each other's routes", async () => {
  const clients = new Map([["first", terminal("first")], ["second", terminal("second")]]);
  const sw = worker(clients);
  sw.hello("first");
  sw.hello("second");
  assert.equal(await (await sw.fetch(scope + "first/8080/")).text(), "first:/");
  assert.equal(await (await sw.fetch(scope + "second/8080/")).text(), "second:/");
  assert.equal(await (await sw.fetch(scope + "first/8080/api")).text(), "first:/api");
});

test("routes and absolute assets survive a worker restart", async () => {
  const clients = new Map([
    ["first", terminal("first")],
    ["page", { url: origin + scope + "first/8080/" }],
  ]);
  const sw = worker(clients);
  assert.equal(await (await sw.fetch(scope + "first/8080/")).text(), "first:/");
  assert.equal(await (await sw.fetch("/style.css", "page")).text(), "first:/style.css");
});

test("a closed terminal does not fall through to another terminal", async () => {
  const sw = worker(new Map([["second", terminal("second")]]));
  const response = await sw.fetch(scope + "first/8080/");
  assert.equal(response.status, 502);
  assert.match(await response.text(), /terminal tab is closed/);
});

test("redirects retain the terminal and the trailing slash", async () => {
  const sw = worker(new Map([["first", terminal("first", [["location", "http://localhost:8080/login"]])]]));
  const response = await sw.fetch(scope + "first/8080/");
  assert.equal(response.headers.get("location"), scope + "first/8080/login");
  const slash = await sw.fetch(scope + "first/8080?q=1");
  assert.equal(slash.headers.get("location"), origin + scope + "first/8080/?q=1");
});
