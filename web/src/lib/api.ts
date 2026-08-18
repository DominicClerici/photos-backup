// Mirrors the JSON photod actually emits. These types are hand-written rather
// than generated because the surface is small and stable; if it grows, the Go
// structs in internal/db and internal/api are the source of truth.

// The sizes live in layout.ts, beside the zoom levels that choose between them.
// Nothing is imported back the other way at runtime — layout.ts takes only a
// type from here — so this is a dependency rather than a cycle.
import { BASE_THUMB_SIZE, type ThumbSize } from "./layout";
// browser gate: the one line of this feature that reaches into the API client.
// See ./session.
import { signedOut } from "./session";

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
  // What an import knew and the file did not: a Google Takeout carries the
  // caption, the star, and the album a photo was in in a JSON sidecar rather
  // than in the photo. All absent on anything the phone delivered directly.
  description?: string;
  favorite?: boolean;
  archived?: boolean;
  albums?: string[];
  people?: string[];
  /**
   * A Snapchat memory: the photograph and the caption layer drawn over it are
   * two archived files, and every rendition the server serves is the two
   * composed. Absent on everything else. It is the whole of what the viewer
   * needs to offer the toggle — the layer is never addressed directly.
   */
  has_overlay?: boolean;
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
    // browser gate: every refusal in this file is constructed here, so this is
    // the one place that catches a session lapsing mid-scroll without a check
    // at each of the dozen call sites. Deleting the feature is deleting these
    // two lines and the import above.
    if (status === 401) signedOut();
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

/**
 * Which slice of the library a timeline is of. Undefined — the gallery's own —
 * is the whole thing.
 *
 * The filtered timeline is the same endpoint, the same cursor and the same page
 * shape as the unfiltered one, which is what lets an album reuse the grid
 * wholesale rather than getting a second, lesser one of its own.
 */
export type CollectionFilter =
  | { kind: "albums"; value: string }
  | { kind: "people"; value: string }
  | { kind: "categories"; value: string };

/**
 * The deleted half of the archive: the same timeline, the same ordering, the
 * same day table, over the rows the library no longer shows.
 *
 * A scope rather than a collection, which is why it carries no value. A
 * collection narrows the library; this replaces the rule that says what the
 * library is, so the two cannot be spelled the same way without letting
 * something ask for a deleted album and get a coherent-looking answer.
 */
export type TimelineFilter = CollectionFilter | { kind: "trash" } | VaultFilter;

/** Which of the two buckets. Two destinations, one mechanism, one password. */
export type Bucket = "archive" | "hidden";

/**
 * A timeline inside the vault, optionally narrowed to one of its collections.
 *
 * A scope like the trash, and unlike it in one way that shows up here: it
 * carries a collection of its own. The trash is flat because a deleted photo
 * has left its albums; the vault is not, because a hidden photo's albums went
 * in with it and the Archive page draws them.
 *
 * Everything about it on the wire is a different path — /v1/vault/{bucket}/… —
 * and nothing about it is different in the grid. Same page shape, same cursor,
 * same day table, so the same gallery draws both halves of the archive.
 */
export type VaultFilter = { kind: "vault"; bucket: Bucket; within?: CollectionFilter };

/** The vault's own endpoints live under a different prefix, not a parameter. */
function vaultPath(filter: VaultFilter, tail: string): string {
  return `/v1/vault/${filter.bucket}${tail}`;
}

/** The query parameter each collection kind is spelled with on the wire. */
const FILTER_PARAM = { albums: "album", people: "person", categories: "category" } as const;

/** Writes the filter into a query string, whichever kind it is. */
function withFilter(params: URLSearchParams, filter?: TimelineFilter): URLSearchParams {
  if (!filter) return params;
  if (filter.kind === "trash") params.set("trash", "1");
  else if (filter.kind === "vault") {
    // The bucket is in the path; only the collection inside it is a parameter.
    if (filter.within) params.set(FILTER_PARAM[filter.within.kind], filter.within.value);
  } else params.set(FILTER_PARAM[filter.kind], filter.value);
  return params;
}

/** Where a timeline request goes. The vault is a prefix; everything else is a
 * parameter on the library's own endpoint. */
function timelinePath(filter: TimelineFilter | undefined, tail: string): string {
  return filter?.kind === "vault" ? vaultPath(filter, tail) : tail;
}

