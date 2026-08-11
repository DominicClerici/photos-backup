// Mirrors the JSON photod actually emits. These types are hand-written rather
// than generated because the surface is small and stable; if it grows, the Go
// structs in internal/db and internal/api are the source of truth.

// The sizes live in layout.ts, beside the zoom levels that choose between them.
// Nothing is imported back the other way at runtime — layout.ts takes only a
// type from here — so this is a dependency rather than a cycle.
import { BASE_THUMB_SIZE, type ThumbSize } from "./layout";

export type MediaKind = "image" | "video";
export type DerivedState = "pending" | "ready" | "failed";
export type PlaybackState = "none" | "pending" | "ready" | "failed";
export type LiveState = "pending" | "ready" | "failed";

export interface TimelineItem {
  id: string;
  kind: MediaKind;
  taken_at: string;
  /** The file's own UTC offset. Absent when the file recorded no timezone. */
  offset_minutes?: number;
  state: DerivedState;
  playback_state?: PlaybackState;
  duration?: number;
  /**
   * State of this still's Live Photo motion. Absent means there is none — the
   * paired video is never an item of its own, so its whole presence in the
   * gallery is this one field on the photo it belongs to.
   */
  live?: LiveState;
}

export interface TimelinePage {
  items: TimelineItem[];
  next_cursor?: string;
}

export interface AssetDetail {
  id: string;
  filename: string;
  kind: MediaKind;
  sha256: string;
  byte_size: number;
  width?: number;
  height?: number;
  duration?: number;
  taken_at: string;
  offset_minutes?: number;
  reported_at?: string;
  uploaded_at: string;
  camera_make?: string;
  camera_model?: string;
  lens?: string;
  gps_lat?: number;
  gps_lon?: number;
  state: DerivedState;
  playback_state?: PlaybackState;
}

// JSON goes through the Next rewrite so it is same-origin and needs no CORS.
const API_BASE = "/api";

// Media can skip that hop. An <img> or <video> is not subject to CORS unless it
// opts in, so pointing them straight at photod costs nothing and keeps a few
// thousand thumbnails from streaming through Node. Unset means "use the proxy",
// which is the right default because it always works.
const MEDIA_BASE = process.env.NEXT_PUBLIC_MEDIA_BASE || API_BASE;

class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, { signal });
  if (!res.ok) throw new ApiError(res.status, await errorText(res));
  return res.json() as Promise<T>;
}

async function errorText(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body.error) return body.error;
  } catch {
    // A non-JSON error body means something other than photod answered —
    // a proxy, or nothing at all. The status is the only honest detail left.
  }
  return `${res.status} ${res.statusText}`;
}

export function fetchTimeline(
  cursor: string | undefined,
  limit: number,
  signal?: AbortSignal,
): Promise<TimelinePage> {
  const params = new URLSearchParams({ limit: String(limit) });
  if (cursor) params.set("cursor", cursor);
  return get<TimelinePage>(`/v1/timeline?${params}`, signal);
}

export function fetchAsset(id: string, signal?: AbortSignal): Promise<AssetDetail> {
  return get<AssetDetail>(`/v1/assets/${id}`, signal);
}

export interface Health {
  ok: boolean;
  failed_jobs: number;
  pending_jobs: number;
}

export function fetchHealth(signal?: AbortSignal): Promise<Health> {
  return get<Health>("/health", signal);
}

/** Re-reads derivative state for tiles that were not ready when first drawn. */
export async function fetchStates(
  ids: string[],
  signal?: AbortSignal,
): Promise<TimelineItem[]> {
  if (ids.length === 0) return [];
  const res = await fetch(`${API_BASE}/v1/timeline/states`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ids }),
    signal,
  });
  if (!res.ok) throw new ApiError(res.status, await errorText(res));
  const body = (await res.json()) as { items: TimelineItem[] };
  return body.items ?? [];
}

/**
 * A stored thumbnail. No size, or the base size, is the unsized route: the same
 * URL the gallery has always used, so the bytes stay in one cache entry rather
 * than two.
 *
 * The base is also the only size every asset is guaranteed to have. A library
 * ingested before the others existed gets them when a backfill runs, and until
 * then asking for one is a 404 the caller falls back from.
 */
export const thumbUrl = (id: string, size?: ThumbSize) =>
  size === undefined || size === BASE_THUMB_SIZE
    ? `${MEDIA_BASE}/v1/assets/${id}/thumb`
    : `${MEDIA_BASE}/v1/assets/${id}/thumb/${size}`;
export const previewUrl = (id: string) => `${MEDIA_BASE}/v1/assets/${id}/preview`;
/** A Live Photo's motion, addressed by the still's id, in the same sizes. */
export const liveThumbUrl = (id: string, size?: ThumbSize) =>
  size === undefined || size === BASE_THUMB_SIZE
    ? `${MEDIA_BASE}/v1/assets/${id}/live/thumb`
    : `${MEDIA_BASE}/v1/assets/${id}/live/thumb/${size}`;
export const livePreviewUrl = (id: string) => `${MEDIA_BASE}/v1/assets/${id}/live/preview`;
export const playbackUrl = (id: string) => `${MEDIA_BASE}/v1/assets/${id}/playback`;
export const originalUrl = (id: string) => `${MEDIA_BASE}/v1/assets/${id}/original`;

export { ApiError };
