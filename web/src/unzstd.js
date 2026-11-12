// Web Worker: unpacks one zstd frame of tabernetes.wasm.zst per message. The page
// runs a few of these, so the frames unpack in parallel and off the main
// thread while the rest of the binary still downloads.
//
// zstddec is the zstd C library compiled to WebAssembly. It unpacks the
// binary about 2.5 times faster than fzstd, which is plain JavaScript.
import { ZSTDDecoder } from "zstddec";

// zstddec loads its WebAssembly with fetch() of a data: URL. The
// Content-Security-Policy of the page (connect-src) may forbid that, so
// such URLs are decoded here instead.
const fetchURL = globalThis.fetch;
globalThis.fetch = (url, ...rest) => {
  if (typeof url === "string" && url.startsWith("data:")) {
    const b64 = url.slice(url.indexOf(",") + 1);
    return Promise.resolve(new Response(Uint8Array.from(atob(b64), (c) => c.charCodeAt(0)), { headers: { "Content-Type": "application/wasm" } }));
  }
  return fetchURL(url, ...rest);
};

const zstd = new ZSTDDecoder();
const ready = zstd.init();

// The messages are answered in order: each one waits for the same promise,
// then unpacks without a pause.
onmessage = async ({ data }) => {
  try {
    await ready;
    // The frame header has the unpacked size (zstdCompressSync writes it).
    const out = zstd.decode(data);
    postMessage({ out }, [out.buffer]);
  } catch (e) {
    postMessage({ error: `zstd: ${e?.message || e}` });
  }
};