/**
 * Where a page begins. Two ways of saying it, and never both.
 *
 * A cursor continues from the page before, which is what scrolling down does
 * and what keeps sequential paging a keyset walk the server answers in constant
 * time. A skip names a position in the day table, which is what a fling into
 * the middle of the library does — there is no page before it to continue from.
 */
export type PageStart = { cursor: string } | { skip: number };

export function fetchTimeline(
  start: PageStart,
  limit: number,
  filter?: TimelineFilter,
  signal?: AbortSignal,
): Promise<TimelinePage> {
  const params = new URLSearchParams({ limit: String(limit) });
  if ("cursor" in start) params.set("cursor", start.cursor);
  else if (start.skip > 0) params.set("skip", String(start.skip));
  withFilter(params, filter);
  return get<TimelinePage>(`${timelinePath(filter, "/v1/timeline")}?${params}`, signal);
}

/** One heading the timeline will draw, and how many tiles hang under it. */
export interface DayRun {
  /** The local calendar day, YYYY-MM-DD. Rendered here, not by the server. */
  day: string;
  count: number;
}

/**
 * The shape of a whole timeline, fetched before any of it.
 *
 * A run per heading rather than a count per date: items are ordered by instant
 * and filed under their own local day, so a date can appear more than once and
 * the grid draws it as many times as it appears. See db.DayTable.
 */
export interface DayTable {
  /** The zone the days were bucketed in, which may not be the one asked for. */
  tz: string;
  /** How many items the timeline holds, known before any of them are fetched. */
  total: number;
  days: DayRun[];
}

/**
 * The timezone to file a photo under when its own file recorded none.
 *
 * The server cannot know this and the rule is the viewer's day, so it travels
 * with the request. Every browser that runs this app resolves a zone; the
 * fallback is for the one that does not, and the server answers UTC for
 * anything it cannot resolve either.
 */
function viewerZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

export function fetchTimelineDays(
  filter?: TimelineFilter,
  signal?: AbortSignal,
): Promise<DayTable> {
  const params = withFilter(new URLSearchParams({ tz: viewerZone() }), filter);
  return get<DayTable>(`${timelinePath(filter, "/v1/timeline/days")}?${params}`, signal);
}

/**
 * Where an asset sits in a timeline, or -1 if that timeline does not hold it.
 *
 * The one translation from an id to a position. Everything else about the
 * timeline is addressed by position — the geometry, the pages, the placeholders
 * — but a link somebody shared carries an id, and without this the only way to
 * turn one into the other is to page until it appears.
 *
 * A 404 is an answer rather than a failure: an album page handed a link to a
 * photo outside that album is the ordinary case, and -1 is what it means.
 */
export async function fetchTimelineIndex(
  id: string,
  filter?: TimelineFilter,
  signal?: AbortSignal,
): Promise<number> {
  const params = withFilter(new URLSearchParams({ id }), filter);
  try {
    const found = await get<{ index: number }>(
      `${timelinePath(filter, "/v1/timeline/locate")}?${params}`,
      signal,
    );
    return found.index;
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return -1;
    throw err;
  }
}

export interface Album {
  id: string;
  /** The importer that created it: two exports can each name a "Favorites". */
  source: string;
  title: string;
  /** What somebody typed under the name, or what an import's sidecar carried. */
  description?: string;
  count: number;
  /** The album's newest asset. Absent for an album with nothing visible in it. */
  cover_id?: string;
  /** Absent on an empty album, which has no date of its own to show. */
  newest_at?: string;
}

/**
 * A name an import carried. Not an identity: it is a label a face-grouping
 * model produced and a person confirmed, and the model was not ours.
 */
export interface Person {
  name: string;
  count: number;
  cover_id?: string;
}

/**
 * A named slice of the library — videos, favourites, screenshots. The server
 * decides which of these exist and never sends an empty one; the labels and
 * icons are the gallery's, in CategoryList.
 */
export interface Category {
  key: string;
  count: number;
  cover_id?: string;
}

export interface Collections {
  people: Person[];
  albums: Album[];
  categories: Category[];
  /** How many items are waiting in Recently Deleted. */
  trash: number;
  /**
   * How much is in each bucket, and absent while the vault is locked.
   *
   * Absent rather than zero: a locked vault does not say how much is in it, so
   * the row reads "Locked" and the number arrives only once somebody has the
   * password. See db.Collections.
   */
  vault?: Partial<Record<Bucket, number>>;
}

