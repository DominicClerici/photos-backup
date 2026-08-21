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

/**
 * Who put a photograph into a shared album, as far as the phone will say.
 *
 * Every field is optional and the whole thing is null far more often than not:
 * see cloudOwner() in the Swift for why this is read through an undocumented
 * door and what happens when that door closes. `displayName` is the one to show
 * — a name where there is one, an address where there is not, and never the
 * hashed identifier, which is for telling two contributors apart rather than for
 * telling anybody who they are.
 */
export type SharedContributor = {
  firstName: string | null;
  lastName: string | null;
  email: string | null;
  /** Stable per person across assets, and not a thing to put on screen. */
  personId: string | null;
  displayName: string | null;
};

/**
 * A dump of one asset's provenance, for reading rather than for using. Every
 * `properties` entry is a "name = value" line straight off the object.
 */
export type SharedProvenance = {
  localId: string;
  /** The Objective-C class the value was read from. */
  class: string;
  sourceTypes: { value: number; names: string[] };
  contributor: SharedContributor | null;
  /** The keys the contributor was looked for under, found or not. */
  contributorKeys: string[];
  properties: string[];
  albums: {
    title: string | null;
    class: string;
    contributor: SharedContributor | null;
    properties: string[];
  }[];
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
  /**
   * The shared albums this asset is in, by title. Empty for everything in the
   * camera roll, which is nearly everything.
   *
   * Read here, at describe time, rather than carried down from the enumeration
   * that queued the asset: the album is what the archive files the photograph
   * under, and a title read weeks ago is a title that may since have been
   * renamed.
   */
  sharedAlbums: string[];
  /** Who added it, for an asset in somebody else's album. Usually null. */
  contributor: SharedContributor | null;
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
  /** Who put it in the album, where the phone would say. See SharedContributor. */
  contributor: SharedContributor | null;
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
  /** Whose album it is, as opposed to who added any one photograph to it. */
  owner: SharedContributor | null;
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

/**
 * A fetch that did not work, described in the terms the caller has to decide on.
 *
 * The domain and the code are Apple's, untranslated, for the same reason every
 * other enumeration in this module is carried raw: which of them iCloud uses to
 * say "you are asking too often" is not known here, and a guess dressed up as a
 * category would be worse than the pair itself. The partial byte count is the
 * other half of the picture — a read that died after thirty seconds and twelve
 * megabytes is a different failure from one that died instantly.
 */
export type SharedFetchFailure = {
  /** NSError's domain, or `PhotoFacts` for something the module found itself. */
  domain: string;
  code: number;
  message: string;
  /** What had arrived before it gave up, and how long it took to get there. */
  bytes: number;
  elapsedMs: number;
};

/** One fetch, either way round. */
export type SharedFetchResult =
  | { ok: true; read: SharedResourceRead }
  | { ok: false; failure: SharedFetchFailure };

/** How far the fetch in flight has got. See onSharedFetchProgress in the Swift. */
export type SharedFetchProgress = {
  localId: string;
  /** Bytes handed over so far. Moves even for an asset already on the phone. */
  bytes: number;
  /** iCloud's own 0..1 for the download, and 0 when there is nothing to download. */
  fraction: number;
};

/**
 * One shared asset, downloaded and kept.
 *
 * The digest is computed while the bytes go past rather than by reading the
 * finished file, which is not a micro-optimization: it is what makes one trip to
 * iCloud enough. The queue has to declare an MD5 the server will verify, and the
 * only other ways to get one are to fetch the asset twice or to read every byte
 * back off flash.
 */
export type SharedDownload = {
  /** Where the bytes are. The caller's file now, including to delete. */
  path: string;
  bytes: number;
  md5: string;
  elapsedMs: number;
  uniformTypeIdentifier: string | null;
  /**
   * What Apple calls the file, which is not always what PhotoKit calls the
   * asset — a shared HEIC comes back re-encoded, and the name is the better
   * evidence of what the bytes actually are.
   */
  originalFilename: string | null;
  resourceType: string;
};

export type SharedDownloadResult =
  | { ok: true; download: SharedDownload }
  | { ok: false; failure: SharedFetchFailure };

/**
 * Which half of a shared Live Photo to read. `primary` is the asset itself and
 * is what everything else asks for.
 */
export type SharedResourceWanted = 'primary' | 'pairedVideo';

/** The flat dictionary the native side resolves, before it is given its shape. */
type NativeFetchResult =
  | ({ ok: true } & SharedResourceRead)
  | ({ ok: false } & SharedFetchFailure);

type NativeDownloadResult =
  | ({ ok: true } & SharedDownload)
  | ({ ok: false } & SharedFetchFailure);

type PhotoFactsNativeModule = {
  factsForAssetAsync(localId: string): Promise<PhotoKitFacts | null>;
  enumerateAsync(limit: number): Promise<PhotoKitAsset[]>;
  sharedAlbumsAsync(): Promise<SharedAlbum[]>;
  fetchSharedResourceAsync(localId: string): Promise<NativeFetchResult | null>;
  downloadSharedResourceAsync(
    localId: string,
    destination: string,
    want: SharedResourceWanted
  ): Promise<NativeDownloadResult | null>;
  sharedProvenanceAsync(localId: string): Promise<SharedProvenance | null>;
  md5ForFileAsync(uri: string): Promise<string>;
  addListener(
    event: 'onSharedFetchProgress',
    listener: (progress: SharedFetchProgress) => void
  ): { remove(): void };
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
 * Whether this build can read a shared asset's bytes at all.
 *
 * Asked because the two halves of the shared-album feature can be in a build
 * separately, and the in-between state is quiet in the worst way: a dev client
 * that carries the enumerator but not the downloader lists every album, lets
 * them be ticked, and then fails every asset in them one at a time. This is what
 * lets the screen say so first.
 */
export const canDownloadShared = carries('downloadSharedResourceAsync');

export const canReadSharedProvenance = carries('sharedProvenanceAsync');

/**
 * What this iOS will say about who shared one photograph, as text to read.
 *
 * A diagnostic with no part in a backup. The contributor comes off properties
 * Apple does not document, so a photograph that reports none is ambiguous
 * between having none and this build not recognising the name of the field that
 * holds it. This lists what the class actually declares, which settles it.
 */
export async function photoKitSharedProvenance(
  localId: string
): Promise<SharedProvenance | null> {
  if (!carries('sharedProvenanceAsync')) return null;
  return (await native!.sharedProvenanceAsync(localId)) ?? null;
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
 * A failed read resolves as `{ ok: false }` rather than rejecting, which is the
 * opposite of md5ForFileAsync's rule and is not an inconsistency: a digest is a
 * claim about bytes and has no failed form, while a fetch is driven by a loop
 * that has to tell a timeout from a withdrawn album in order to decide whether
 * to try again. See SharedFetchFailure.
 *
 * Null still means the one thing it means everywhere else in this file: the
 * build cannot. It also resolves null for an identifier that no longer names
 * anything, which is the same answer photoKitFacts gives and for the same
 * reason — a photo the owner has since deleted is not an error worth acting on.
 *
 * The fields are copied across one at a time rather than spread, because the
 * native dictionary is flat and the two halves of the union share `bytes` and
 * `elapsedMs`. A spread would carry a failure's partial byte count into a
 * successful read's shape and typecheck perfectly while doing it.
 */
export async function photoKitReadSharedResource(
  localId: string
): Promise<SharedFetchResult | null> {
  if (!carries('fetchSharedResourceAsync')) return null;

  const result = await native!.fetchSharedResourceAsync(localId);
  if (!result) return null;

  if (result.ok) {
    return {
      ok: true,
      read: {
        bytes: result.bytes,
        elapsedMs: result.elapsedMs,
        uniformTypeIdentifier: result.uniformTypeIdentifier,
        originalFilename: result.originalFilename,
        resourceType: result.resourceType,
      },
    };
  }

  return {
    ok: false,
    failure: {
      domain: result.domain,
      code: result.code,
      message: result.message,
      bytes: result.bytes,
      elapsedMs: result.elapsedMs,
    },
  };
}

/**
 * Reads one shared asset out of iCloud and keeps it at `destination`.
 *
 * The backup's version of photoKitReadSharedResource, and it answers in the same
 * three ways for the same three reasons: a resolved failure for a fetch that did
 * not work, null for a build that cannot do this or an identifier that no longer
 * names anything, and a success carrying what came back.
 *
 * The file belongs to the caller from the moment this resolves, and so does
 * deleting it. Nothing is left behind by a failure — see downloadResource() in
 * the Swift for why a half-written original is worse than none.
 */
export async function photoKitDownloadSharedResource(
  localId: string,
  destination: string,
  want: SharedResourceWanted = 'primary'
): Promise<SharedDownloadResult | null> {
  if (!carries('downloadSharedResourceAsync')) return null;

  const result = await native!.downloadSharedResourceAsync(localId, destination, want);
  if (!result) return null;

  // Copied field by field rather than spread, for the reason
  // photoKitReadSharedResource is: the two halves of the union share `bytes`,
  // and a spread would carry a failure's partial count into a success and
  // typecheck while doing it.
  if (result.ok) {
    return {
      ok: true,
      download: {
        path: result.path,
        bytes: result.bytes,
        md5: result.md5,
        elapsedMs: result.elapsedMs,
        uniformTypeIdentifier: result.uniformTypeIdentifier,
        originalFilename: result.originalFilename,
        resourceType: result.resourceType,
      },
    };
  }

  return {
    ok: false,
    failure: {
      domain: result.domain,
      code: result.code,
      message: result.message,
      bytes: result.bytes,
      elapsedMs: result.elapsedMs,
    },
  };
}

/**
 * Watches the fetch in flight, and hands back the way to stop watching.
 *
 * A build with no event in it returns a no-op unsubscribe rather than throwing,
 * which is the same bargain the rest of this file strikes: the fetch still runs
 * on an older dev client, the bar on screen simply does not move until each
 * asset finishes. Progress is the first thing that should degrade and the last
 * thing worth failing a run over.
 *
 * The try/catch is for the same reason `carries` exists one layer down. A dev
 * client from before this event was declared has an `addListener` — every Expo
 * module does — and it is the event name it has never heard of.
 */
export function photoKitOnSharedFetchProgress(
  listener: (progress: SharedFetchProgress) => void
): () => void {
  if (!carries('addListener')) return () => {};

  try {
    const subscription = native!.addListener('onSharedFetchProgress', listener);
    return () => subscription.remove();
  } catch {
    return () => {};
  }
}
