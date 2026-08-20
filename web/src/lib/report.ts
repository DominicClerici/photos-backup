// Types only from the wire client, which reaches for the browser on the way in.
// The extension on format is not decoration: this module is tested by node's
// own runner, which resolves what it imports for real.
import type { Failure, Problem, Status } from "./api";
import { formatBytes } from "./format.ts";

/**
 * What goes on the clipboard.
 *
 * The status page's other half: it can tell you a transcode failed, and the
 * fix for that is almost never in the browser — it is in the server, and it is
 * going to be read by somebody (or something) that was not looking at this
 * page. So the button hands over a report that stands on its own: what broke,
 * which file, how many times, and the error verbatim, in Markdown because that
 * is what a chat window and a code agent both read.
 *
 * Pure string-building, kept out of the components so it can be tested and so
 * the wording is decided in one place rather than three.
 */

/** One failed job, as a section of a report. */
export function reportFailure(f: Failure): string {
  const name = f.filename || "an asset with no name on record";
  const lines = [
    `### ${f.kind} job failed — ${name}`,
    "",
    `- job: ${f.id} (${f.kind}), ${f.attempts} ${f.attempts === 1 ? "attempt" : "attempts"}`,
    `- asset: ${f.asset_id}${f.media_kind ? ` (${f.media_kind})` : ""}`,
    `- last failed: ${f.failed_at}`,
    "",
    "```",
    f.error || "(the job recorded no error text)",
    "```",
  ];
  return lines.join("\n");
}

function reportProblem(p: Problem): string {
  return [`### ${p.title} (${p.severity})`, "", p.detail].join("\n");
}

/**
 * The whole page, for handing to somebody who cannot see it.
 *
 * The summary at the top is not decoration: a queue of forty thousand and a
 * queue of four are the same failure message with completely different causes,
 * and the reader has no other way to tell which one they are looking at.
 */
export function reportStatus(status: Status, at: Date): string {
  const { library, queue, storage } = status;
  const out = [
    "# photos-backup status",
    "",
    `Taken ${at.toISOString()}`,
    "",
    `- library: ${library.items.toLocaleString()} items (${library.photos.toLocaleString()} photos, ${library.videos.toLocaleString()} videos)`,
    `- queue: ${queue.pending.toLocaleString()} pending, ${queue.running.toLocaleString()} running, ${queue.failed.toLocaleString()} failed`,
    `- storage: ${formatBytes(storage.archive.used)} used of ${formatBytes(storage.archive.total)} on ${storage.archive.path || "the archive volume"}`,
  ];

  if (queue.kinds.length > 0) {
    const byKind = queue.kinds.map((c) => `${c.kind}/${c.state}: ${c.count}`).join(", ");
    out.push(`- jobs: ${byKind}`);
  }

  if (status.problems.length > 0) {
    out.push("", "## Server problems", "");
    out.push(status.problems.map(reportProblem).join("\n\n"));
  }

  if (status.failures.length > 0) {
    out.push("", `## Failed jobs (${status.failures.length})`, "");
    out.push(status.failures.map(reportFailure).join("\n\n"));
  }

  if (status.problems.length === 0 && status.failures.length === 0) {
    out.push("", "Nothing is failing.");
  }

  return out.join("\n") + "\n";
}

/** One server problem, for the copy button on its own card. */
export function reportOneProblem(p: Problem): string {
  return ["# photos-backup problem", "", reportProblem(p), ""].join("\n");
}

/** One failure, for the copy button on its own row. */
export function reportOneFailure(f: Failure): string {
  return ["# photos-backup failed job", "", reportFailure(f), ""].join("\n");
}
