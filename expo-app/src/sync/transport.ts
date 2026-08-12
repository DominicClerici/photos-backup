import { File } from 'expo-file-system';

import { CHUNK_SIZE, planChunks } from './chunkPlan';
import { ChunkTransport } from './chunked';
import {
  errorText,
  SyncError,
  toIso,
  type AssetFacts,
  type CheckRequestItem,
  type CheckResultItem,
  type Transport,
  type UploadProgress,
  type UploadRequest,
  type UploadResponse,
} from './types';

/**
 * React Native's fetch has no default timeout, and a half-open Wi-Fi connection
 * would otherwise wedge the run loop for good.
 */
const CHECK_TIMEOUT_MS = 15_000;

/**
 * Opening or committing a session is a small request against a server that is
 * either answering or not. A chunk gets no timeout at all: 8MB over a weak
 * connection is slow, not stalled.
 */
const SESSION_TIMEOUT_MS = 30_000;

/**
 * How many times a single upload will re-read the server's offset and re-plan
 * before giving up. A conflict means the two disagreed about progress, which is
 * recoverable once; a server that keeps disagreeing is broken, and retrying
 * forever would pin the run loop on one file.
 */
const MAX_REPLANS = 3;

type SessionState = { uploadId: string; offset: number; complete: boolean };

/**
 * Talks to photod.
 *
 * The base URL is a function rather than a value so discovery can change the
 * address — LAN to Tailscale, or a freshly resolved mDNS address — without the
 * engine being rebuilt around a new transport.
 */
export class HttpTransport implements Transport {
  private readonly chunks: ChunkTransport;

  /**
   * Both the address and the token are read per request rather than captured.
   * Discovery can move the address — LAN to Tailscale — and re-pairing can
   * replace the token, neither of which should mean rebuilding the engine
   * around a new transport mid-run.
   */
  constructor(
    private readonly baseUrl: () => string,
    private readonly deviceToken: () => string | null,
    onLog?: (line: string) => void
  ) {
    this.chunks = new ChunkTransport(onLog);
  }

  /**
   * The headers every request to photod carries.
   *
   * A missing token is sent as a missing header rather than as an empty one, so
   * the server answers "a device token is required" instead of rejecting a
   * malformed credential — the two are a different message on the phone.
   */
  private headers(extra: Record<string, string> = {}): Record<string, string> {
    const token = this.deviceToken();
    return token ? { ...extra, authorization: `Bearer ${token}` } : extra;
  }

  /** Which chunk strategy ended up in use, for the diagnostics panel. */
  get chunkMode(): string {
    return this.chunks.mode;
  }

