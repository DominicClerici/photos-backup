// Mirrors the JSON photod actually emits. These types are hand-written rather
// than generated because the surface is small and stable; if it grows, the Go
// structs in internal/db and internal/api are the source of truth.

// The sizes live in layout.ts, beside the zoom levels that choose between them.
// Nothing is imported back the other way at runtime — layout.ts takes only a
// type from here — so this is a dependency rather than a cycle.
import { BASE_THUMB_SIZE, type ThumbSize } from "../lib/layout.ts";
import { baseUrl, headers, unauthorized } from "./transport.ts";

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
  /**
   * Where those coordinates are, in words, resolved offline from a GeoNames
   * extract. Three fields rather than one string because the panel joins them
   * its own way; all absent on the 38% of the library with no GPS fix, and on
   * anything nobody has geocoded yet.
   */
  place_city?: string;
  place_admin1?: string;
  place_country?: string;
  // What an import knew and the file did not: a Google Takeout carries the
  // caption, the star, and the album a photo was in in a JSON sidecar rather
  // than in the photo. All absent on anything the phone delivered directly.
  description?: string;
  favorite?: boolean;
  archived?: boolean;
  albums?: string[];
  people?: string[];
  /**
   * Who put this into an iCloud Shared Album, on the phone it came off.
   *
   * Absent on everything else, which is nearly everything. It is the only
   * provenance a shared photograph has — Apple re-encodes what goes into a
   * shared album, so there is nothing in the file that knows, and the name is
   * gone the day the album is left.
   */
  contributor?: string;
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

// Where the archive is and what proves who is asking are the transport's, not
// this file's — see transport.ts. Both are read per request rather than held,
// which is what lets a phone's address change under a running app.
//
// The browser reaches photod at /api and the phone at /v1; they are the same
// handler behind a StripPrefix, so the difference is entirely in the base and
// no path in this file knows which client it is serving.

/**
 * A field and an assignment rather than a parameter property, which is the one
 * shape of TypeScript node cannot strip: `node --test` runs the tests in this
 * package by erasing types and nothing else, and it would refuse this file for
 * `constructor(readonly status: number)` alone. Nothing else here needs saying
 * about it — no enums, no namespaces, no parameter properties, and the whole
 * package stays runnable without a build.
 */
class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

/**
 * Every failed response is built here, so that a 401 is noticed exactly once
 * and in one place.
 *
 * photod answers 401 when a session has ended — the idle window elapsed, the
 * absolute cap was reached, the browser was closed and reopened, or somebody
 * ran `photobackup web --revoke-all`. There is nothing the gallery can do about
 * that except go and ask for a new one, and it cannot render anything
 * meaningful in the meantime: every request it has in flight is about to fail
 * the same way.
 *
 * The sign-in page is served by photod rather than by Next — see
 * internal/api/frontdoor.go — which is why this is a location assignment rather
 * than a router push. There is no route on this side to push to.
 */
export function apiError(status: number, message: string): ApiError {
  if (status === 401) sessionEnded();
  return new ApiError(status, message);
}

/**
 * Says the credential is dead, once.
 *
 * Guarded, because a grid firing two hundred thumbnail requests will see two
 * hundred 401s within a few milliseconds of each other, and each one must not
 * queue its own navigation — or, on a phone, its own trip to the pairing
 * screen. What actually happens is the transport's business; that it happens
 * exactly once is this file's.
 */
let leaving = false;
function sessionEnded() {
  if (leaving) return;
  leaving = true;
  unauthorized();
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(`${baseUrl()}${path}`, { headers: headers(), signal });
  if (!res.ok) throw apiError(res.status, await errorText(res));
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
 * The order a timeline is read in.
 *
 * Two of them are the same walk through time in opposite directions, which the
 * server answers from the index either way. The other two order by length, have
 * no index behind them, and are deliberately allowed to be slower: they are
 * what somebody reaches for once to find the twenty-minute video, not what they
 * scroll a hundred thousand photographs in.
 */
export type SortKey = "newest" | "oldest" | "longest" | "shortest";

/**
 * How a grid is being looked at, as opposed to what it is a grid *of*.
 *
 * Kept apart from TimelineFilter because the two answer different questions and
 * change on different schedules. The filter is the place — this album, the
 * trash, the Hidden bucket — and it changes when the route does. The view is
 * the order and the narrowing somebody has chosen while standing in that place,
 * and it changes under their hands without going anywhere.
 *
 * Every field but the order is an adjective, and they combine: "the videos in
 * this album that are in no other album" is one request. Which is what makes
 * this a record rather than a second union.
 */
export interface View {
  sort: SortKey;
  /** One medium, or both when absent. */
  media?: MediaKind;
  favorites?: boolean;
  /** In no album — the pile left over after the organising. */
  unalbumed?: boolean;
}

/**
 * Writes a view into a query string.
 *
 * The default order is written as nothing rather than as "newest", which keeps
 * the URL of an ordinary timeline exactly what it was before any of this
 * existed — and keeps that request's cache entry from splitting in two.
 */
function withView(params: URLSearchParams, view?: View): URLSearchParams {
  if (!view) return params;
  if (view.sort !== "newest") params.set("sort", view.sort);
  if (view.media) params.set("kind", view.media);
  if (view.favorites) params.set("favorites", "1");
  if (view.unalbumed) params.set("unalbumed", "1");
  return params;
}

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
  view?: View,
  signal?: AbortSignal,
): Promise<TimelinePage> {
  const params = new URLSearchParams({ limit: String(limit) });
  if ("cursor" in start) params.set("cursor", start.cursor);
  else if (start.skip > 0) params.set("skip", String(start.skip));
  withFilter(params, filter);
  withView(params, view);
  return get<TimelinePage>(`${timelinePath(filter, "/v1/timeline")}?${params}`, signal);
}

