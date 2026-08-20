/**
 * The JavaScript side of the PhotoFacts native module — see
 * ios/PhotoFactsModule.swift for what it reads and why any of it matters.
 *
 * The module is loaded optionally, and that is not defensive habit: this app is
 * developed against a dev client that is rebuilt far less often than the JS
 * bundle is reloaded, so the very normal case is a bundle that knows about
 * PhotoFacts running on a binary that does not carry it. When that happens the
 * archive loses the extra facts for those uploads and nothing else — never an
 * upload, never a crash.
 */
import { requireOptionalNativeModule } from 'expo';

/** One of Apple's enumerations: the raw value, and Apple's own constant name. */
export type EnumFact = { value: number; name: string };

/**
 * One of Apple's option sets: the raw bitmask, and a name per bit that was set.
 * A bit the native module has no name for is still in `value`, which is the
 * point of carrying both.
 */
export type OptionSetFact = { value: number; names: string[] };

/**
 * One entry of PHAssetResource.assetResources(for:) — what PhotoKit holds for
 * this asset. An asset that has been edited carries three of these: the
 * original, the rendered version, and the adjustment data itself.
 */
export type ResourceFact = {
  type: EnumFact;
  originalFilename: string | null;
  uniformTypeIdentifier: string | null;
};

/**
 * The whole CLLocation. Course, speed and the accuracies are CoreLocation's
 * values verbatim, and it writes a negative in them for "not measured".
 */
export type PhotoKitLocation = {
  latitude: number;
  longitude: number;
  altitude: number;
  horizontalAccuracy: number;
  verticalAccuracy: number;
  course: number;
  speed: number;
  timestamp: string | null;
};

export type PhotoKitFacts = {
  localId: string;
  /** The Hidden album: a decision a person made, and invisible in the file. */
  hidden: boolean;
  favorite: boolean;
  mediaType: EnumFact;
  /** Apple's constant names, e.g. 'photoScreenshot'. */
  mediaSubtypes: OptionSetFact;
  /** Camera roll, iTunes sync or somebody else's shared album. */
  sourceType: OptionSetFact;
  playbackStyle: EnumFact;
  /** Shared by every frame of one burst; null for anything that is not one. */
  burstIdentifier: string | null;
  /** Which frame the phone picked, and which one the person picked. */
  burstSelectionTypes: OptionSetFact;
  representsBurst: boolean;
  pixelWidth: number;
  pixelHeight: number;
  durationSeconds: number;
  /** RFC3339. */
  createdAt: string | null;
  modifiedAt: string | null;
  /** True when the asset carries adjustment data, i.e. it has been edited. */
  hasAdjustments: boolean;
  /** The name the camera gave the file. */
  originalFilename: string | null;
  resources: ResourceFact[];
  location: PhotoKitLocation | null;
};

/**
 * One row of the library listing — see listAssets() in the Swift module.
 *
 * `localId` carries the same `ph://` prefix expo-media-library puts on its own
 * identifiers, because it is the queue's primary key and rows already exist
 * under it.
 */
export type PhotoKitAsset = {
  localId: string;
  kind: 'still' | 'video';
  filename: string | null;
  createdAt: number | null;
  modifiedAt: number | null;
  /** True for a Live Photo's still, whose paired video is a second queue entry. */
  isLive: boolean;
};

/**
 * One asset in an iCloud Shared Album: everything the library enumerator
 * reports, plus what the survey needs in order to say what sharing did to it.
 *
 * The dimensions and the duration are the answer to "is Apple's copy worth
 * archiving", and PhotoKit knows them without a byte being fetched. The resource
 * inventory is the follow-up question: a lone `photo` means the downscaled
 * render is all there is, and a `fullSizePhoto` beside it would mean it is not.
 */
export type SharedAsset = PhotoKitAsset & {
  pixelWidth: number;
  pixelHeight: number;
  /** Zero for a still. */
  durationSeconds: number;
  /** Expected to be exactly ['typeCloudShared']. Reported, not assumed. */
  sourceTypes: OptionSetFact;
  /** Apple's constant names, one per PHAssetResource the asset carries. */
  resourceTypes: string[];
};

/**
 * One shared album and its contents.
 *
 * The assets are nested rather than flattened because the album title is the
 * only provenance a shared asset has, and one asset can belong to several
 * albums. Deduplication is the caller's: whether an asset in three albums counts
 * once or three times depends on what is being counted.
 */
export type SharedAlbum = {
  localId: string;
  title: string | null;
  /** The album's own span, as PhotoKit reports it. Milliseconds. */
  startDate: number | null;
  endDate: number | null;
  assets: SharedAsset[];
};

