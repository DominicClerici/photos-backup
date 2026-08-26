import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash, randomBytes } from "node:crypto";

import { Sha256, sha256 } from "./sha256.ts";

const bytes = (s: string) => new TextEncoder().encode(s);
const reference = (b: Uint8Array) => createHash("sha256").update(b).digest("hex");

// This is the archive's own content key computed somewhere the archive cannot
// check it, so the whole test is "does it agree with a real SHA-256" — and the
// cases are the ones a hand-written implementation gets wrong: the empty input,
// the block boundary, the two-block padding, and a length that needs both
// halves of the 64-bit counter.

describe("Sha256", () => {
  it("matches the published vectors", () => {
    assert.equal(
      sha256(bytes("")),
      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    );
    assert.equal(
      sha256(bytes("abc")),
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    );
    assert.equal(
      sha256(bytes("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq")),
      "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
    );
  });

  it("pads correctly either side of the block boundary", () => {
    // 55 bytes leaves exactly room for the terminator and the length; 56 does
    // not, and needs a second block. 64 is a whole block with nothing left.
    for (const n of [54, 55, 56, 57, 63, 64, 65, 119, 120, 128]) {
      const input = randomBytes(n);
      assert.equal(sha256(input), reference(input), `${n} bytes`);
    }
  });

  it("does not care how the bytes were divided up", () => {
    const input = randomBytes(5000);
    const whole = sha256(input);

    for (const chunk of [1, 7, 63, 64, 65, 1000, 4096]) {
      const digest = new Sha256();
      for (let at = 0; at < input.length; at += chunk) {
        digest.update(input.subarray(at, at + chunk));
      }
      assert.equal(digest.hex(), whole, `${chunk}-byte chunks`);
    }
  });

  it("counts a length past 2**29 bytes into the high word", () => {
    // The low word of the bit count wraps at 512MB of input, which is a size a
    // video in this archive genuinely reaches. Rather than hash half a gigabyte
    // here, the counter is advanced directly and the padding checked against
    // what a real implementation writes for the same length.
    const digest = new Sha256();
    const block = new Uint8Array(64);
    const fed = 0x20000000 + 64; // one byte past the wrap, in whole blocks
    for (let at = 0; at < fed; at += 64) digest.update(block);

    const expected = createHash("sha256");
    for (let at = 0; at < fed; at += 64) expected.update(block);
    assert.equal(digest.hex(), expected.digest("hex"));
  });

  it("refuses to be used twice", () => {
    const digest = new Sha256().update(bytes("abc"));
    digest.hex();
    assert.throws(() => digest.hex(), /twice/);
    assert.throws(() => digest.update(bytes("abc")), /after digest/);
  });
});
