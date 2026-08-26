import type { ContentWhere } from "./api.ts";

/**
 * What the upload page will and will not send, decided before anything moves.
 *
 * The rules are the browser's half of a pair. photod refuses a file whose name
 * and whose leading bytes both fail to identify it as a photograph or a video —
 * see internal/api/galleryupload.go — and that is the check that actually
 * protects the archive, because it reads the file. This one reads a filename
 * and a length, which is all a `File` offers before it is opened, and it exists
 * so that dragging a folder of holiday photos and one stray .zip does not mean
 * uploading the zip to find out.
 *
 * So the two disagree in one direction on purpose: a file with no extension is
 * rejected here and would have been accepted there, because a Google Takeout
 * Live Photo video is the only thing that arrives that way and it does not
 * arrive through a browser. Nothing this accepts is refused there for its type.
 */

/** The extensions photod knows how to name a blob by. */
const IMAGE_EXTENSIONS = [
  ".heic",
  ".heif",
  ".jpg",
  ".jpeg",
  ".png",
  ".gif",
  ".webp",
  ".tif",
  ".tiff",
  ".dng",
] as const;

const VIDEO_EXTENSIONS = [".mov", ".mp4", ".m4v", ".avi", ".webm"] as const;

export const ACCEPTED_EXTENSIONS: readonly string[] = [
  ...IMAGE_EXTENSIONS,
  ...VIDEO_EXTENSIONS,
];

/**
 * The `accept` attribute for the file picker.
 *
 * Extensions rather than `image/*,video/*`, and deliberately: a browser's idea
 * of a HEIC's MIME type is inconsistent enough that the wildcard hides half an
 * iPhone's camera roll behind "All files" in the system dialog. The picker's
 * filter is a convenience anyway — everything it lets through is checked here.
 */
export const ACCEPT_ATTRIBUTE = ACCEPTED_EXTENSIONS.join(",");

/**
 * The largest file this page will send, which is not the largest file the
 * archive will hold.
 *
 * A browser upload is one request with no resumable form: there is no session
 * to come back to, because closing the tab takes the picker's selection with
 * it. That makes an eight-gigabyte video a single transfer that has to survive
 * from beginning to end or start again from nothing, and the app on the phone —
 * which chunks, resumes, and retries — is the right way to move one. Two
 * gigabytes is comfortably above every still and most videos anybody drags onto
 * a web page, and below the size where "start again" is a real cost.
 */
export const MAX_UPLOAD_BYTES = 2 * 1024 * 1024 * 1024;

/**
 * Why a file is not going to be sent.
 *
 * `unreadable` is the only one this module does not decide: it is a file the
 * picker handed over and the disk then would not produce — moved, unmounted, or
 * on a drive that went to sleep — and it is discovered while hashing rather than
 * while inspecting.
 */
export type RejectionCode =
  | "empty"
  | "too-large"
  | "unsupported"
  | "unreadable"
  | "duplicate-in-batch";

export interface Rejection {
  code: RejectionCode;
  /** A sentence for the row, written to the person who dropped the file. */
  reason: string;
}

export type FileKind = "image" | "video";

/** The extension, lowercased and including its dot, or "" when there is none. */
export function extensionOf(filename: string): string {
  const at = filename.lastIndexOf(".");
  // A leading dot is a hidden file rather than an extension, and a dot in a
  // directory name is not one either.
  if (at <= 0 || at === filename.length - 1) return "";
  return filename.slice(at).toLowerCase();
}

/** What the archive will file this as, or null when it will not take it. */
export function kindOf(filename: string): FileKind | null {
  const ext = extensionOf(filename);
  if ((IMAGE_EXTENSIONS as readonly string[]).includes(ext)) return "image";
  if ((VIDEO_EXTENSIONS as readonly string[]).includes(ext)) return "video";
  return null;
}

/** The extension in the form the row shows it: HEIC, MP4, or nothing. */
export function labelFor(filename: string): string {
  return extensionOf(filename).replace(".", "").toUpperCase();
}

/**
 * Whether this file can be sent at all, and if not, what to tell somebody.
 *
 * Checked in the order the answers are useful. A zero-byte file is almost
 * always a copy that did not finish, and saying "unsupported" about it because
 * it is also called `.zip` would send whoever dropped it looking for the wrong
 * problem.
 */
export function inspect(file: File): Rejection | null {
  if (file.size === 0) {
    return { code: "empty", reason: "The file is empty — 0 bytes." };
  }
  if (file.size > MAX_UPLOAD_BYTES) {
    return {
      code: "too-large",
      reason: `Larger than the ${formatLimit(MAX_UPLOAD_BYTES)} web upload limit. Send this one from the phone app.`,
    };
  }
  if (kindOf(file.name) === null) {
    const ext = extensionOf(file.name);
    return {
      code: "unsupported",
      reason: ext
        ? `${ext.slice(1).toUpperCase()} is not a photo or video format this archive stores.`
        : "No file extension, so there is no telling what this is.",
    };
  }
  return null;
}

/** The size limit as the sentence above wants it: "2 GB", not "2.1 GB". */
function formatLimit(bytes: number): string {
  return `${bytes / 1024 / 1024 / 1024} GB`;
}

/**
 * Why a duplicate is worth a different sentence in each case.
 *
 * All four mean "these exact bytes are already accounted for", and what to do
 * about it is different every time: one is already on the timeline, one is a
 * restore away, one is somewhere the gallery will not say, and one was thrown
 * away on purpose — and re-uploading that last one would undo a decision.
 *
 * @param filename What the archive stored it as, when that is worth saying.
 * The caller leaves it out when it matches the file being dropped — "already
 * in the library as beach.jpg" on a row headed beach.jpg is a word salad.
 */
export function describeDuplicate(where: ContentWhere, filename?: string): string {
  const named = filename ? ` as ${filename}` : "";
  switch (where) {
    case "library":
      return `Already in the library${named}.`;
    case "trash":
      return "Already in the archive, in Recently Deleted. Restore it from the trash rather than uploading it again.";
    case "vault":
      return "Already in the archive, in the vault.";
    case "purged":
      return "These bytes were purged from the archive on purpose. Uploading them would undo that.";
  }
}
