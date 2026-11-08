// Web Worker: unpacks one zstd frame of tabernetes.wasm.zst per message. The page
// runs a few of these, so the frames unpack in parallel and off the main
// thread while the rest of the binary still downloads.
import { decompress } from "fzstd";

onmessage = ({ data }) => {
  try {
    let out = decompress(data);
    if (out.byteOffset || out.byteLength !== out.buffer.byteLength) out = out.slice();
    postMessage({ out }, [out.buffer]);
  } catch (e) {
    postMessage({ error: `zstd: ${e.message}` });
  }
};