  async check(deviceId: string, items: CheckRequestItem[]): Promise<CheckResultItem[]> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), CHECK_TIMEOUT_MS);

    let response: Response;
    try {
      response = await fetch(`${this.base()}/v1/sync/check`, {
        method: 'POST',
        headers: this.headers({ 'content-type': 'application/json' }),
        body: JSON.stringify({ deviceId, items }),
        signal: controller.signal,
      });
    } catch (e) {
      // A refused connection, a DNS failure and an abort are all "the server is
      // not answering", which is the server's problem and not any item's.
      throw new SyncError(errorText(e), 'unreachable');
    } finally {
      clearTimeout(timer);
    }

    if (!response.ok) {
      throw new SyncError(
        `sync/check returned ${response.status}: ${await safeText(response)}`,
        blameFor(response.status),
        response.status
      );
    }

    let payload: unknown;
    try {
      payload = await response.json();
    } catch (e) {
      throw new SyncError(`unreadable sync/check response: ${errorText(e)}`, 'item');
    }

    const results = (payload as { results?: unknown })?.results;
    if (!Array.isArray(results)) {
      throw new SyncError('sync/check response carried no results array', 'item');
    }
    return results as CheckResultItem[];
  }

  /**
   * Hands the archive what the library knows about an asset it already holds.
   *
   * The same endpoint the Google Takeout importer posts its sidecars to, with
   * the phone named as the source: a heart, an album and "this is a screenshot"
   * are exactly the class of thing a sidecar carries — decisions a person made,
   * which no amount of re-reading the original recovers.
   */
  async describe(assetId: string, facts: AssetFacts): Promise<void> {
    const response = await postJson(
      `${this.base()}/v1/assets/${encodeURIComponent(assetId)}/import-metadata`,
      this.headers(),
      {
        source: 'ios-photokit',
        // The albums travel beside the sidecar rather than inside it, which is
        // the shape the endpoint already takes and what puts them in the
        // manifest as their own field.
        albums: facts.albums.map((title) => ({ title })),
        sidecar: {
          favorite: facts.favorite,
          subtypes: facts.subtypes,
          location: facts.location,
          // Left out entirely rather than sent as false when the native module
          // is not in this build: the sidecar is stored verbatim and read again
          // years later, and "the phone never said" is not the same claim as
          // "the phone said no".
          ...(facts.photoKit ? { hidden: facts.photoKit.hidden, photoKit: facts.photoKit } : {}),
        },
      }
    );

    if (response.status !== 204) {
      throw new SyncError(
        `import-metadata returned ${response.status}: ${await safeText(response)}`,
        blameFor(response.status),
        response.status
      );
    }
  }

  /**
   * Uploads one original straight from PhotoKit, in a single request.
   *
   * sessionType must be 'foreground'. Phase 0 established that a background
   * NSURLSession hands the file to nsurlsessiond, which does not inherit the
   * app's PhotoKit sandbox extension, and the upload fails before a byte leaves
   * the device with no indication of why.
   */
  async upload(request: UploadRequest): Promise<UploadResponse> {
    const headers: Record<string, string> = this.headers({
      'content-type': 'application/octet-stream',
      'x-photo-filename': request.filename,
      'x-photo-md5': request.md5,
      'x-photo-size': String(request.size),
      'x-photo-device-id': request.deviceId,
      'x-photo-local-id': request.localId,
    });
    const capturedAt = toIso(request.createdAt);
    if (capturedAt) headers['x-photo-captured-at'] = capturedAt;
    const modifiedAt = toIso(request.modifiedAt);
    if (modifiedAt) headers['x-photo-modified-at'] = modifiedAt;
    if (request.liveParentLocalId) {
      headers['x-photo-live-parent-local-id'] = request.liveParentLocalId;
    }

    let response: { status: number; body: string };
    try {
      response = await new File(request.uri).upload(`${this.base()}/v1/assets`, {
        httpMethod: 'POST',
        sessionType: 'foreground',
        headers,
      });
    } catch (e) {
      throw new SyncError(errorText(e), 'unreachable');
    }

    if (response.status < 200 || response.status >= 300) {
      throw new SyncError(
        `upload returned ${response.status}: ${response.body}`,
        blameFor(response.status),
        response.status
      );
    }

    let parsed: UploadResponse;
    try {
      parsed = JSON.parse(response.body) as UploadResponse;
    } catch {
      throw new SyncError(`unreadable upload response: ${response.body}`, 'item');
    }
    if (!parsed?.id) {
      throw new SyncError(`upload response carried no asset id: ${response.body}`, 'item');
    }
    return parsed;
  }

  /**
   * Uploads one large original in resumable chunks.
   *
   * Opening the session is also how it resumes: the id is derived server-side
   * from the declaration, so this says "here is what I am sending" and is told
   * how much already arrived. The phone stores nothing about a transfer in
   * flight and still picks up where it left off after being killed, which is
   * the property that makes a 550MB video survivable on a phone iOS can
   * terminate at any moment.
   */
  async uploadResumable(request: UploadRequest, onProgress?: UploadProgress): Promise<UploadResponse> {
    const file = new File(request.uri);
    let session = await this.beginSession(request);
    onProgress?.(session.offset, request.size);

    for (let attempt = 0; ; attempt++) {
      const conflicted = await this.sendChunks(file, request, session, onProgress);
      if (!conflicted) break;

      if (attempt >= MAX_REPLANS) {
        throw new SyncError(
          `the server kept disagreeing about how much of ${request.filename} it holds`,
          'item'
        );
      }
      // Re-read the truth and plan again from there.
      session = await this.beginSession(request);
      onProgress?.(session.offset, request.size);
    }

    return this.commitSession(session.uploadId);
  }

  /**
   * Sends every chunk the session still owes, mutating its offset as it goes.
   * Returns true when the server reported a different offset than expected and
   * the plan needs rebuilding.
   */
  private async sendChunks(
    file: File,
    request: UploadRequest,
    session: SessionState,
    onProgress?: UploadProgress
  ): Promise<boolean> {
    for (const [start, end] of planChunks(request.size, session.offset, CHUNK_SIZE)) {
      const headers = this.headers({
        'content-type': 'application/octet-stream',
        'content-range': `bytes ${start}-${end - 1}/${request.size}`,
      });

      let response: Response;
      try {
        response = await this.chunks.send(`${this.base()}/v1/uploads/${session.uploadId}`, headers, {
          file,
          start,
          end,
        });
      } catch (e) {
        throw new SyncError(errorText(e), 'unreachable');
      }

      if (response.status === 409) {
        return true;
      }
      if (!response.ok) {
        throw new SyncError(
          `chunk at ${start} returned ${response.status}: ${await safeText(response)}`,
          blameFor(response.status),
          response.status
        );
      }

      const next = await readSession(response);
      session.offset = next.offset;
      onProgress?.(session.offset, request.size);
    }
    return false;
  }

  private async beginSession(request: UploadRequest): Promise<SessionState> {
    const response = await postJson(`${this.base()}/v1/uploads`, this.headers(), {
      deviceId: request.deviceId,
      localId: request.localId,
      filename: request.filename,
      md5: request.md5,
      size: request.size,
      capturedAt: toIso(request.createdAt),
      modifiedAt: toIso(request.modifiedAt),
      liveParentLocalId: request.liveParentLocalId ?? undefined,
    });

    if (!response.ok) {
      throw new SyncError(
        `opening an upload session returned ${response.status}: ${await safeText(response)}`,
        blameFor(response.status),
        response.status
      );
    }
    return readSession(response);
  }

  private async commitSession(uploadId: string): Promise<UploadResponse> {
    const response = await postJson(`${this.base()}/v1/uploads/${uploadId}/commit`, this.headers(), undefined);

    if (!response.ok) {
      throw new SyncError(
        `committing an upload returned ${response.status}: ${await safeText(response)}`,
        blameFor(response.status),
        response.status
      );
    }

    let parsed: UploadResponse;
    try {
      parsed = (await response.json()) as UploadResponse;
    } catch (e) {
      throw new SyncError(`unreadable commit response: ${errorText(e)}`, 'item');
    }
    if (!parsed?.id) {
      throw new SyncError('commit response carried no asset id', 'item');
    }
    return parsed;
  }

  private base(): string {
    return trimSlash(this.baseUrl());
  }
}