/** One heading the timeline will draw, and how many tiles hang under it. */
export interface DayRun {
  /**
   * The local calendar day, YYYY-MM-DD. Rendered here, not by the server.
   *
   * Empty means this run has no date and draws no heading, which is what a
   * timeline ordered by something other than time is: the days are still in
   * there, scattered through an order that has nothing to do with the calendar,
   * and a heading per tile would be a ruin of that shape rather than a
   * description of it. See lib/layout.headless.
   */
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
  view?: View,
  signal?: AbortSignal,
): Promise<DayTable> {
  const params = withFilter(new URLSearchParams({ tz: viewerZone() }), filter);
  withView(params, view);
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
  view?: View,
  signal?: AbortSignal,
): Promise<number> {
  const params = withView(withFilter(new URLSearchParams({ id }), filter), view);
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

/** One word a model wrote, as it is searched and as it was written. */
export interface AnalysisTag {
  /** What a search resolves this to, after any merge. */
  name: string;
  /**
   * What the captioner actually wrote, present only when a merge has folded it
   * into something else. Seeing that a photograph was called "puppy" and is
   * findable as "dog" is what makes the tag cleanup reviewable.
   */
  raw?: string;
  confidence?: number;
}

/**
 * What the ML passes have said about one photograph — the mirror of a search.
 *
 * Every field is optional and an absent one is not the same as an empty one,
 * which is what `jobs` is for: a photograph with no caption may be one the
 * captioner has not reached, or one it failed on, and a panel that drew both as
 * blank would be a filter with no evidence of itself.
 */
export interface AssetAnalysis {
  caption?: string;
  caption_model?: string;
  captioned_at?: string;
  tags?: AnalysisTag[];
  /** Everything the text recogniser read, unabridged. */
  text?: string;
  text_model?: string;
  read_at?: string;
  /** How many vectors the encoder wrote: 1 for a still, one per sampled frame. */
  frames?: number;
  vision_model?: string;
  /** State of each ML job that has a row: mlprep, vision, ocr, describe. */
  jobs?: Record<string, string>;
}

/**
 * Fetched on its own rather than with the detail, because the panel that draws
 * it is a toggle and recognised text is unbounded — a screenshot of a terminal
 * is kilobytes of it, and nobody arrow-keying through the viewer with the panel
 * shut should be carrying that.
 */
export function fetchAnalysis(
  id: string,
  signal?: AbortSignal,
): Promise<AssetAnalysis> {
  return get<AssetAnalysis>(`/v1/assets/${id}/analysis`, signal);
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
  const res = await fetch(`${baseUrl()}/v1/timeline/states`, {
    method: "POST",
    headers: { ...headers(), "Content-Type": "application/json" },
    body: JSON.stringify({ ids }),
    signal,
  });
  if (!res.ok) throw apiError(res.status, await errorText(res));
  const body = (await res.json()) as { items: TimelineItem[] };
  return body.items ?? [];
}

/**
 * Where a photograph was taken, at whichever of the three levels the query
 * named it. Exactly one field is set — see searchquery.Place.
 */
export interface SearchPlace {
  city?: string;
  admin1?: string;
  country?: string;
}

/**
 * What the server thought the question was.
 *
 * Echoed back with every result page, and the whole of why the chips can exist:
 * a parser fails *confidently* — it decides "last summer" meant 2024, silently
 * removes the right answer, and shows an empty grid. Sending the reading back
 * out is what lets somebody see it and click the × . See internal/api/search.go.
 */
export interface ParsedQuery {
  /** What was typed, verbatim. */
  text: string;
  people?: string[];
  place?: SearchPlace;
  tags?: string[];
  /** Civil days, YYYY-MM-DD, both inclusive. */
  after?: string;
  before?: string;
  kind?: MediaKind;
  category?: string;
  favorites?: boolean;
  /**
   * The leftover phrase that went to the encoder, which is not what was typed.
   * Echoed because "why did searching for my dog return the ocean" is answered
   * by seeing that the phrase became "ocean".
   */
  visual?: string;
  /** "grammar" when the Go parser answered alone, "model" when photo-ml added. */
  source: string;
}

/**
 * One ranked photograph, and why it is here.
 *
 * A timeline item with the evidence bolted on, so the grid draws a search
 * result with the component it draws everything else with.
 */
export interface SearchResult extends TimelineItem {
  /** The fused rank. Comparable within one result set and meaningless between two. */
  score: number;
  /** Cosine similarity to the query phrase, absent when nothing was embedded. */
  similarity?: number;
  caption?: string;
  tags?: string[];
  /** The matching stretch of recognised text, with the match marked by []. */
  snippet?: string;
}

export interface SearchPage {
  query: ParsedQuery;
  items: SearchResult[];
  /**
   * How many candidates the ranking had to choose from, not how many
   * photographs in the archive could conceivably match. For a fused ranking
   * that is the honest number.
   */
  total: number;
  /** What this search could not do, in a sentence. Empty when nothing was lost. */
  degraded?: string;
}

/**
 * One page of a ranked search.
 *
 * The request is the caller's own parameters rather than a record, because they
 * are the API's: `q` alone asks the server to parse, and the explicit spelling
 * — `parse=0` beside `person`, `after`, `visual` and the rest — is what a chip
 * row edits. Materialising the parse into parameters is the only way to say
 * "and not the date it found"; see lib/search.
 */
export function fetchSearch(
  params: URLSearchParams,
  limit: number,
  offset: number,
  signal?: AbortSignal,
): Promise<SearchPage> {
  // Copied through its spelling rather than handed to the constructor.
  // React Native's `URLSearchParams` is its own small implementation, and its
  // object branch would read the own properties of the instance it was given —
  // producing one parameter called `_searchParams` holding "[object Map]".
  // A string is the one argument both implementations agree about.
  const query = new URLSearchParams(params.toString());
  query.set("limit", String(limit));
  if (offset > 0) query.set("offset", String(offset));
  return get<SearchPage>(`/v1/search?${query}`, signal);
}

/**
 * Which rendition of an asset. Every one photod serves, spelled the way the
 * path spells it, so that `media` is a template rather than a switch.
 *
 * The plain pair are a Snapchat memory without its caption layer. The composite
 * is what the unqualified names serve, because it is the picture that was sent
 * — the photograph alone was never shown to anyone. These are what the viewer
 * reaches for when somebody holds down on one, or turns the overlay off.
 *
 * `live/*` is a Live Photo's motion, addressed by the still's id: the paired
 * video is never an item of its own.
 */
export type MediaVariant =
  | "thumb"
  | `thumb/${ThumbSize}`
  | "preview"
  | "preview/plain"
  | "playback"
  | "playback/plain"
  | "live/thumb"
  | `live/thumb/${ThumbSize}`
  | "live/preview"
  | "original";

/**
 * Where a rendition is, and what it takes to be allowed to have it.
 *
 * The two travel together because on a phone they are inseparable: expo-image
 * and expo-video both take `{ uri, headers }`, and a thumbnail fetched without
 * the same header the timeline went out with 401s inside a grid that rendered
 * fine. A browser spreads the uri onto an `<img src>` and ignores the headers,
 * because its cookie is already attached and it could not have added one
 * anyway.
 *
 * That is the entire difference between the two clients' media paths, and it
 * is one line at each call site rather than a second client.
 */
export interface MediaSource {
  uri: string;
  headers: Record<string, string>;
}

/**
 * One rendition of one asset.
 *
 * This replaced eight `…Url(id)` helpers and a `MEDIA_BASE` that let media skip
 * the Next rewrite. The skip is gone with the rewrite — photod is the front
 * door now and serves the bytes itself — so there was one base left and no
 * reason for eight functions over it.
 */
export function media(id: string, variant: MediaVariant): MediaSource {
  return { uri: `${baseUrl()}/v1/assets/${id}/${variant}`, headers: headers() };
}

/**
 * The variant for a thumbnail at a size. No size, or the base size, is the
 * unsized route: the same URL the gallery has always used, so the bytes stay in
 * one cache entry rather than two.
 *
 * The base is also the only size every asset is guaranteed to have. A library
 * ingested before the others existed gets them when a backfill runs, and until
 * then asking for one is a 404 the caller falls back from.
 */
export function thumbVariant(size?: ThumbSize): "thumb" | `thumb/${ThumbSize}` {
  return size === undefined || size === BASE_THUMB_SIZE ? "thumb" : `thumb/${size}`;
}

/** The same rule, for the motion half of a Live Photo. */
export function liveThumbVariant(size?: ThumbSize): "live/thumb" | `live/thumb/${ThumbSize}` {
  return size === undefined || size === BASE_THUMB_SIZE ? "live/thumb" : `live/thumb/${size}`;
}

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
  /**
   * How that timeline was being looked at when the positions were counted.
   *
   * Travels for the same reason the filter does and is the same kind of mistake
   * to leave behind: position 2 of a grid sorted oldest-first, or of one
   * showing only the videos, is a different photograph from position 2 of the
   * library. A range means nothing without the whole description of the grid it
   * was drawn in.
   */
  view?: View;
}

