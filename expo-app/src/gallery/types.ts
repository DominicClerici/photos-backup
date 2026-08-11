/**
 * Mirrors the JSON photod emits from its read path. Hand-written rather than
 * generated for the same reason web/src/lib/api.ts is: the surface is small and
 * stable, and the Go structs in internal/db and internal/api are the source of
 * truth.
 *
 * Kept deliberately identical to the web gallery's types, field for field. The
 * plan is to port the dashboard here once it has grown up, and a second dialect
 * of the same JSON would make that a translation rather than a move.
 */

export type MediaKind = 'image' | 'video';
export type DerivedState = 'pending' | 'ready' | 'failed';
export type PlaybackState = 'none' | 'pending' | 'ready' | 'failed';

export type TimelineItem = {
  id: string;
  kind: MediaKind;
  taken_at: string;
  /** The file's own UTC offset. Absent when the file recorded no timezone. */
  offset_minutes?: number;
  state: DerivedState;
  playback_state?: PlaybackState;
  duration?: number;
};

export type TimelinePage = {
  items: TimelineItem[];
  next_cursor?: string;
};

export type AssetDetail = {
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
};

/**
 * The four renditions of one asset.
 *
 * `thumb` is the stored 256px WebP, `preview` is rendered on demand at 2048px,
 * `playback` is the transcoded MP4 a video plays from, and `original` is the
 * archived file itself — the only one that is a download rather than a view.
 */
export type MediaVariant = 'thumb' | 'preview' | 'playback' | 'original';