async function readSession(response: Response): Promise<SessionState> {
  let payload: { uploadId?: unknown; offset?: unknown; complete?: unknown };
  try {
    payload = await response.json();
  } catch (e) {
    throw new SyncError(`unreadable upload session response: ${errorText(e)}`, 'item');
  }
  if (typeof payload?.uploadId !== 'string' || typeof payload?.offset !== 'number') {
    throw new SyncError('upload session response was missing its id or offset', 'item');
  }
  return { uploadId: payload.uploadId, offset: payload.offset, complete: !!payload.complete };
}

/**
 * Who to blame for a status code.
 *
 * 401 and 403 are this device's standing, not this item's: a revoked token or a
 * pairing that no longer matches. Charging them to items would retire the whole
 * library to `failed`, five attempts at a time, over something one pairing fixes.
 * 426 lands here too — it means the address in use is photod's read-only
 * listener, so no amount of retrying will get an upload accepted on it.
 */
function blameFor(status: number): 'unauthorized' | 'server' | 'item' {
  if (status === 401 || status === 403 || status === 426) return 'unauthorized';
  return status >= 500 ? 'server' : 'item';
}

async function postJson(
  url: string,
  headers: Record<string, string>,
  body: unknown
): Promise<Response> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), SESSION_TIMEOUT_MS);
  try {
    return await fetch(url, {
      method: 'POST',
      headers: { ...headers, 'content-type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: controller.signal,
    });
  } catch (e) {
    throw new SyncError(errorText(e), 'unreachable');
  } finally {
    clearTimeout(timer);
  }
}

async function safeText(response: Response): Promise<string> {
  try {
    return (await response.text()).slice(0, 300);
  } catch {
    return '<unreadable body>';
  }
}

function trimSlash(url: string): string {
  return url.replace(/\/+$/, '');
}
