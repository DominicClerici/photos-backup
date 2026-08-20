// The report is the one thing on the status page that leaves the browser, and
// the only place its wording can be checked before somebody pastes it into a
// chat window and finds out it left out which file failed.

import { test } from "node:test";
import assert from "node:assert/strict";

import { reportOneFailure, reportStatus } from "./report.ts";
import type { Failure, Status } from "./api.ts";

const GB = 1_000_000_000;

function failure(over: Partial<Failure> = {}): Failure {
  return {
    id: 1421,
    kind: "playback",
    asset_id: "3f2a0c48-0000-4000-8000-000000000000",
    attempts: 5,
    error: "ffmpeg: exit status 234",
    failed_at: "2026-08-20T10:12:00Z",
    filename: "IMG_8071.MOV",
    media_kind: "video",
    viewable: true,
    ...over,
  };
}

function status(over: Partial<Status> = {}): Status {
  return {
    library: { items: 12_481, photos: 9_812, videos: 2_669, trashed: 0 },
    storage: {
      archive: { path: "/mnt/photos", total: 500 * GB, used: 120 * GB, free: 380 * GB },
      derivatives: { path: "/var/lib/photod/derivatives", total: 900 * GB, used: 40 * GB, free: 860 * GB },
      same_volume: false,
      photos: 60 * GB,
      videos: 45 * GB,
      photo_derivatives: 3 * GB,
      video_derivatives: 8 * GB,
      unattributed_derivatives: 0,
      measured_at: "2026-08-20T10:00:00Z",
    },
    queue: { pending: 12, running: 2, failed: 1, kinds: [{ kind: "playback", state: "failed", count: 1 }] },
    problems: [],
    failures: [failure()],
    ...over,
  };
}

test("a failure report names the file, the job and the error verbatim", () => {
  const text = reportOneFailure(failure());
  assert.match(text, /IMG_8071\.MOV/);
  assert.match(text, /job: 1421 \(playback\), 5 attempts/);
  assert.match(text, /3f2a0c48-0000-4000-8000-000000000000/);
  // Fenced, so a stack trace or a shell command in the error survives being
  // pasted into Markdown.
  assert.match(text, /```\nffmpeg: exit status 234\n```/);
});

test("a nameless asset is said to be nameless rather than left blank", () => {
  const text = reportOneFailure(failure({ filename: undefined }));
  assert.match(text, /an asset with no name on record/);
  assert.doesNotMatch(text, /—\s*$/m);
});

test("the whole report carries the numbers that tell one cause from another", () => {
  const text = reportStatus(status(), new Date("2026-08-20T10:15:00Z"));
  assert.match(text, /Taken 2026-08-20T10:15:00\.000Z/);
  assert.match(text, /12,481 items \(9,812 photos, 2,669 videos\)/);
  assert.match(text, /12 pending, 2 running, 1 failed/);
  assert.match(text, /120 GB used of 500 GB on \/mnt\/photos/);
  assert.match(text, /## Failed jobs \(1\)/);
});

test("a healthy server produces a report that says so", () => {
  const text = reportStatus(status({ failures: [], problems: [] }), new Date());
  assert.match(text, /Nothing is failing\./);
  assert.doesNotMatch(text, /## Failed jobs/);
});

test("server problems are reported with their severity", () => {
  const text = reportStatus(
    status({
      problems: [
        {
          id: "worker-disabled",
          severity: "error",
          title: "Derivative workers are off",
          detail: "WORKER_DISABLED is set on this server.",
        },
      ],
    }),
    new Date(),
  );
  assert.match(text, /### Derivative workers are off \(error\)/);
  assert.match(text, /WORKER_DISABLED is set on this server\./);
});
