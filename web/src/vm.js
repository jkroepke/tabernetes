// The container side of the tab node (k8s/tabernetes/v86.go), as
// globalThis.tabernetesVM. Every pod with a container image gets its own
// worker (vmworker.js) with an x86 emulator in it. Stopping the pod ends the
// worker, and with it the VM.

export function vmAPI({ version, workerURL, wasmURL, biosURL, kernelURL, initURL, registry }) {
  const abs = (u) => new URL(u, location.href).href;
  return {
    version, // of v86, for the container runtime of the node
    registry,

    // start pulls the image and boots it. opts: image, container (command,
    // args, env as "NAME=value", workingDir), hostname, ip, gateway, memory
    // (bytes, 0 for automatic). on: pulling(), pulled(info), started(info),
    // console(text), exit(code, message: why the entrypoint did not start),
    // error(message, pull).
    start(opts, on) {
      const worker = new Worker(workerURL);
      const conns = new Map();
      let next = 1;
      worker.onmessage = ({ data: m }) => {
        switch (m.type) {
          case "pulling": on.pulling(); break;
          case "pulled": on.pulled(m); break;
          case "started": on.started(m); break;
          case "console": on.console(m.data); break;
          case "exit": on.exit(m.code, m.message || ""); break;
          case "error": on.error(m.message, !!m.pull); break;
          case "conn": {
            const c = conns.get(m.id);
            if (!c) break;
            if (m.event === "connect") c.onConnect();
            else if (m.event === "data") c.onData(m.data);
            else { conns.delete(m.id); c.onClose(); }
          }
        }
      };
      worker.onerror = (e) => { e.preventDefault(); on.error(e.message || "the VM worker failed", false); };
      worker.postMessage({
        type: "start",
        ...opts,
        registry,
        version,
        wasmURL: abs(wasmURL),
        biosURL: abs(biosURL),
        kernelURL: abs(kernelURL),
        initURL: abs(initURL),
      });
      return {
        // connect opens a TCP connection to port of the pod.
        connect(port, onConnect, onData, onClose) {
          const id = next++;
          conns.set(id, { onConnect, onData, onClose });
          worker.postMessage({ type: "connect", id, port });
          return {
            write(data) {
              const b = data.slice();
              worker.postMessage({ type: "write", id, data: b }, [b.buffer]);
            },
            close() {
              if (conns.delete(id)) worker.postMessage({ type: "close", id });
            },
          };
        },
        stop() {
          worker.terminate();
          for (const c of conns.values()) c.onClose();
          conns.clear();
        },
      };
    },
  };
}
