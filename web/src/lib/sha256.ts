/**
 * SHA-256, incrementally, in script.
 *
 * The archive is content-addressed by this digest, so a browser that can
 * produce it can ask "have you already got this?" before sending a single byte
 * — which is the difference between a duplicate being a row on the upload page
 * and a duplicate being the result of a four-hundred-megabyte transfer.
 *
 * Written out rather than delegated to `crypto.subtle` for two reasons, and the
 * second is the one that settles it:
 *
 *  1. `crypto.subtle.digest` has no streaming form. It takes one buffer, which
 *     means holding an entire video in memory to hash it.
 *  2. `crypto.subtle` only exists in a secure context. The gallery is served
 *     over plain http, and reaching it at http://nas.local:3000 rather than
 *     through localhost — which is exactly what somebody does when they want to
 *     upload from a laptop to the machine holding the archive — leaves
 *     `window.crypto.subtle` undefined. A digest that stops working depending
 *     on which hostname the same page was opened under is not one to build a
 *     duplicate check on.
 *
 * Feed it slices in any sizes; the block boundaries are its own problem.
 */

// The first 32 bits of the fractional parts of the cube roots of the first 64
// primes, which is where SHA-256's round constants come from.
const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

const HEX = Array.from({ length: 256 }, (_, i) => i.toString(16).padStart(2, "0"));

/** How many bytes one 2**32 counts as, for splitting the length into two words. */
const BYTES_PER_HIGH_WORD = 0x20000000; // 2**32 / 8

export class Sha256 {
  private readonly h = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  /** The message schedule, reused across blocks rather than reallocated. */
  private readonly w = new Uint32Array(64);
  /** Bytes carried over from the last update that did not fill a block. */
  private readonly tail = new Uint8Array(64);
  private held = 0;
  private length = 0;
  private done = false;

  update(bytes: Uint8Array): this {
    if (this.done) throw new Error("sha256: update after digest");
    this.length += bytes.length;

    let at = 0;
    if (this.held > 0) {
      const wanted = Math.min(64 - this.held, bytes.length);
      this.tail.set(bytes.subarray(0, wanted), this.held);
      this.held += wanted;
      at = wanted;
      if (this.held < 64) return this;
      this.compress(this.tail, 0);
      this.held = 0;
    }

    for (; at + 64 <= bytes.length; at += 64) this.compress(bytes, at);

    if (at < bytes.length) {
      this.tail.set(bytes.subarray(at), 0);
      this.held = bytes.length - at;
    }
    return this;
  }

  /** The digest as lowercase hex. The instance is spent afterwards. */
  hex(): string {
    if (this.done) throw new Error("sha256: digest called twice");
    this.done = true;

    // The padding: a 1 bit, zeroes, and the length in bits as a big-endian
    // 64-bit integer. Two blocks when the length would not fit in this one.
    const pad = new Uint8Array(this.held < 56 ? 64 : 128);
    pad.set(this.tail.subarray(0, this.held), 0);
    pad[this.held] = 0x80;

    const view = new DataView(pad.buffer);
    view.setUint32(pad.length - 8, Math.floor(this.length / BYTES_PER_HIGH_WORD));
    // ToUint32 wraps, which is the modulo the low word wants.
    view.setUint32(pad.length - 4, this.length * 8);

    for (let at = 0; at < pad.length; at += 64) this.compress(pad, at);

    let out = "";
    for (const word of this.h) {
      out +=
        HEX[(word >>> 24) & 0xff] +
        HEX[(word >>> 16) & 0xff] +
        HEX[(word >>> 8) & 0xff] +
        HEX[word & 0xff];
    }
    return out;
  }

  private compress(block: Uint8Array, at: number): void {
    const w = this.w;
    for (let i = 0; i < 16; i++) {
      const j = at + i * 4;
      w[i] = (block[j] << 24) | (block[j + 1] << 16) | (block[j + 2] << 8) | block[j + 3];
    }
    for (let i = 16; i < 64; i++) {
      const a = w[i - 15];
      const b = w[i - 2];
      const s0 = ((a >>> 7) | (a << 25)) ^ ((a >>> 18) | (a << 14)) ^ (a >>> 3);
      const s1 = ((b >>> 17) | (b << 15)) ^ ((b >>> 19) | (b << 13)) ^ (b >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
    }

    const h = this.h;
    let a = h[0];
    let b = h[1];
    let c = h[2];
    let d = h[3];
    let e = h[4];
    let f = h[5];
    let g = h[6];
    let hh = h[7];

    for (let i = 0; i < 64; i++) {
      const s1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const ch = (e & f) ^ (~e & g);
      const t1 = (hh + s1 + ch + K[i] + w[i]) | 0;
      const s0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (s0 + maj) | 0;

      hh = g;
      g = f;
      f = e;
      e = (d + t1) | 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) | 0;
    }

    h[0] = (h[0] + a) | 0;
    h[1] = (h[1] + b) | 0;
    h[2] = (h[2] + c) | 0;
    h[3] = (h[3] + d) | 0;
    h[4] = (h[4] + e) | 0;
    h[5] = (h[5] + f) | 0;
    h[6] = (h[6] + g) | 0;
    h[7] = (h[7] + hh) | 0;
  }
}

/** The digest of one buffer, for callers with nothing to stream. */
export function sha256(bytes: Uint8Array): string {
  return new Sha256().update(bytes).hex();
}
