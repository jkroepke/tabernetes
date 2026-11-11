// SHA-256 (FIPS 180-4) that takes the bytes in chunks. crypto.subtle.digest
// wants all of them at once, so a pull would have to keep a whole layer in
// memory (up to ~1 GB for the big layer of python) just to check its digest.
// This one hashes while the layer streams in, at ~135 MB/s in V8. Keep the
// round in local variables: a version with array destructuring was 2.5x
// slower and made the pull slower than the download.
const K = new Uint32Array([
  0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2,
]);
export class SHA256 {
  h = new Uint32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]);
  w = new Uint32Array(64);
  buf = new Uint8Array(64); // the start of a block that did not fill up yet
  n = 0; // bytes in buf
  len = 0; // bytes so far

  update(d) {
    let i = 0;
    this.len += d.length;
    if (this.n) {
      i = Math.min(64 - this.n, d.length);
      this.buf.set(d.subarray(0, i), this.n);
      this.n += i;
      if (this.n < 64) return this;
      this.#block(this.buf, 0);
      this.n = 0;
    }
    for (; i + 64 <= d.length; i += 64) this.#block(d, i);
    if (i < d.length) {
      this.buf.set(d.subarray(i));
      this.n = d.length - i;
    }
    return this;
  }

  // hex returns the digest as lower-case hex. Call it once, at the end.
  hex() {
    const bits = this.len * 8;
    const pad = new Uint8Array(((this.n + 9 + 63) & ~63) - this.n);
    pad[0] = 0x80;
    const v = new DataView(pad.buffer);
    v.setUint32(pad.length - 8, Math.floor(bits / 2 ** 32));
    v.setUint32(pad.length - 4, bits >>> 0);
    this.update(pad);
    return [...this.h].map((x) => x.toString(16).padStart(8, "0")).join("");
  }

  #block(d, o) {
    const w = this.w, h = this.h;
    for (let t = 0; t < 16; t++, o += 4) w[t] = (d[o] << 24) | (d[o + 1] << 16) | (d[o + 2] << 8) | d[o + 3];
    for (let t = 16; t < 64; t++) {
      const a = w[t - 15], b = w[t - 2];
      w[t] = ((((a >>> 7) | (a << 25)) ^ ((a >>> 18) | (a << 14)) ^ (a >>> 3)) + (((b >>> 17) | (b << 15)) ^ ((b >>> 19) | (b << 13)) ^ (b >>> 10)) + w[t - 7] + w[t - 16]) | 0;
    }
    let A = h[0] | 0, B = h[1] | 0, C = h[2] | 0, D = h[3] | 0, E = h[4] | 0, F = h[5] | 0, G = h[6] | 0, H = h[7] | 0;
    for (let t = 0; t < 64; t++) {
      const t1 = (H + (((E >>> 6) | (E << 26)) ^ ((E >>> 11) | (E << 21)) ^ ((E >>> 25) | (E << 7))) + ((E & F) ^ (~E & G)) + K[t] + w[t]) | 0;
      const t2 = ((((A >>> 2) | (A << 30)) ^ ((A >>> 13) | (A << 19)) ^ ((A >>> 22) | (A << 10))) + ((A & B) ^ (A & C) ^ (B & C))) | 0;
      H = G; G = F; F = E; E = (D + t1) | 0; D = C; C = B; B = A; A = (t1 + t2) | 0;
    }
    h[0] += A; h[1] += B; h[2] += C; h[3] += D; h[4] += E; h[5] += F; h[6] += G; h[7] += H;
  }
}
