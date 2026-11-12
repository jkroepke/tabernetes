// kernelboot starts the Linux kernel of a pod (vm/Dockerfile) in v86 without
// a BIOS, the way Firecracker does it: the page puts the unpacked kernel
// (vmlinux, an ELF file), the initramfs and the boot parameters into the
// memory of the VM and sets the CPU to the entry of the kernel, in 32-bit
// protected mode (the 32-bit boot protocol, Documentation/arch/x86/boot.rst).
//
// With SeaBIOS, v86 boots the BIOS, the BIOS starts the setup code of the
// bzImage in real mode, and the kernel unpacks itself on the emulated CPU.
// The BIOS was cheap; unpacking the kernel was not. Measured with Linux
// 7.3.4 in Node.js, from run() to init ready: ~300 -> ~280 ms with 256 MiB,
// ~400 -> ~330 ms with 2 GiB. A bzImage started without the BIOS was as
// slow as with it.
//
// What the BIOS did and the kernel still needs is done here: the memory map
// (E820) and the routing of the PCI interrupts.

// Where the boot parameters go: below 640 KiB, where the kernel does not
// load anything. The kernel copies them before it uses this memory.
const ZERO_PAGE = 0x10000;
const CMDLINE = 0x20000;
const GDT = 0x1000;

// The boot protocol puts the initramfs at most at initrd_addr_max, and the
// kernel must reach it before it maps high memory.
const INITRD_MAX = 0x37000000;

// Where a BIOS puts its tables. Linux looks for a PCI interrupt routing
// table between 0xf0000 and 0xfffff: in steps of 16 bytes for "$PIR", then
// byte by byte for "$IRT". Without a table that search is 0.3 million
// instructions of every boot (2 % of it).
const PIR = 0xf0000;

// The PCI interrupts as SeaBIOS routes them: PIRQ A-D of the PIIX3 to IRQ
// 10, 10, 11, 11.
const PIRQ = [10, 10, 11, 11];

// bootKernel prepares a VM that v86 created but did not run yet. cpu is
// emulator.v86.cpu, kernel the bytes of vmlinux, initrd those of the
// initramfs.
export function bootKernel(cpu, { kernel, initrd, cmdline }) {
  const mem = cpu.mem8;
  const memory = cpu.memory_size[0] >>> 0;
  const entry = loadELF(mem, kernel);

  // the boot parameters ("zero page") with the setup header at 0x1f1
  const zp = new DataView(new ArrayBuffer(4096));
  zp.setUint16(0x1fe, 0xaa55, true); // boot_flag
  zp.setUint32(0x202, 0x53726448, true); // header: "HdrS"
  zp.setUint16(0x206, 0x020f, true); // version of the protocol
  zp.setUint8(0x210, 0xff); // type_of_loader: unknown
  zp.setUint8(0x211, 0x01); // loadflags: LOADED_HIGH
  const cl = new TextEncoder().encode(cmdline + "\0");
  mem.set(cl, CMDLINE);
  zp.setUint32(0x228, CMDLINE, true); // cmd_line_ptr
  zp.setUint32(0x238, cl.length, true); // cmdline_size
  if (initrd) {
    const at = (Math.min(memory, INITRD_MAX) - initrd.byteLength) & ~0xfff;
    mem.set(initrd, at);
    zp.setUint32(0x218, at, true); // ramdisk_image
    zp.setUint32(0x21c, initrd.byteLength, true); // ramdisk_size
  }
  // E820: RAM below 640 KiB and from 1 MiB on. The rest of the first MiB
  // is not RAM on a PC (VGA memory, ROMs).
  const e820 = [[0, 0x9fc00, 1], [0x100000, memory - 0x100000, 1]];
  e820.forEach(([addr, size, type], i) => {
    zp.setUint32(0x2d0 + i * 20, addr, true);
    zp.setUint32(0x2d0 + i * 20 + 8, size, true);
    zp.setUint32(0x2d0 + i * 20 + 16, type, true);
  });
  zp.setUint8(0x1e8, e820.length); // e820_entries
  mem.set(new Uint8Array(zp.buffer), ZERO_PAGE);

  // The protocol wants flat segments __BOOT_CS (0x10) and __BOOT_DS (0x18).
  // The kernel loads a GDT of its own right away.
  const gdt = new DataView(mem.buffer, mem.byteOffset + GDT, 32);
  gdt.setUint32(16, 0x0000ffff, true);
  gdt.setUint32(20, 0x00cf9a00, true); // code, 4 GiB
  gdt.setUint32(24, 0x0000ffff, true);
  gdt.setUint32(28, 0x00cf9200, true); // data, 4 GiB
  cpu.gdtr_offset[0] = GDT;
  cpu.gdtr_size[0] = 31;

  // protected mode without paging, interrupts off, %esi at the zero page
  cpu.reg32.fill(0);
  cpu.reg32[6] = ZERO_PAGE;
  cpu.cr[0] = 1;
  cpu.protected_mode[0] = 1;
  cpu.flags[0] = 2;
  cpu.is_32[0] = 1;
  cpu.stack_size_32[0] = 1;
  cpu.cpl[0] = 0;
  for (let i = 0; i < 6; i++) { // es, cs, ss, ds, fs, gs
    cpu.segment_is_null[i] = 0;
    cpu.segment_offsets[i] = 0;
    cpu.segment_limits[i] = 0xffffffff;
    cpu.segment_access_bytes[i] = i === 1 ? 0x9a : 0x92;
    cpu.sreg[i] = i === 1 ? 0x10 : 0x18;
  }
  cpu.instruction_pointer[0] = entry;
  cpu.update_state_flags();

  routePCI(cpu.io);
  mem.set(pirTable(), PIR);
}

