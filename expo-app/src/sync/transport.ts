import { File } from 'expo-file-system';

import {
  errorText,
  SyncError,
  toIso,
  type CheckRequestItem,
  type CheckResultItem,
  type Transport,
  type UploadRequest,
  type UploadResponse,
} from './types';

/**
 * React Native's fetch has no default timeout, and a half-open Wi-Fi connection
 * would otherwise wedge the run loop for good.
 */
const CHECK_TIMEOUT_MS = 15_000;

/**
 * Talks to photod.
 *
 * The base URL is a function rather than a value so discovery can change the
 * address — LAN to Tailscale, or a freshly resolved mDNS address — without the
 * engine being rebuilt around a new transport.
 */
export class HttpTransport implements Transport {
  constructor(private readonly baseUrl: () => string) {}

  async check(deviceId: string, items: CheckRequestItem[]): Promise<CheckResultItem[]> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), CHECK_TIMEOUT_MS);

    let response: Response;
    try {
      response = await fetch(`${trimSlash(this.baseUrl())}/v1/sync/check`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
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
        response.status >= 500 ? 'server' : 'item',
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
   * Uploads one original straight from PhotoKit.
   *
   * sessionType must be 'foreground'. Phase 0 established that a background
   * NSURLSession hands the file to nsurlsessiond, which does not inherit the
   * app's PhotoKit sandbox extension, and the upload fails before a byte leaves
   * the device with no indication of why.
   */
  async upload(request: UploadRequest): Promise<UploadResponse> {
    const headers: Record<string, string> = {
      'content-type': 'application/octet-stream',
      'x-photo-filename': request.filename,
      'x-photo-md5': request.md5,
      'x-photo-size': String(request.size),
      'x-photo-device-id': request.deviceId,
      'x-photo-local-id': request.localId,
    };
    const capturedAt = toIso(request.createdAt);
    if (capturedAt) headers['x-photo-captured-at'] = capturedAt;
    const modifiedAt = toIso(request.modifiedAt);
    if (modifiedAt) headers['x-photo-modified-at'] = modifiedAt;

    let response: { status: number; body: string };
    try {
      response = await new File(request.uri).upload(`${trimSlash(this.baseUrl())}/v1/assets`, {
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
        response.status >= 500 ? 'server' : 'item',
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