export function fetchCollections(signal?: AbortSignal): Promise<Collections> {
  return get<Collections>("/v1/collections", signal);
}

export function fetchAlbum(id: string, signal?: AbortSignal): Promise<Album> {
  return get<Album>(`/v1/collections/albums/${id}`, signal);
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
/**
 * The same two renditions of a Snapchat memory without its caption layer.
 *
 * The composite is what the unqualified URLs above serve, because it is the
 * picture that was sent — the photograph alone was never shown to anyone. These
 * are what the viewer reaches for when someone holds down on one, or turns the
 * overlay off.
 */
export const plainPreviewUrl = (id: string) =>
  `${MEDIA_BASE}/v1/assets/${id}/preview/plain`;
export const plainPlaybackUrl = (id: string) =>
  `${MEDIA_BASE}/v1/assets/${id}/playback/plain`;
/** A Live Photo's motion, addressed by the still's id, in the same sizes. */
export const liveThumbUrl = (id: string, size?: ThumbSize) =>
  size === undefined || size === BASE_THUMB_SIZE
    ? `${MEDIA_BASE}/v1/assets/${id}/live/thumb`
    : `${MEDIA_BASE}/v1/assets/${id}/live/thumb/${size}`;
export const livePreviewUrl = (id: string) => `${MEDIA_BASE}/v1/assets/${id}/live/preview`;
export const playbackUrl = (id: string) => `${MEDIA_BASE}/v1/assets/${id}/playback`;
export const originalUrl = (id: string) => `${MEDIA_BASE}/v1/assets/${id}/original`;

export { ApiError };

// The write half of the gallery's API: everything that can move a photograph
// out of the library, back into it, or off the disk.
//
// Only ever reached through the Next rewrite, which is the same loopback
// listener the reads go through. See api.galleryRoutes for why photod serves
// these without a token there and what that costs.

/**
 * What an operation applies to, said either way round.
 *
 * Ids are exact and are what the grid sends for a tile it is holding. Ranges
 * are positions in the timeline the grid is drawn from, which is the only way
 * to name a selection covering photographs the browser has never fetched — the
 * day table gives every item in a collection a place before any of them are
 * downloaded, so "everything below here" is one interval rather than forty
 * thousand identifiers.
 *
 * The filter travels with the ranges because a position means nothing without
 * it: index 2 is a different photograph inside an album than in the library.
 */
export interface Target {
  ids?: string[];
  ranges?: readonly { start: number; end: number }[];
  filter?: TimelineFilter;
}

/** The body every selection endpoint takes. The scope is the endpoint's. */
function selectionBody(target: Target): Record<string, unknown> {
  const body: Record<string, unknown> = {};
  if (target.ids?.length) body.ids = target.ids;
  if (target.ranges?.length) body.ranges = target.ranges;

  // The scope — library, trash, or a bucket — is the endpoint's, so only the
  // collection a position was counted in travels in the body. Inside the vault
  // that collection is nested one level down, because the bucket itself is
  // already in the path.
  const filter = target.filter;
  if (filter?.kind === "vault") {
    if (filter.within) body[FILTER_PARAM[filter.within.kind]] = filter.within.value;
  } else if (filter && filter.kind !== "trash") {
    body[FILTER_PARAM[filter.kind]] = filter.value;
  }
  return body;
}

async function send<T>(path: string, body: unknown, method = "POST"): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw new ApiError(res.status, await errorText(res));
  return res.json() as Promise<T>;
}

/**
 * What a delete hands back. The batch is the undo: not the ids it took, because
 * a selection can be the whole library and the point of naming it by position
 * was not to have to enumerate it.
 */
export interface Deleted {
  batch: string;
  deleted: number;
  /** Set only by an album delete: how many album rows went with the photos. */
  albums?: number;
}

/** Moves a selection to Recently Deleted, where it waits a year. */
export function deleteItems(target: Target): Promise<Deleted> {
  return send<Deleted>("/v1/trash", selectionBody(target));
}

export interface Restored {
  restored: number;
  albums?: number;
}

/** Puts a selection back into the library, from inside the trash. */
export function restoreItems(target: Target): Promise<Restored> {
  return send<Restored>("/v1/trash/restore", selectionBody(target));
}

/**
 * Undoes one delete, exactly — including the paired videos it carried along and
 * excluding anything that was already deleted when it ran.
 *
 * By batch rather than by selection because by the time Undo is clicked the
 * selection is gone and the grid has been redrawn around what is left, so every
 * position in it means something else. The batch still means what it meant.
 */