// pirTable is a PCI interrupt routing table ($PIR) without entries, for the
// PIIX3 at 00:01.0. Linux finds it at the first address it looks at, and
// takes the IRQ of each device from its interrupt line (routePCI).
function pirTable() {
  const t = new Uint8Array(32);
  const v = new DataView(t.buffer);
  t.set([0x24, 0x50, 0x49, 0x52]); // "$PIR"
  v.setUint16(4, 0x0100, true); // version 1.0
  v.setUint16(6, t.length, true); // size: the header, no slots
  t[8] = 0; // bus of the router
  t[9] = 1 << 3; // device 1, function 0
  v.setUint16(12, 0x8086, true); // the router: PIIX3
  v.setUint16(14, 0x7000, true);
  t[31] = -t.reduce((a, b) => a + b, 0) & 0xff; // checksum: all bytes add up to 0
  return t;
}

// loadELF copies the segments of vmlinux to their physical addresses and
// returns the physical address of the entry (startup_32).
function loadELF(mem, kernel) {
  const v = new DataView(kernel.buffer, kernel.byteOffset, kernel.byteLength);
  if (v.getUint32(0) !== 0x7f454c46 || kernel[4] !== 1 || v.getUint16(18, true) !== 3) {
    throw new Error("the kernel is no 32-bit x86 ELF file (vmlinux)");
  }
  const phoff = v.getUint32(28, true), phsize = v.getUint16(42, true), phnum = v.getUint16(44, true);
  for (let i = 0; i < phnum; i++) {
    const p = phoff + i * phsize;
    if (v.getUint32(p, true) !== 1) continue; // PT_LOAD
    const offset = v.getUint32(p + 4, true), paddr = v.getUint32(p + 12, true), size = v.getUint32(p + 16, true);
    mem.set(kernel.subarray(offset, offset + size), paddr);
  }
  // The entry of vmlinux is physical already (ENTRY(phys_startup_32)).
  return v.getUint32(24, true);
}

// routePCI does what SeaBIOS did for the PCI interrupts: it routes PIRQ A-D
// of the PIIX3, writes the IRQ to the interrupt line of every device, and
// makes these IRQs level triggered. Without a BIOS, the routing table has no
// entries, and without the interrupt lines the virtio devices get no IRQ
// ("can't find IRQ for PCI INT A").
// v86 maps pin and slot to a PIRQ like SeaBIOS: (pin - 1 + slot - 1) & 3.
function routePCI(io) {
  const select = (dev, reg) => io.port_write32(0xcf8, (0x80000000 | (dev << 11) | (reg & 0xfc)) >>> 0);
  const read = (dev, reg) => { select(dev, reg); return io.port_read32(0xcfc) >>> 0; };
  const write8 = (dev, reg, value) => { select(dev, reg); io.port_write8(0xcfc + (reg & 3), value); };
  for (let dev = 0; dev < 32; dev++) {
    const id = read(dev, 0);
    if (id === 0xffffffff || id === 0) continue;
    if (id === 0x70008086) PIRQ.forEach((irq, i) => write8(dev, 0x60 + i, irq)); // PIIX3 ISA bridge
    const pin = (read(dev, 0x3c) >> 8) & 0xff;
    if (pin) write8(dev, 0x3c, PIRQ[(pin - 1 + dev - 1) & 3]);
  }
  let elcr = 0;
  for (const irq of PIRQ) elcr |= 1 << irq;
  io.port_write8(0x4d0, elcr & 0xff);
  io.port_write8(0x4d1, elcr >> 8);
}
