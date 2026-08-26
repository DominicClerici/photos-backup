import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  ACCEPT_ATTRIBUTE,
  MAX_UPLOAD_BYTES,
  describeDuplicate,
  extensionOf,
  inspect,
  kindOf,
  labelFor,
} from "./upload.ts";

/** A stand-in for the parts of File this module reads. */
const file = (name: string, size: number) => ({ name, size }) as File;

describe("extensionOf", () => {
  it("lowercases what it finds", () => {
    assert.equal(extensionOf("IMG_8071.HEIC"), ".heic");
    assert.equal(extensionOf("clip.MP4"), ".mp4");
  });

  it("reads the last one, not the first", () => {
    assert.equal(extensionOf("holiday.2019.edited.jpg"), ".jpg");
  });

  it("finds none where there is none", () => {
    // A dotfile is not a file with an extension, and neither is a name that
    // merely ends in a dot.
    assert.equal(extensionOf("IMG_0001"), "");
    assert.equal(extensionOf(".gitignore"), "");
    assert.equal(extensionOf("trailing."), "");
  });
});

describe("kindOf", () => {
  it("sorts the formats the archive stores", () => {
    assert.equal(kindOf("a.heic"), "image");
    assert.equal(kindOf("a.DNG"), "image");
    assert.equal(kindOf("a.mov"), "video");
    assert.equal(kindOf("a.webm"), "video");
  });

  it("refuses everything else", () => {
    assert.equal(kindOf("budget.csv"), null);
    assert.equal(kindOf("archive.zip"), null);
    // Adjacent to a format it does take, which is the mistake worth catching.
    assert.equal(kindOf("scan.pdf"), null);
    assert.equal(kindOf("IMG_0001"), null);
  });
});

describe("inspect", () => {
  it("passes an ordinary photograph", () => {
    assert.equal(inspect(file("IMG_8071.HEIC", 3_400_000)), null);
  });

  it("calls an empty file empty rather than unsupported", () => {
    // Both are true of a zero-byte .zip. Only one sends somebody looking in the
    // right place.
    const rejected = inspect(file("archive.zip", 0));
    assert.equal(rejected?.code, "empty");
  });

  it("names the format it will not take", () => {
    const rejected = inspect(file("budget.csv", 900));
    assert.equal(rejected?.code, "unsupported");
    assert.match(rejected!.reason, /CSV/);
  });

  it("says what to do about a file over the limit", () => {
    const rejected = inspect(file("long.mov", MAX_UPLOAD_BYTES + 1));
    assert.equal(rejected?.code, "too-large");
    assert.match(rejected!.reason, /phone app/);
    // Exactly the limit is not over it.
    assert.equal(inspect(file("long.mov", MAX_UPLOAD_BYTES)), null);
  });
});

describe("labelFor", () => {
  it("is the extension a row shows", () => {
    assert.equal(labelFor("IMG_8071.HEIC"), "HEIC");
    assert.equal(labelFor("IMG_0001"), "");
  });
});

describe("describeDuplicate", () => {
  it("names the file when the archive would say which", () => {
    assert.match(describeDuplicate("library", "IMG_8071.HEIC"), /IMG_8071\.HEIC/);
    assert.match(describeDuplicate("library"), /Already in the library\./);
  });

  it("sends a trashed duplicate to the trash rather than back through here", () => {
    assert.match(describeDuplicate("trash"), /Restore it/);
  });

  it("says a purge was a decision", () => {
    assert.match(describeDuplicate("purged"), /purpose/);
  });
});

describe("ACCEPT_ATTRIBUTE", () => {
  it("lists extensions, because a HEIC's MIME type is not dependable", () => {
    assert.ok(ACCEPT_ATTRIBUTE.includes(".heic"));
    assert.ok(!ACCEPT_ATTRIBUTE.includes("image/*"));
  });
});