export function undoDelete(batch: string): Promise<Restored> {
  return send<Restored>("/v1/trash/restore", { batch });
}

export interface Purged {
  purged: number;
  bytes: number;
}

/** Destroys a selection outright. Only reaches things already in the trash. */
export function purgeItems(target: Target): Promise<Purged> {
  return send<Purged>("/v1/trash/purge", selectionBody(target));
}

/**
 * Removes an album. With `photos`, everything in it goes to the trash too.
 *
 * One batch covers both, so the undo puts the album and its photographs back
 * together — an album restored empty, or photos restored into an album that no
 * longer exists, would be a worse outcome than either half.
 */
export function deleteAlbum(id: string, photos: boolean): Promise<Deleted> {
  return send<Deleted>(
    `/v1/collections/albums/${id}${photos ? "?photos=true" : ""}`,
    undefined,
    "DELETE",
  );
}

// The vault: the Archive and Hidden buckets, and the lock in front of them.
//
// Split exactly where the server splits it. Everything that puts a photograph
// in works while the vault is locked — that is what makes "Archive" a
// right-click rather than a password prompt — and everything that reads one
// answers 423 until somebody unlocks it. The gallery turns that status into the
// prompt, which is why it is the one status this file names.

/** The three states the vault can be in, as one answer. */
export interface VaultStatus {
  /** False before anything has ever been hidden: there is no password yet. */
  exists: boolean;
  unlocked: boolean;
  /** When the key will be dropped if nothing touches it. */
  expires_at?: string;
}

/** 423 Locked. Thrown by every vault read while the vault is shut. */
export const LOCKED = 423;
/** 428 Precondition Required: an archive that has no vault yet. */
export const NO_VAULT = 428;

export function fetchVaultStatus(signal?: AbortSignal): Promise<VaultStatus> {
  return get<VaultStatus>("/v1/vault", signal);
}

/** Creates the vault. Called from the first hide, and leaves it unlocked. */
export function setupVault(password: string): Promise<VaultStatus> {
  return send<VaultStatus>("/v1/vault/setup", { password });
}

export function unlockVault(password: string): Promise<VaultStatus> {
  return send<VaultStatus>("/v1/vault/unlock", { password });
}

export function lockVault(): Promise<VaultStatus> {
  return send<VaultStatus>("/v1/vault/lock", {});
}

/**
 * Changes the password without re-encrypting anything: the keypair is the same
 * one every file in the vault was sealed to, and only its wrapping is rewritten.
 */
export function changeVaultPassword(password: string, next: string): Promise<VaultStatus> {
  return send<VaultStatus>("/v1/vault/password", { password, new: next });
}

/** What a hide hands back. The batch is the Undo, exactly as a delete's is. */
export interface Vaulted {
  batch: string;
  moved: number;
  albums?: number;
  people?: number;
}

/** Hides a selection of photographs. Works on a locked vault. */
export function vaultItems(bucket: Bucket, target: Target): Promise<Vaulted> {
  return send<Vaulted>(`/v1/vault/${bucket}`, selectionBody(target));
}

/**
 * Hides an album and everything in it, under one batch — so the Undo puts the
 * album and its photographs back together.
 */
export function vaultAlbum(bucket: Bucket, id: string): Promise<Vaulted> {
  return send<Vaulted>(`/v1/vault/${bucket}/albums/${id}`, {});
}

/**
 * Hides everyone a name is tagged on, and the name.
 *
 * The name is in the body rather than the path because it is somebody's name:
 * it can hold a slash or a right-to-left mark, and a path segment is the wrong
 * place to find that out.
 */
export function vaultPerson(bucket: Bucket, name: string): Promise<Vaulted> {
  return send<Vaulted>(`/v1/vault/${bucket}/people`, { name });
}

export interface Unvaulted {
  restored: number;
  albums?: number;
  people?: number;
}

/**
 * Takes things back out, four ways: a selection inside the vault's own grid, a
 * batch from a toast's Undo, or a whole album or person.
 *
 * This one needs the password, which is the asymmetry the whole feature rests
 * on — a restore has to decrypt the bytes to write them back.
 */
