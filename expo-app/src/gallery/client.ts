import { errorText, SyncError } from '../sync/types';
import type { AssetDetail, MediaVariant, TimelineItem, TimelinePage } from './types';

/**
 * The read path answers from Postgres and the disk, so a request that has not
 * come back in this long is a server that is not coming back.
 */
const READ_TIMEOUT_MS = 15_000;

const DEFAULT_PAGE = 200;

/**
 * Reads the gallery from photod.
 *
 * Deliberately shaped like HttpTransport: the address and the token are
 * functions read per request rather than values captured at construction, so
 * discovery moving LAN to Tailscale and a re-pairing both take effect without
 * anything holding a client being rebuilt.
 *
 * The reason this exists at all — and the reason the in-app gallery is a
 * different problem from the browser one — is `mediaHeaders`. A browser cannot
 * put an Authorization header on an `<img>`, which is what left photod's read
 * path open through Phase 5 and would have forced signed URLs to close it. React
 * Native has no such limit: expo-image, expo-video and File.downloadFileAsync
 * all take headers, so every rendition authenticates with the token already in
 * the keychain and no second credential exists to leak.
 */
export class GalleryClient {
  constructor(
    private readonly baseUrl: () => string,
    private readonly deviceToken: () => string | null
  ) {}

  /** True when there is a token to send. Not proof the server still honours it. */
  get paired(): boolean {
    return Boolean(this.deviceToken());
  }

  /**
   * The headers every gallery request carries.
   *
   * Public because the media components need exactly these: an `<Image>` source
   * takes `{ uri, headers }`, and it must be the same header the JSON goes out
   * with or a thumbnail 401s while the timeline around it renders.
   *
   * A missing token is a missing header rather than an empty one, so photod
   * answers "a device token is required" instead of rejecting a malformed
   * credential — two different messages on a phone screen.
   */
  mediaHeaders(): Record<string, string> {
    const token = this.deviceToken();
    return token ? { authorization: `Bearer ${token}` } : {};
  }

  /**
   * Where one rendition of an asset lives.
   *
   * A URL alone is not enough to fetch it — pair it with mediaHeaders(). They
   * are separate because every consumer wants them separately: a source object
   * for expo-image, two arguments to File.downloadFileAsync.
   */
  mediaUrl(id: string, variant: MediaVariant): string {
    return this.url(`/v1/assets/${id}/${variant}`);
  }

  /** An absolute URL against whatever address discovery has settled on. */
  url(path: string): string {
    return `${this.base()}${path}`;
  }

  /** One page of the timeline, newest first. */
  timeline(options: { cursor?: string; limit?: number } = {}): Promise<TimelinePage> {
    const params = new URLSearchParams({ limit: String(options.limit ?? DEFAULT_PAGE) });
    if (options.cursor) params.set('cursor', options.cursor);
    return this.getJson<TimelinePage>(`/v1/timeline?${params.toString()}`);
  }

  /** Everything known about one asset, for a viewer's metadata panel. */
  asset(id: string): Promise<AssetDetail> {
    return this.getJson<AssetDetail>(`/v1/assets/${id}`);
  }

  /**
   * Re-reads derivative state for tiles that were not ready when first drawn,
   * which is how a gallery watches a backfill fill in without re-fetching pages.
   */
  async states(ids: string[]): Promise<TimelineItem[]> {
    if (ids.length === 0) return [];
    const body = await this.request('/v1/timeline/states', {
      method: 'POST',
      headers: { ...this.mediaHeaders(), 'content-type': 'application/json' },
      body: JSON.stringify({ ids }),
    });
    const parsed = await readJson<{ items?: TimelineItem[] }>(body, '/v1/timeline/states');
    return parsed.items ?? [];
  }

  /**
   * Fetches one rendition as bytes.
   *
   * Not how a gallery should draw a thumbnail — that is expo-image's job, with
   * its own caching — but it is how anything that needs the bytes themselves
   * gets them, and it is what proves the media path authenticates end to end.
   */
  async media(id: string, variant: MediaVariant): Promise<{ bytes: number; contentType: string }> {
    const response = await this.request(`/v1/assets/${id}/${variant}`, {
      headers: this.mediaHeaders(),
    });
    const contentType = response.headers.get('content-type') ?? 'unknown';

    // Content-Length first: photod serves every rendition through
    // http.ServeContent, which always sets it, and reading the header does not
    // depend on which ArrayBuffer support the current RN runtime has.
    const declared = Number.parseInt(response.headers.get('content-length') ?? '', 10);
    if (Number.isFinite(declared) && declared >= 0) {
      return { bytes: declared, contentType };
    }
    const buffer = await response.arrayBuffer();
    return { bytes: buffer.byteLength, contentType };
  }

  private async getJson<T>(path: string): Promise<T> {
    const response = await this.request(path, { headers: this.mediaHeaders() });
    return readJson<T>(response, path);
  }

  /**
   * One request, with the failure vocabulary the rest of the app already speaks.
   *
   * blameFor is duplicated from the transport rather than shared, because the
   * two disagree on purpose: an upload's 426 means "you are talking to the
   * read-only listener", while a read arriving there works fine.
   */
  private async request(path: string, init: RequestInit = {}): Promise<Response> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), READ_TIMEOUT_MS);

    let response: Response;
    try {
      response = await fetch(`${this.base()}${path}`, { ...init, signal: controller.signal });
    } catch (e) {
      throw new SyncError(errorText(e), 'unreachable');
    } finally {
      clearTimeout(timer);
    }

    if (!response.ok) {
      throw new SyncError(
        `${path} returned ${response.status}: ${await safeText(response)}`,
        response.status === 401 || response.status === 403
          ? 'unauthorized'
          : response.status >= 500
            ? 'server'
            : 'item',
        response.status
      );
    }
    return response;
  }

  private base(): string {
    return this.baseUrl().replace(/\/+$/, '');
  }
}

async function readJson<T>(response: Response, path: string): Promise<T> {
  try {
    return (await response.json()) as T;
  } catch (e) {
    throw new SyncError(`unreadable ${path} response: ${errorText(e)}`, 'item');
  }
}

async function safeText(response: Response): Promise<string> {
  try {
    return (await response.text()).slice(0, 300);
  } catch {
    return '<unreadable body>';
  }
}
