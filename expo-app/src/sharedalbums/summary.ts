/**
 * What a pile of iCloud Shared Albums adds up to.
 *
 * Pure, and importing nothing but types, for the reason src/stats/format.ts is:
 * survey.ts reaches into the native module and cannot be loaded under jest, so
 * the arithmetic that is actually worth checking lives on this side of the line.
 *
 * The questions this is built to answer are the ones that decide whether backing
 * up shared albums is worth doing at all:
 *
 *   How much is there?          Counts, deduplicated across albums.
 *   What did sharing cost?      Dimensions against Apple's documented caps.
 *   Is the original reachable?  The resource inventory.
 *   What are these things?      Source types, stills against videos, Live Photos.
 *
 * It deliberately does not try to work out which of them are photographs the
 * phone already owns the original of. Everything found here is going to be
 * uploaded and the server will decide what is a duplicate of what, so a
 * heuristic answer here would be a second opinion nobody consults.
 */

import type { SharedAlbum, SharedAsset } from '../../modules/photo-facts';

/**
 * Apple's documented ceilings for what goes into a Shared Album: 2048 pixels on
 * a photo's long edge, 720p and fifteen minutes on a video.
 *
 * They are here to be *tested against* rather than relied on. The survey counts
 * what exceeds them, and anything that does is the interesting result — it would
 * mean the shared copy is not the downscale the rest of this assumes.
 */
export const STILL_LONG_EDGE_CAP = 2048;
export const VIDEO_LONG_EDGE_CAP = 1280;
export const VIDEO_SECONDS_CAP = 15 * 60;

/** How many assets the byte-fetch step will try. */
export const SAMPLE_SIZE = 3;

export type AlbumSummary = {
  /** PhotoKit's own title, and null where an album has none. */
  title: string | null;
  assets: number;
  stills: number;
  videos: number;
  live: number;
  /** Capture time of the oldest and newest asset in the album, milliseconds. */
  oldest: number | null;
  newest: number | null;
};

/**
 * The dimensions of one class of asset, described by its extreme rather than its
 * average.
 *
 * A mean long edge would say nothing: the question is not how big these are, it
 * is whether anything got through above the cap, and one asset that did is the
 * whole finding. `atMax` is what tells a cap from a coincidence — 700 stills
 * sharing a maximum is a ceiling being enforced, and two is a couple of photos
 * that happened to be the same size.
 */
export type EdgeSummary = {
  maxLongEdge: number | null;
  atMax: number;
  overCap: number;
};

export type SharedSurvey = {
  /** False when this dev client has no shared-album enumerator in it. */
  supported: boolean;
  albums: AlbumSummary[];
  /** Deduplicated across albums: an asset in three of them counts once here. */
  assets: number;
  stills: number;
  videos: number;
  live: number;
  inMultipleAlbums: number;
  oldest: number | null;
  newest: number | null;
  still: EdgeSummary;
  video: EdgeSummary;
  /** Longest clip in the whole set, in seconds. */
  longestVideoSeconds: number;
  /** Videos longer than Apple's documented fifteen minutes. */
  longVideos: number;
  /** How many assets carry each PHAssetResource type. */
  resourceTypes: Record<string, number>;
  /** How many assets carry each PHAsset source type. */
  sourceTypes: Record<string, number>;
  /** The assets the byte fetch will try, and why these ones — see pickSample. */
  sample: SharedAsset[];
};

export function emptySurvey(supported: boolean): SharedSurvey {
  return {
    supported,
    albums: [],
    assets: 0,
    stills: 0,
    videos: 0,
    live: 0,
    inMultipleAlbums: 0,
    oldest: null,
    newest: null,
    still: emptyEdges(),
    video: emptyEdges(),
    longestVideoSeconds: 0,
    longVideos: 0,
    resourceTypes: {},
    sourceTypes: {},
    sample: [],
  };
}

/**
 * Folds the albums into one description of them.
 *
 * The per-album figures count memberships and the totals count assets, and that
 * is not an inconsistency to be tidied away: an album's own count is what the
 * Photos app shows beside it, and the total is how many things a backup would
 * have to fetch. `inMultipleAlbums` is the difference, stated rather than left
 * to be worked out from a discrepancy.
 */
