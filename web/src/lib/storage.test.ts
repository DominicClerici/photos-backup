// The breakdown is the only part of the storage card that can be wrong in a way
// nobody notices: a pie with a gap in it still draws, and a slice charged to
// the wrong disk still adds up to something plausible.

import { test } from "node:test";
import assert from "node:assert/strict";

import { breakdown, percentUsed } from "./storage.ts";
import type { StorageStatus } from "./api.ts";

const GB = 1_000_000_000;

function status(over: Partial<StorageStatus> = {}): StorageStatus {
  return {
    archive: { path: "/mnt/photos", total: 500 * GB, used: 120 * GB, free: 380 * GB },
    derivatives: { path: "/var/lib/photod/derivatives", total: 900 * GB, used: 40 * GB, free: 860 * GB },
    same_volume: false,
    photos: 60 * GB,
    videos: 45 * GB,
    photo_derivatives: 3 * GB,
    video_derivatives: 8 * GB,
    unattributed_derivatives: 0,
    measured_at: "2026-08-20T10:00:00Z",
    ...over,
  };
}

const bytesOf = (rows: { key: string; bytes: number }[], key: string) =>
  rows.find((r) => r.key === key)?.bytes;

test("what the archive cannot account for is a row rather than a rounding", () => {
  const b = breakdown(status());
  // 120 used, 105 in photographs: the remaining 15 is the database, the vault
  // and the reserved blocks, and the card says so out loud.
  assert.equal(bytesOf(b.rows, "other"), 15 * GB);
  assert.equal(
    b.rows.reduce((sum, r) => sum + r.bytes, 0),
    b.used,
  );
});

test("derivatives on another disk are listed apart from the pie", () => {
  const b = breakdown(status());
  assert.equal(bytesOf(b.rows, "photo_derivatives"), undefined);
  assert.equal(bytesOf(b.elsewhere, "video_derivatives"), 8 * GB);
  assert.equal(b.elsewherePath, "/var/lib/photod/derivatives");
});

test("derivatives on the same disk are slices of it", () => {
  const b = breakdown(status({ same_volume: true }));
  assert.equal(bytesOf(b.rows, "photo_derivatives"), 3 * GB);
  assert.deepEqual(b.elsewhere, []);
  // The same 120GB, now with 116 of it named.
  assert.equal(bytesOf(b.rows, "other"), 4 * GB);
});

test("a drive fuller than the archive can explain never shows a negative slice", () => {
  const b = breakdown(status({ photos: 200 * GB }));
  assert.equal(bytesOf(b.rows, "other"), 0);
});

test("an unreadable volume reads as empty rather than as NaN", () => {
  const b = breakdown(status({ archive: { path: "", total: 0, used: 0, free: 0 } }));
  assert.equal(percentUsed(b.total, b.used), 0);
  assert.equal(bytesOf(b.rows, "other"), 0);
});