/** What one shared asset's primary resource turned out to be, once read. */
export type SharedResourceRead = {
  bytes: number;
  /** Wall clock for the whole fetch, including whatever iCloud took. */
  elapsedMs: number;
  uniformTypeIdentifier: string | null;
  originalFilename: string | null;
  /** Which PHAssetResource was read — `photo`, `fullSizePhoto`, `video`, … */
  resourceType: string;
};

type PhotoFactsNativeModule = {
  factsForAssetAsync(localId: string): Promise<PhotoKitFacts | null>;
  enumerateAsync(limit: number): Promise<PhotoKitAsset[]>;
  sharedAlbumsAsync(): Promise<SharedAlbum[]>;
  fetchSharedResourceAsync(localId: string): Promise<SharedResourceRead | null>;
  md5ForFileAsync(uri: string): Promise<string>;
};

const native = requireOptionalNativeModule<PhotoFactsNativeModule>('PhotoFacts');

/** Whether this build carries the native module at all. */
export const hasPhotoFacts = native != null;

/**
 * Whether a function is in this build, asked one function at a time.
 *
 * A missing module is not the only way to be out of date, and after the first
 * time this module grew it stopped being the likely one: the ordinary case is a
 * dev client that carries PhotoFacts as it was a fortnight ago, running a JS
 * bundle that knows about something added since. That build answers
 * `requireOptionalNativeModule` perfectly well and then throws on the call.
 */
function carries(name: keyof PhotoFactsNativeModule): boolean {
  return typeof native?.[name] === 'function';
}

/**
 * Everything PhotoKit knows about one asset, or null when this build has no
 * native module, when the identifier no longer names anything, or when the photo
 * library is not readable.
 */
export async function photoKitFacts(localId: string): Promise<PhotoKitFacts | null> {
  if (!carries('factsForAssetAsync')) return null;
  return (await native!.factsForAssetAsync(localId)) ?? null;
}

/**
 * The whole library in one call, or null when this build cannot do it and the
 * caller should fall back to listing it the slow way.
 *
 * Null means "this build cannot", never "the library is empty" — an empty
 * library is an empty array. The caller's fallback is a great deal slower, so
 * the two must not be confusable.
 *
 * `limit` of 0 means the whole library.
 */
export async function photoKitEnumerate(limit: number): Promise<PhotoKitAsset[] | null> {
  if (!carries('enumerateAsync')) return null;
  return native!.enumerateAsync(limit);
}

/**
 * The MD5 of a local file, computed off the JavaScript thread, or null when this
 * build has no native hash and the caller should fall back to the synchronous
 * one.
 *
 * Again null is only ever "this build cannot". A file that could not be read
 * rejects, because a digest is a claim about bytes and the queue has to be able
 * to tell a failed read from a hash.
 */
export async function photoKitMd5(uri: string): Promise<string | null> {
  if (!carries('md5ForFileAsync')) return null;
  return native!.md5ForFileAsync(uri);
}

/**
 * Every iCloud Shared Album on the phone, or null when this build cannot ask.
 *
 * Null is "this build has no shared-album enumerator", never "there are none" —
 * a phone with Shared Albums switched off in Settings answers with an empty
 * array, and the two have to read differently or the survey reports a missing
 * feature as an empty result.
 *
 * There is no `limit`. The library enumerator takes one because a camera roll is
 * tens of thousands of assets and the caller may want the newest slice; shared
 * albums are a handful of collections and the whole point is to see all of them.
 */
export async function photoKitSharedAlbums(): Promise<SharedAlbum[] | null> {
  if (!carries('sharedAlbumsAsync')) return null;
  return native!.sharedAlbumsAsync();
}

/**
 * Reads one shared asset's bytes from iCloud and reports what came back.
 *
 * Rejects rather than resolving null when the read fails, for the same reason
 * md5ForFileAsync does: the survey exists to find out how this fails, so a
 * timeout, a withdrawn album or a resource that is not there has to arrive as an
 * error and never as a successful read of zero bytes.
 *
 * Null still means the one thing it means everywhere else in this file: the
 * build cannot. It also resolves null for an identifier that no longer names
 * anything, which is the same answer photoKitFacts gives and for the same
 * reason — a photo the owner has since deleted is not an error worth acting on.
 */
export async function photoKitReadSharedResource(
  localId: string
): Promise<SharedResourceRead | null> {
  if (!carries('fetchSharedResourceAsync')) return null;
  return (await native!.fetchSharedResourceAsync(localId)) ?? null;
}