export function summarize(albums: SharedAlbum[]): SharedSurvey {
  const survey = emptySurvey(true);

  survey.albums = albums.map((album) => ({
    title: album.title,
    assets: album.assets.length,
    stills: album.assets.filter((asset) => asset.kind === 'still').length,
    videos: album.assets.filter((asset) => asset.kind === 'video').length,
    live: album.assets.filter((asset) => asset.isLive).length,
    oldest: extreme(album.assets, Math.min),
    newest: extreme(album.assets, Math.max),
  }));

  const seen = new Map<string, SharedAsset>();
  const memberships = new Map<string, number>();
  for (const album of albums) {
    for (const asset of album.assets) {
      seen.set(asset.localId, asset);
      memberships.set(asset.localId, (memberships.get(asset.localId) ?? 0) + 1);
    }
  }

  // Newest capture first, matching every other listing in this app, and the
  // order the sample is drawn in.
  const assets = [...seen.values()].sort((a, b) => (b.createdAt ?? 0) - (a.createdAt ?? 0));

  survey.assets = assets.length;
  survey.stills = assets.filter((asset) => asset.kind === 'still').length;
  survey.videos = assets.filter((asset) => asset.kind === 'video').length;
  survey.live = assets.filter((asset) => asset.isLive).length;
  survey.inMultipleAlbums = [...memberships.values()].filter((count) => count > 1).length;
  survey.oldest = extreme(assets, Math.min);
  survey.newest = extreme(assets, Math.max);

  survey.still = edges(
    assets.filter((asset) => asset.kind === 'still'),
    STILL_LONG_EDGE_CAP
  );
  survey.video = edges(
    assets.filter((asset) => asset.kind === 'video'),
    VIDEO_LONG_EDGE_CAP
  );

  const durations = assets.filter((asset) => asset.kind === 'video').map((a) => a.durationSeconds);
  survey.longestVideoSeconds = durations.length > 0 ? Math.max(...durations) : 0;
  survey.longVideos = durations.filter((seconds) => seconds > VIDEO_SECONDS_CAP).length;

  for (const asset of assets) {
    for (const name of new Set(asset.resourceTypes)) {
      survey.resourceTypes[name] = (survey.resourceTypes[name] ?? 0) + 1;
    }
    for (const name of new Set(asset.sourceTypes.names)) {
      survey.sourceTypes[name] = (survey.sourceTypes[name] ?? 0) + 1;
    }
  }

  survey.sample = pickSample(assets);
  return survey;
}

/**
 * The handful of assets worth actually downloading.
 *
 * Spread across kinds before depth, because the three of them fail differently:
 * a still is one small fetch, a Live Photo raises the question of whether its
 * paired clip came across at all, and a video is the one big enough for the
 * elapsed time to mean something. Once there is one of each, the rest of the
 * budget goes to whatever is newest — a second still is a second reading on the
 * throughput, which is worth more than nothing at all.
 *
 * `assets` is expected newest first, and the sample inherits that: the newest
 * shared asset is the one most likely to still be there to fetch.
 */
export function pickSample(assets: SharedAsset[]): SharedAsset[] {
  const picked: SharedAsset[] = [];
  const take = (match: (asset: SharedAsset) => boolean) => {
    const found = assets.find((asset) => match(asset) && !picked.includes(asset));
    if (found) picked.push(found);
  };

  take((asset) => asset.kind === 'still' && !asset.isLive);
  take((asset) => asset.kind === 'still' && asset.isLive);
  take((asset) => asset.kind === 'video');

  for (const asset of assets) {
    if (picked.length >= SAMPLE_SIZE) break;
    if (!picked.includes(asset)) picked.push(asset);
  }

  return picked.slice(0, SAMPLE_SIZE);
}

/**
 * Bytes per second as megabytes, for reading a fetch against the Wi-Fi it came
 * over.
 *
 * Null rather than Infinity for a fetch that reported no time at all. It happens
 * for anything PhotoKit had already cached, and "instant" is a fact about the
 * cache rather than about iCloud — a throughput figure there would be a lie the
 * width of the screen.
 */
export function throughputMbPerSecond(bytes: number, elapsedMs: number): number | null {
  if (!Number.isFinite(bytes) || !Number.isFinite(elapsedMs) || elapsedMs <= 0) return null;
  return bytes / 1e6 / (elapsedMs / 1000);
}

/** "1:04" or "12s", for a clip length rather than an age. */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0s';
  if (seconds < 60) return `${Math.round(seconds)}s`;

  const whole = Math.round(seconds);
  const minutes = Math.floor(whole / 60);
  return `${minutes}:${String(whole % 60).padStart(2, '0')}`;
}

/** The long edge of one asset, which is the number the caps are written about. */
export function longEdge(asset: SharedAsset): number {
  return Math.max(asset.pixelWidth, asset.pixelHeight);
}

function edges(assets: SharedAsset[], cap: number): EdgeSummary {
  if (assets.length === 0) return emptyEdges();

  const longest = assets.map(longEdge);
  const maxLongEdge = Math.max(...longest);
  return {
    maxLongEdge,
    atMax: longest.filter((edge) => edge === maxLongEdge).length,
    overCap: longest.filter((edge) => edge > cap).length,
  };
}

function emptyEdges(): EdgeSummary {
  return { maxLongEdge: null, atMax: 0, overCap: 0 };
}

/** The oldest or newest capture time, ignoring assets that carry none. */
function extreme(assets: SharedAsset[], pick: (...values: number[]) => number): number | null {
  const times = assets
    .map((asset) => asset.createdAt)
    .filter((at): at is number => at !== null && Number.isFinite(at));
  return times.length > 0 ? pick(...times) : null;
}
