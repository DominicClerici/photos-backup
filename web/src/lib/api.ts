// The gallery's client, as this app sees it.
//
// The file itself is packages/core/src/wire/api.ts now: the wire types, the
// paths and the error vocabulary are identical on a phone, and the three things
// that are not — the address, the credential, and what a 401 means — are the
// transport installed in ./archive.
//
// What is left here is what only a browser can do. An XMLHttpRequest that can
// report how much of a body has gone out, a WebAuthn ceremony, and a sign-out
// that ends by navigating. None of the three has an equivalent on the phone and
// none of them should: the phone backs up its camera roll rather than uploading
// files, and it authenticates with a device token, which is the better
// credential in that direction.
//
// The star re-export is why the thirty-eight files importing "@/lib/api" did
// not have to change on the day it moved. It exports the whole of core rather
// than the wire half alone, because core's own two entry points are the place
// that distinction is drawn and drawing it twice would mean maintaining a list.
import "./archive";

import {
  apiBaseUrl,
  apiError,
  ApiError,
  finishPasskeyRegistration,
  liveThumbVariant,
  logout,
  media,
  startPasskeyRegistration,
  thumbVariant,
  type MediaVariant,
  type Passkey,
  type ThumbSize,
} from "@photobackup/core";
import {
  decodeCreationOptions,
  encodeAttestation,
  type WireCreationOptions,
} from "./webauthn";

export * from "@photobackup/core";

// ---------------------------------------------------------------------------
// Media, as a browser addresses it: a bare string, because that is what goes in
// an `<img src>`. The headers `media` returns beside it are for the phone,
// which cannot put a cookie on an image and does not need to.
//
// NEXT_PUBLIC_MEDIA_BASE survives here and nowhere else. It dates from the
// arrangement where media could skip a Node proxy hop by pointing straight at
// photod; there is no hop to skip now, so it is unset in every deployment and
// core has one base rather than two. Anything it is pointed at still has to be
// an origin the session cookie reaches. Read at build time — see web/README.
// ---------------------------------------------------------------------------

const MEDIA_BASE = process.env.NEXT_PUBLIC_MEDIA_BASE;

const at = (id: string, variant: MediaVariant): string =>
  MEDIA_BASE ? `${MEDIA_BASE}/v1/assets/${id}/${variant}` : media(id, variant).uri;

export const thumbUrl = (id: string, size?: ThumbSize) => at(id, thumbVariant(size));
export const previewUrl = (id: string) => at(id, "preview");
export const plainPreviewUrl = (id: string) => at(id, "preview/plain");
export const plainPlaybackUrl = (id: string) => at(id, "playback/plain");
export const liveThumbUrl = (id: string, size?: ThumbSize) => at(id, liveThumbVariant(size));
export const livePreviewUrl = (id: string) => at(id, "live/preview");
export const playbackUrl = (id: string) => at(id, "playback");
export const originalUrl = (id: string) => at(id, "original");

// ---------------------------------------------------------------------------
// Sending an original from a file picker.
// ---------------------------------------------------------------------------

export interface Uploaded {
  id: string;
  sha256: string;
  /** The archive already held these bytes, so nothing was added. */
  duplicate: boolean;
}

export interface UploadOptions {
  /** The digest, declared so photod can hold the transfer to it. */
  sha256?: string;
  /** Bytes acknowledged so far, for the row's own bar. */
  onProgress?: (sent: number) => void;
  signal?: AbortSignal;
}

/**
 * Sends one original.
 *
 * XMLHttpRequest, alone in this file, and for the one thing it still does that
 * `fetch` does not: report how much of a request body has gone out. A duplex
 * fetch would need a `ReadableStream` body, which rules out HTTP/1.1 — the
 * protocol the Next rewrite in front of photod speaks — and would still be
 * measuring what the page has handed to the browser rather than what has
 * reached the server.
 *
 * The file is the body, raw. There is no multipart wrapper because there is
 * exactly one file and nothing else to say: everything about it that photod
 * needs is a header, and the alternative is a boundary-encoded copy of a
 * gigabyte to save nothing.
 */