export function unvault(
  what:
    | { bucket: Bucket; ids?: string[]; ranges?: readonly { start: number; end: number }[]; filter?: CollectionFilter }
    | { batch: string }
    | { bucket: Bucket; album: string }
    | { bucket: Bucket; person: string },
): Promise<Unvaulted> {
  const body: Record<string, unknown> = { ...what };
  // The collection a range was counted in travels under its own key, because
  // the top level already spends `album` and `person` on "restore this whole
  // grouping" — two different questions that would otherwise share a word.
  if ("filter" in what && what.filter) {
    delete body.filter;
    body.filter = { [FILTER_PARAM[what.filter.kind]]: what.filter.value };
  }
  return send<Unvaulted>("/v1/vault/restore", body);
}

// Albums, as a thing the gallery can make rather than only read.
//
// Every one of these is spelled twice on the wire — once under /v1/collections
// and once under /v1/vault/{bucket} — because they are two different writes.
// The library's membership is a table; a hidden photograph's is a line inside
// its sealed document, which only the password can open. The bucket argument is
// the whole of what chooses, and it comes from the grid the menu opened over.

const albumsPath = (bucket?: Bucket) =>
  bucket ? `/v1/vault/${bucket}/albums` : "/v1/collections/albums";

/**
 * The albums somewhere can be added to, for the "Add to album" menu.
 *
 * The library has an endpoint of its own for this, because the menu opens over
 * a grid and has no use for the people or the category covers the collections
 * page asks for in the same breath. A bucket has no such shortcut and could not
 * usefully have one: its albums are counted by opening every sealed document in
 * it, which is the same work its collections page already does.
 */
export function fetchAlbums(bucket?: Bucket, signal?: AbortSignal): Promise<Album[]> {
  if (bucket) return fetchVaultCollections(bucket, signal).then((page) => page.albums);
  return get<{ albums: Album[] }>("/v1/collections/albums", signal).then((r) => r.albums ?? []);
}

/** A new album, and how much of the selection that made it went in. */
export interface CreatedAlbum extends Album {
  added?: number;
}

/**
 * Makes an album, and fills it in the same request when there is something to
 * put in it.
 *
 * One request rather than two because the common way to make an album is out of
 * a selection — right-click, Add to album, Create "Iceland" — and splitting
 * that in half leaves a failure mode where the album exists and is empty.
 */
export function createAlbum(
  spec: { title: string; description?: string; bucket?: Bucket },
  target?: Target,
): Promise<CreatedAlbum> {
  const body: Record<string, unknown> = {
    title: spec.title,
    description: spec.description ?? "",
    ...(target ? selectionBody(target) : {}),
  };
  return send<CreatedAlbum>(albumsPath(spec.bucket), body);
}

/**
 * What a membership change did. A count of what actually moved, so a client can
 * tell "added forty" from "all forty were already in there".
 */
export interface Membership {
  added?: number;
  removed?: number;
}

export function addToAlbum(album: string, target: Target, bucket?: Bucket): Promise<Membership> {
  return send<Membership>(`${albumsPath(bucket)}/${album}/items`, selectionBody(target));
}

/**
 * Takes a selection back out of an album.
 *
 * A DELETE with a body, which is the same shape the purge endpoint refused for
 * the opposite reason: a selection *is* a body, and the verb that says "take
 * these out" has nowhere else to carry what to take out.
 *
 * Nothing is deleted. The photographs stay in the library, in their other
 * albums and in the timeline — which is why there is no batch to undo.
 */
export function removeFromAlbum(
  album: string,
  target: Target,
  bucket?: Bucket,
): Promise<Membership> {
  return send<Membership>(`${albumsPath(bucket)}/${album}/items`, selectionBody(target), "DELETE");
}

/**
 * Which albums hold one photograph, by id.
 *
 * Ids rather than the titles the viewer's panel shows, because this decides
 * which rows of a menu get a tick and two imports can each have contributed a
 * "Favorites". A hidden photograph answers from its sealed document, so this
 * is 423 while the vault is shut.
 */
export function fetchAssetAlbums(id: string, signal?: AbortSignal): Promise<string[]> {
  return get<{ albums: string[] }>(`/v1/assets/${id}/albums`, signal).then((r) => r.albums ?? []);
}

/** The vault's own collections page, plus how much is in the bucket overall. */
export interface VaultCollections extends Collections {
  total: number;
}

export function fetchVaultCollections(
  bucket: Bucket,
  signal?: AbortSignal,
): Promise<VaultCollections> {
  return get<VaultCollections>(`/v1/vault/${bucket}/collections`, signal);
}
