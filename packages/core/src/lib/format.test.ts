import { describe, it, test } from "node:test";
import assert from "node:assert/strict";

import { describeAction, formatSince, nounFor } from "./format.ts";

// The labels are the feature's most visible surface: every menu item and every
// button in the gallery is one of these strings, and getting the count or the
// noun wrong is the kind of bug somebody reads a hundred times a day.

describe("describeAction", () => {
  it("names one photograph by what it is", () => {
    assert.equal(
      describeAction("Archive", { kind: "items", count: 1, noun: nounFor("image") }),
      "Archive photo",
    );
    assert.equal(
      describeAction("Hide", { kind: "items", count: 1, noun: nounFor("video") }),
      "Hide video",
    );
  });

  it("calls several of them items, whatever they are", () => {
    // Not "12 photos": a selection of eleven photographs and one video is
    // neither eleven nor twelve photos, and the grid cannot know the mix
    // without fetching all of it.
    assert.equal(
      describeAction("Archive", { kind: "items", count: 12, noun: nounFor("video") }),
      "Archive 12 items",
    );
    assert.equal(
      describeAction("Delete", { kind: "items", count: 2 }),
      "Delete 2 items",
    );
  });

  it("groups thousands the way the rest of the gallery does", () => {
    assert.equal(
      describeAction("Delete", { kind: "items", count: 11_482 }),
      "Delete 11,482 items",
    );
  });

  it("calls an album an album, not by its title", () => {
    // "Archive Iceland 2025" reads like a sentence about Iceland.
    assert.equal(describeAction("Archive", { kind: "album" }), "Archive album");
    assert.equal(describeAction("Delete", { kind: "album" }), "Delete album");
  });

  it("calls a person by their name, which is the opposite call", () => {
    assert.equal(
      describeAction("Hide", { kind: "person", name: "Brody" }),
      "Hide Brody",
    );
    assert.equal(
      describeAction("Unarchive", { kind: "person", name: "Brody" }),
      "Unarchive Brody",
    );
  });

  it("falls back to the generic noun when nothing more specific is known", () => {
    // The selection sheet has no tile under a pointer to ask.
    assert.equal(describeAction("Archive", { kind: "items", count: 1 }), "Archive photo");
  });
});

test("how long ago picks the unit somebody would say out loud", () => {
  const now = new Date("2026-08-20T12:00:00Z");
  const since = (iso: string) => formatSince(iso, now);

  assert.equal(since("2026-08-20T11:59:40Z"), "just now");
  assert.equal(since("2026-08-20T11:56:00Z"), "4 minutes ago");
  assert.equal(since("2026-08-20T09:00:00Z"), "3 hours ago");
  assert.equal(since("2026-08-19T12:00:00Z"), "yesterday");
  assert.equal(since("2026-07-20T12:00:00Z"), "last month");
  // A job whose row says something no Date can be made of should not put
  // "Invalid Date" on the page.
  assert.equal(since("not a time"), "at an unknown time");
});