export function uploadAsset(file: File, options: UploadOptions = {}): Promise<Uploaded> {
  const { sha256, onProgress, signal } = options;

  return new Promise<Uploaded>((resolve, reject) => {
    const req = new XMLHttpRequest();
    req.open("POST", `${apiBaseUrl()}/v1/gallery/assets`);
    req.responseType = "text";

    req.setRequestHeader("Content-Type", "application/octet-stream");
    req.setRequestHeader("X-Photo-Filename", headerSafe(file.name));
    req.setRequestHeader("X-Photo-Size", String(file.size));
    if (sha256) req.setRequestHeader("X-Photo-Sha256", sha256);
    if (file.lastModified) {
      // A browser has no capture time; the file's own date is the best guess at
      // where a screenshot belongs on the timeline, and photod prefers whatever
      // EXIF says over it for everything that has any.
      const when = new Date(file.lastModified).toISOString();
      req.setRequestHeader("X-Photo-Captured-At", when);
      req.setRequestHeader("X-Photo-Modified-At", when);
    }

    const abort = () => req.abort();
    signal?.addEventListener("abort", abort);
    const done = () => signal?.removeEventListener("abort", abort);

    req.upload.onprogress = (ev) => onProgress?.(ev.loaded);

    req.onload = () => {
      done();
      if (req.status >= 200 && req.status < 300) {
        try {
          resolve(JSON.parse(req.responseText) as Uploaded);
        } catch {
          reject(apiError(req.status, "the server's answer was not JSON"));
        }
        return;
      }
      reject(apiError(req.status, uploadErrorText(req)));
    };
    // No status and no body, so there is nothing to report but the fact.
    req.onerror = () => {
      done();
      reject(new ApiError(0, "the connection to the server failed"));
    };
    req.onabort = () => {
      done();
      reject(signal?.reason ?? new DOMException("Aborted", "AbortError"));
    };

    req.send(file);
  });
}

/** photod's own message, when it sent one. */
function uploadErrorText(req: XMLHttpRequest): string {
  try {
    const body = JSON.parse(req.responseText) as { error?: string };
    if (body.error) return body.error;
  } catch {
    // Same reasoning as errorText: a non-JSON body means something other than
    // photod answered, and the status is the only honest detail left.
  }
  return `${req.status} ${req.statusText}`;
}

/**
 * A filename that can travel in a header.
 *
 * Header values are bytes, not text, and a photograph called `Ångström.heic`
 * or one carrying a stray newline would either throw on setRequestHeader or —
 * worse — be silently mangled into something the archive then stores under.
 * Percent-encoding is not what photod decodes, so this does the honest thing
 * instead: it keeps what is unambiguous and replaces the rest, so the stored
 * name is legible and no longer pretends to be exact.
 */
function headerSafe(name: string): string {
  // eslint-disable-next-line no-control-regex
  const cleaned = name.replace(/[^\x20-\x7e]/g, "_").trim();
  return cleaned === "" ? "upload" : cleaned;
}

/**
 * Ends this session on the server, then reloads.
 *
 * The reload is what actually takes the browser to the sign-in page: photod
 * guards the app itself, so the next navigation is redirected there. Doing it
 * this way rather than pushing a route means there is one place that decides
 * where an unauthenticated browser goes, and it is in Go.
 */
export async function signOut(): Promise<void> {
  await logout();
  window.location.assign("/signin");
}

/**
 * Registers an additional passkey from a browser that is already signed in.
 *
 * The two requests are core's; the ceremony between them is the browser's, and
 * is the reason this did not move with the rest of the file.
 */
export async function registerPasskey(): Promise<Passkey> {
  const options = await startPasskeyRegistration<WireCreationOptions>();
  const credential = (await navigator.credentials.create({
    publicKey: decodeCreationOptions(options),
  })) as PublicKeyCredential | null;

  if (!credential) throw new Error("no passkey was created");
  return finishPasskeyRegistration(encodeAttestation(credential));
}