/** The body every selection endpoint takes. The scope is the endpoint's. */
function selectionBody(target: Target): Record<string, unknown> {
  const body: Record<string, unknown> = {};
  if (target.ids?.length) body.ids = target.ids;
  if (target.ranges?.length) body.ranges = target.ranges;

  const view = target.view;
  if (view) {
    if (view.sort !== "newest") body.sort = view.sort;
    if (view.media) body.kind = view.media;
    if (view.favorites) body.favorites = true;
    if (view.unalbumed) body.unalbumed = true;
  }

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
  const res = await fetch(`${baseUrl()}${path}`, {
    method,
    headers: { ...headers(), "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw apiError(res.status, await errorText(res));
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
    | {
        bucket: Bucket;
        ids?: string[];
        ranges?: readonly { start: number; end: number }[];
        filter?: CollectionFilter;
        view?: View;
      }
    | { batch: string }
    | { bucket: Bucket; album: string }
    | { bucket: Bucket; person: string },
): Promise<Unvaulted> {
  const body: Record<string, unknown> = { ...what };
  // The whole description of the grid a range was counted in travels under one
  // key, because the top level already spends `album` and `person` on "restore
  // this whole grouping" — two different questions that would otherwise share a
  // word. Nothing but a range has any use for it: a restore by id names its
  // photographs exactly, whatever order they were being looked at in.
  delete body.filter;
  delete body.view;
  if ("ranges" in what && what.ranges?.length) {
    const collection = what.filter ? { [FILTER_PARAM[what.filter.kind]]: what.filter.value } : {};
    const view = what.view;
    body.filter = {
      ...collection,
      ...(view?.sort && view.sort !== "newest" ? { sort: view.sort } : {}),
      ...(view?.media ? { kind: view.media } : {}),
      ...(view?.favorites ? { favorites: true } : {}),
      ...(view?.unalbumed ? { unalbumed: true } : {}),
    };
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

// The archive's opinion about what ought to be one item and is several. Two
// kinds, one shape, and only one of them is a question anybody is asked — see
// internal/merge on why a split recording is put back together without being
// approved and a set of duplicates is not.

/** Which kind of group. Not interchangeable; see the review page. */
export type MergeKind = "duplicate" | "video-segments";

/**
 * What has happened to a group, plus one thing that has not.
 *
 * "failed" is not a fourth state a row can be in: it is the pending groups
 * whose join gave up, which is a fact in the jobs table rather than this one. It
 * is asked for by name because the joined recordings tab shows those first —
 * they are the only rows on that page where anything is still owed.
 */
export type MergeState = "pending" | "merged" | "dismissed" | "failed";

/** The last attempt at a join, as much of it as belongs beside the recording. */
export interface MergeFailure {
  job: number;
  attempts: number;
  error: string;
  failed_at: string;
}

/**
 * One candidate, described in the terms the choice is actually made in.
 *
 * Everything here is on the wire because the comparison is made from it. A page
 * that had to fetch each member's detail separately would issue a hundred
 * requests to draw one burst.
 */
export interface MergeMember {
  id: string;
  position: number;
  filename: string;
  kind: MediaKind;
  width?: number;
  height?: number;
  byte_size: number;
  duration?: number;
  taken_at: string;
  import_source?: string;
  favorite?: boolean;
  /**
   * What a discarded copy would take with it, if the merge did not carry them
   * across. It does — but "this one is in three albums" is exactly the sort of
   * thing that makes somebody pick a different keeper.
   */
  albums?: string[];
  people?: string[];
  /** "live" or "deleted", so a resolved group can still be drawn honestly. */
  state: string;
}

export interface MergeGroup {
  id: string;
  kind: MergeKind;
  state: MergeState;
  detected_at: string;
  /** The copy that was kept, or the recording that was built. */
  keeper_asset_id?: string;
  /**
   * When somebody read this entry of the joined recordings log and was content
   * with it. Absent until they do, which is what keeps it on the list.
   */
  approved_at?: string;
  /** True when the join was archived over the objection of the duration check. */
  forced?: boolean;
  /** The merge job that gave up on this group, when one has. */
  failure?: MergeFailure;
  /**
   * True when the join this group's last attempt refused to archive is still on
   * disk to be watched. False for every failure that never produced a file,
   * which is why the page asks rather than assuming.
   */
  preview?: boolean;
  members: MergeMember[];
}

/** How much of the library the scan could see when it last ran. */
export interface SignatureCoverage {
  assets: number;
  signed: number;
}

/**
 * What the status page's review cards say.
 *
 * Groups and photographs are both here because they answer different questions:
 * the first is how many decisions are waiting, the second is how much is at
 * stake in them.
 */
export interface MergeCounts {
  pending_duplicates: number;
  duplicate_items: number;
  pending_segments: number;
  merged_segments: number;
  /** Joins that gave up. Apart from the two above: this one waits forever. */
  failed_segments: number;
  /** Joins that have been read and signed off, and so counted nowhere else. */
  approved_segments: number;
  merged_duplicates: number;
  coverage: SignatureCoverage;
}

export function fetchMergeCounts(signal?: AbortSignal): Promise<MergeCounts> {
  return get<MergeCounts>("/v1/merges", signal);
}

export function fetchMergeGroups(
  kind: MergeKind = "duplicate",
  state: MergeState = "pending",
  { approved = false, signal }: { approved?: boolean; signal?: AbortSignal } = {},
): Promise<MergeGroup[]> {
  const params = new URLSearchParams({ kind, state });
  // Off by default on the server too. Sent explicitly anyway, because "show
  // approved" is a switch somebody flicked and the request should read like it.
  if (approved) params.set("approved", "true");
  return get<{ groups: MergeGroup[] }>(`/v1/merges/groups?${params}`, signal).then(
    (r) => r.groups ?? [],
  );
}

/** What a resolved group did. The batch is the undo, as everywhere else here. */
export interface Merged {
  keeper: string;
  batch: string;
  trashed: number;
}

/**
 * Keeps one copy and moves the rest to Recently Deleted, carrying their albums,
 * people, caption and favourite onto the one that stays first.
 *
 * The keeper is always sent, never defaulted. The order the members arrive in is
 * the server's opinion about which is the better copy and the page preselects
 * it, so a request that named nothing would be ambiguous between agreeing with
 * the suggestion and forgetting the field.
 */
export function mergeGroup(group: string, keeper: string): Promise<Merged> {
  return send<Merged>(`/v1/merges/${group}/merge`, { keeper });
}

/** Records that these are different photographs, and stops them being paired again. */
export async function dismissGroup(group: string): Promise<void> {
  const res = await fetch(`${baseUrl()}/v1/merges/${group}/dismiss`, {
    method: "POST",
    headers: headers(),
  });
  if (!res.ok) throw apiError(res.status, await errorText(res));
}

/** What an undo put back, and what it took away in exchange. */
export interface Unmerged {
  restored: number;
  /** The joined recording that has now gone to the trash. Absent for duplicates. */
  keeper?: string;
}

/**
 * Undoes a merge: the pieces come back out of the trash, and a joined recording
 * goes into it.
 *
 * The undo for the half of this that happens without being asked. It leaves the
 * group dismissed rather than pending — a pending set of segments would be
 * re-joined by the worker within the minute, and the undo would appear not to
 * have worked.
 */
export function unmergeGroup(group: string): Promise<Unmerged> {
  return send<Unmerged>(`/v1/merges/${group}/undo`, {});
}

/**
 * Records that this entry of the joined recordings log has been read, or takes
 * that back.
 *
 * It changes nothing about the photographs — the pieces stay in the trash, the
 * joined recording stays in the library, and splitting it back up goes on
 * working. That is what makes it one click with no confirmation.
 */
export async function approveGroup(group: string, approved: boolean): Promise<void> {
  const path = approved ? "approve" : "unapprove";
  const res = await fetch(`${baseUrl()}/v1/merges/${group}/${path}`, {
    method: "POST",
    headers: headers(),
  });
  if (!res.ok) throw apiError(res.status, await errorText(res));
}

/**
 * Archives a join the server refused to make.
 *
 * The refusal is arithmetic: the concatenated file did not come out the length
 * its parts add up to, which is indistinguishable from ffmpeg having dropped
 * one — by arithmetic. Watching it tells them apart, so this is only ever
 * pressed after the preview, and it queues the join again with the objection
 * disabled rather than doing it here.
 */
export function forceJoin(group: string): Promise<{ queued: boolean }> {
  return send<{ queued: boolean }>(`/v1/merges/${group}/force`, {});
}

/**
 * The join a merge job built and then refused to archive.
 *
 * The one video in this API that belongs to no asset: it was never committed,
 * never indexed, and is not in the library. It is served anyway because the
 * decision it is evidence for cannot be made any other way.
 */
export const joinPreviewUrl = (group: string) => `${baseUrl()}/v1/merges/${group}/preview`;

/** What one sweep of the library found. */
export interface ScanResult {
  segments: number;
  duplicates: number;
  queued: number;
  signed: number;
  assets: number;
}

/**
 * Looks over the whole library again.
 *
 * Synchronous and slow — a few seconds — because the page that called it is
 * about to redraw from what it found. It is a button rather than a timer
 * because the things that create work here (an import, a signature backfill)
 * both end.
 */
export function scanForMerges(): Promise<ScanResult> {
  return send<ScanResult>("/v1/merges/scan", {});
}

/** One filesystem, as the server's kernel reports it. */
export interface Volume {
  path: string;
  total: number;
  used: number;
  free: number;
}

/**
 * The drive, and what of it this archive accounts for.
 *
 * `used` and `free` close on `total` by construction, so a pie drawn from them
 * has no gap. What the four attributed figures do *not* close on is `used`:
 * the database, the vault, and the blocks the filesystem reserves for root are
 * all real bytes with no card of their own, and the remainder is where they go.
 */
export interface StorageStatus {
  archive: Volume;
  derivatives: Volume;
  /** Whether the renditions are on the same disk as the originals. */
  same_volume: boolean;
  photos: number;
  videos: number;
  photo_derivatives: number;
  video_derivatives: number;
  unattributed_derivatives: number;
  /** When the derivative walk behind those last three figures last ran. */
  measured_at: string;
}

export interface JobCount {
  kind: string;
  state: string;
  count: number;
}

export interface QueueStatus {
  pending: number;
  running: number;
  failed: number;
  kinds: JobCount[];
}

/** Something wrong with the server rather than with one photograph. */
export interface Problem {
  id: string;
  severity: "error" | "warning";
  title: string;
  detail: string;
}

/** One job that gave up, and the asset it gave up on. */
export interface Failure {
  id: number;
  kind: string;
  asset_id: string;
  attempts: number;
  error: string;
  failed_at: string;
  filename?: string;
  media_kind?: MediaKind;
  /** False when there is no thumbnail to draw: the asset is vaulted or gone. */
  viewable: boolean;
}

export interface LibraryStats {
  items: number;
  photos: number;
  videos: number;
  trashed: number;
}

/**
 * The status page, in one answer.
 *
 * One request rather than five, because every card is a claim about the same
 * instant — see the note on statusResponse in the server.
 */
export interface Status {
  library: LibraryStats;
  storage: StorageStatus;
  queue: QueueStatus;
  problems: Problem[];
  failures: Failure[];
}

export function fetchStatus(signal?: AbortSignal): Promise<Status> {
  return get<Status>("/v1/status", signal);
}

// The tag cleanup — ML_IMAGES.md §9, and the one part of this archive where
// what a model wrote is edited rather than displayed.
//
// Two passes with a review after each. The words are judged useful or not, then
// what survives is clustered and the near-identical spellings are folded into
// one. Neither destroys anything: `junk` and `canonical_id` are read at every
// point of search, so both take effect everywhere at once and are undone the
// same way.

/** One word of the vocabulary, with everything the review is decided on. */
export interface TagWord {
  id: number;
  name: string;
  /** How many photographs carry it. The stakes, and the order both lists read in. */
  uses: number;
  junk?: boolean;
  /** What the captioner thought, 0 to 1. Absent until something has judged it. */
  score?: number;
  /** When a model judged it, and when a person did. The gap is the whole point. */
  triaged_at?: string;
  judged_at?: string;
  /** The word this one was folded into. Only on the merged log. */
  canonical?: string;
  /** How near this word sits to the head of the proposal it is in. */
  similarity?: number;
  /** A few photographs carrying it, as ids — the evidence behind a verdict. */
  samples?: string[];
}

/** A group of words that mean one thing, with the one they should become first. */
export interface TagProposal {
  canonical: TagWord;
  members: TagWord[];
  /** The group's claims added up: how many photographs the merge is worth. */
  uses: number;
}

/** The cleanup in nine numbers. Every write answers with a fresh copy. */
export interface TagCounts {
  vocabulary: number;
  claims: number;
  /** The two review lists. They do not add up to `vocabulary`: a folded word is on neither. */
  kept: number;
  junk: number;
  /** What the analyse pass has left, and what approving is for. */
  untriaged: number;
  unreviewed: number;
  /** Kept words with no vector yet, which is what the clustering is missing. */
  unembedded: number;
  folded: number;
  groups: number;
  /** How many merges the current vocabulary would propose. Computed, not counted. */
  suggestions?: number;
}

export function fetchTagCounts(signal?: AbortSignal): Promise<TagCounts> {
  return get<TagCounts>("/v1/tags", signal);
}

export interface TagWordPage {
  words: TagWord[];
  /** How many words match, of which this is one page. */
  total: number;
}

export function fetchTagWords(
  options: { junk?: boolean; q?: string; limit?: number; offset?: number } = {},
  signal?: AbortSignal,
): Promise<TagWordPage> {
  const params = new URLSearchParams();
  if (options.junk) params.set("junk", "1");
  if (options.q) params.set("q", options.q);
  if (options.limit) params.set("limit", String(options.limit));
  if (options.offset) params.set("offset", String(options.offset));
  return get<TagWordPage>(`/v1/tags/words?${params}`, signal);
}

export interface TagProposals {
  groups: TagProposal[];
  similarity: number;
  /**
   * Kept words with no vector. It travels with the groups because an empty list
   * means two different things — everything is merged, or nothing is embedded —
   * and the page has a different button for each.
   */
  unembedded: number;
}

export function fetchTagProposals(
  similarity?: number,
  signal?: AbortSignal,
): Promise<TagProposals> {
  const params = new URLSearchParams();
  if (similarity) params.set("similarity", similarity.toFixed(2));
  return get<TagProposals>(`/v1/tags/proposals?${params}`, signal);
}

export function fetchMergedTags(signal?: AbortSignal): Promise<TagProposal[]> {
  return get<{ groups: TagProposal[] }>("/v1/tags/merged", signal).then((r) => r.groups ?? []);
}

/**
 * What a pass did, and where it got to.
 *
 * `counts.untriaged` and `counts.unembedded` are the loop conditions: each call
 * judges or embeds a bounded slice and the page calls again while there is
 * anything left. A couple of minutes of GPU is too long for one request and far
 * too short to be worth a background job, so the loop lives in the browser and
 * every call is resumable.
 */
export interface TagPass {
  triaged?: number;
  embedded?: number;
  judged?: number;
  approved?: number;
  reindexed?: number;
  counts: TagCounts;
}

export function triageTags(): Promise<TagPass> {
  return send<TagPass>("/v1/tags/triage", {});
}

export function embedTags(): Promise<TagPass> {
  return send<TagPass>("/v1/tags/embed", {});
}

/** Moves words between the two lists, and stamps them as a person's answer. */
export function judgeTags(ids: number[], junk: boolean): Promise<TagPass> {
  return send<TagPass>("/v1/tags/judge", { ids, junk });
}

/** Signs the whole triage off, and rebuilds the search index in the same breath. */
export function approveTriage(): Promise<TagPass> {
  return send<TagPass>("/v1/tags/approve", {});
}

export interface TagMerged {
  canonical: string;
  merged: number;
  rejected?: number;
  /** How many photographs had their search row rewritten. */
  reindexed: number;
}

/**
 * Folds words into one.
 *
 * `rejected` travels with the merge rather than as a second call because it is
 * one decision: without it the next clustering run proposes exactly the group
 * that was just corrected.
 */
export function mergeTags(
  canonical: number,
  members: number[],
  rejected: number[] = [],
): Promise<TagMerged> {
  return send<TagMerged>("/v1/tags/merge", { canonical, members, rejected });
}

/** Records that a group of words are not one word, so the clustering stops asking. */
export function dismissTagProposal(ids: number[]): Promise<{ blocked: number }> {
  return send<{ blocked: number }>("/v1/tags/dismiss", { ids });
}

export function unmergeTags(ids: number[]): Promise<{ restored: number }> {
  return send<{ restored: number }>("/v1/tags/unmerge", { ids });
}

// Uploading from the browser. The one write in this file that creates a
// photograph rather than moving one, and the only one that does not go through
// `send` — see uploadAsset for why.

/**
 * Where the archive already holds some content. The same words photod answers
 * with, and `where` is never a vault *bucket*: see db.LookupContent.
 */
export type ContentWhere = "library" | "trash" | "vault" | "purged";

export interface KnownContent {
  sha256: string;
  where: ContentWhere;
  /** Absent for a vault or purged match, which name no row anybody may read. */
  id?: string;
  filename?: string;
}

/**
 * Asks which of these digests the archive already has, in the order asked.
 *
 * This is what makes a duplicate a row on the upload page rather than the
 * result of sending one. The browser has read every byte to compute these
 * anyway — it had to, to be able to declare one — so the check is free where
 * the transfer it saves is not.
 */
export async function checkContent(
  sha256: string[],
  signal?: AbortSignal,
): Promise<KnownContent[]> {
  const res = await fetch(`${baseUrl()}/v1/gallery/uploads/check`, {
    method: "POST",
    headers: { ...headers(), "Content-Type": "application/json" },
    body: JSON.stringify({ sha256 }),
    signal,
  });
  if (!res.ok) throw apiError(res.status, await errorText(res));
  const body = (await res.json()) as { known: KnownContent[] };
  return body.known ?? [];
}


// ---------------------------------------------------------------------------
// Signing in, and the credentials behind it.
//
// Signing *in* is not here: photod serves that page itself, so that an
// unauthenticated visitor gets a sign-in form rather than this bundle. See
// server/internal/api/frontdoor.go. What is here is everything the gallery does
// once it is already signed in — look at its own session, add an authenticator,
// withdraw one, and leave.
// ---------------------------------------------------------------------------

export interface AuthStatus {
  /** False only on an archive nobody has claimed yet. */
  enrolled: boolean;
  signedIn: boolean;
  /** "passkey" or "recovery". Absent without a session. */
  method?: string;
  /** When this session dies if nothing touches it — idle window or cap. */
  expires?: string;
  /**
   * How many recovery codes remain. Withheld from an unauthenticated caller,
   * so it is absent rather than zero when nobody is signed in.
   */
  recoveryRemaining?: number;
}

export interface Passkey {
  id: string;
  label: string;
  /** "internal" for Touch ID and the like, "usb"/"nfc" for a security key. */
  transports?: string;
  createdAt: string;
  lastUsedAt?: string;
  revokedAt?: string;
}

export function fetchAuthStatus(signal?: AbortSignal): Promise<AuthStatus> {
  return get<AuthStatus>("/v1/auth/status", signal);
}

export function fetchPasskeys(signal?: AbortSignal): Promise<{ passkeys: Passkey[] }> {
  return get<{ passkeys: Passkey[] }>("/v1/auth/passkeys", signal);
}

/**
 * Withdraws an authenticator, and every session it opened.
 *
 * photod refuses to withdraw the last one: an archive reachable only by
 * recovery code is a state worth being able to get into deliberately from the
 * command line and not by accident from a menu.
 */
export function revokePasskey(id: string): Promise<{ passkeyId: string; sessionsEnded: number }> {
  return send<{ passkeyId: string; sessionsEnded: number }>(
    `/v1/auth/passkeys/${encodeURIComponent(id)}`,
    undefined,
    "DELETE",
  );
}

/**
 * Replaces the recovery codes and returns the new set. This is the only time
 * they exist anywhere but on whatever they get written down on.
 */
export function mintRecoveryCodes(): Promise<{ codes: string[] }> {
  return send<{ codes: string[] }>("/v1/auth/recovery-codes", undefined);
}

/** Ends this session on the server. Where the caller goes next is its own. */
export function logout(): Promise<unknown> {
  return send<unknown>("/v1/auth/logout", undefined);
}

/**
 * The two halves of registering an additional passkey from a client that is
 * already signed in.
 *
 * No enrollment code: whoever holds a live session can already read and delete
 * the entire archive, so requiring a trip to the terminal to add a second
 * authenticator would be ceremony rather than security. The codeless path is
 * photod's AddPasskeyAuthorized.
 *
 * The ceremony between them is `navigator.credentials.create`, which exists in
 * exactly one of the two clients — so the options come back as whatever the
 * caller says they are. The shape is `WireCreationOptions` in web/src/lib, and
 * it is typed there because its every field is a DOM type; naming it here would
 * put `PublicKeyCredentialCreationOptions` in a package a phone imports.
 */
export function startPasskeyRegistration<T>(): Promise<T> {
  return send<T>("/v1/auth/register/start", {});
}

export function finishPasskeyRegistration(attestation: unknown): Promise<Passkey> {
  return send<Passkey>("/v1/auth/register/finish", attestation);
}
