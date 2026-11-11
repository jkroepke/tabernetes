// initramfs builds the initramfs of a pod VM: an uncompressed cpio archive
// (newc) with the init of the pod (vm/init) as /init. The kernel starts it
// first; it waits there for the pod (vm/init/init.c).

const enc = new TextEncoder();

function header(name, mode, size, ino) {
  const f = [ino, mode, 0, 0, 1, 0, size, 0, 0, 0, 0, name.length + 1, 0];
  return enc.encode("070701" + f.map((n) => n.toString(16).padStart(8, "0")).join(""));
}

export function initramfs(init) {
  const entries = [
    ["dev", 0o040755, null],
    ["newroot", 0o040755, null],
    ["init", 0o100755, init],
    ["TRAILER!!!", 0, null],
  ];
  const parts = [];
  let size = 0;
  const push = (b) => { parts.push(b); size += b.length; };
  const pad = () => { if (size % 4) push(new Uint8Array(4 - (size % 4))); };
  entries.forEach(([name, mode, data], i) => {
    push(header(name, mode, data ? data.length : 0, i + 1));
    push(enc.encode(name + "\0"));
    pad();
    if (data) { push(data); pad(); }
  });
  const out = new Uint8Array(size);
  let at = 0;
  for (const p of parts) { out.set(p, at); at += p.length; }
  return out;
}
