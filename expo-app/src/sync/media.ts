import { Directory, File, Paths } from 'expo-file-system';
import {
  Asset,
  AssetField,
  MediaSubtype,
  MediaType,
  Query,
  type AssetMetadata,
} from 'expo-media-library';

import {
  photoKitEnumerate,
  photoKitFacts,
  photoKitMd5,
  type PhotoKitAsset,
  type PhotoKitFacts,
  type PhotoKitLocation,
} from '../../modules/photo-facts';
import { sweepChunks } from './chunked';
import {
  errorText,
  SyncError,
  type AssetFacts,
  type AssetLocation,
  type EnumeratedAsset,
  type MediaSource,
  type OpenedAsset,
  type QueueItem,
} from './types';

/**
 * Suffix that turns a Live Photo's single PhotoKit identifier into a second
 * queue key for its paired video. The server treats local ids as opaque, so this
 * only has to be stable, which it is.
 */
export const LIVE_SUFFIX = '#live';

/**
 * Where extracted Live Photo videos are kept.
 *
 * getLivePhotoVideoUri() writes into NSTemporaryDirectory() under a random UUID
 * and never cleans up, and nothing can enumerate that directory afterwards. So
 * every extraction is moved here immediately: a directory we own, can sweep on
 * startup, and can reason about.
 */
const LIVE_DIRECTORY = 'livephotos';

export class PhotoKitMediaSource implements MediaSource {
  /**
   * Lists the newest assets, adding a second queue entry for each Live Photo's
   * paired video.
   *
   * maxItems of 0 means the whole library. This re-runs on every start and
   * relies on the store to ignore what it already has, which is only a
   * reasonable design if listing the library is cheap — and Phase 0's "3,573
   * assets without a noticeable pause" was measured on the listing alone. The
   * Live Photo question was not free: expo-media-library's metadata carries no
   * subtypes, so answering it meant a native round-trip per image, awaited in
   * turn, tens of thousands of times, every run. The native enumerator answers it
   * in the same pass that produces the list.
   */
  async enumerate(maxItems: number): Promise<EnumeratedAsset[]> {
    const listed = (await photoKitEnumerate(maxItems)) ?? (await listViaMediaLibrary(maxItems));

    const assets: EnumeratedAsset[] = [];
    for (const entry of listed) {
      const filename = entry.filename ?? entry.localId;
      assets.push({
        localId: entry.localId,
        kind: entry.kind,
        parentLocalId: null,
        filename,
        createdAt: entry.createdAt,
        modifiedAt: entry.modifiedAt,
      });

      if (!entry.isLive) continue;

      assets.push({
        localId: entry.localId + LIVE_SUFFIX,
        kind: 'live_video',
        parentLocalId: entry.localId,
        filename: pairedVideoName(filename),
        createdAt: entry.createdAt,
        modifiedAt: entry.modifiedAt,
      });
    }
    return assets;
  }

  async open(item: QueueItem, opts: { hash: boolean }): Promise<OpenedAsset> {
    return item.kind === 'live_video' ? this.openPairedVideo(item, opts) : this.openOriginal(item, opts);
  }

  /**
   * Reads what PhotoKit knows and the file does not.
   *
   * Read here, at upload time, rather than during enumeration: a backfill
   * enumerates the whole library on every run and uploads a shrinking slice of
   * it, so asking per asset here is a handful of native calls per photo archived
   * rather than per photo owned.
   *
   * Four of the five lookups go through expo-media-library, which is why the
   * fifth exists at all: its Asset exposes ten getters out of PHAsset's forty,
   * and the thirty it leaves behind — the Hidden album, the burst, whether the
   * shot has ever been edited — are unrecoverable rather than merely absent.
   *
   * Nothing here is allowed to fail the item. A photo whose albums could not be
   * listed is still a photo that belongs in the archive, so a failed lookup
   * degrades to the part that did answer, and a build without the native module
   * degrades to exactly what this returned before there was one.
   */
  async facts(item: QueueItem): Promise<AssetFacts | null> {
    // The paired video is not an asset to PhotoKit — it is a property of the
    // still, and the still is the one carrying the heart and the album.
    if (item.kind === 'live_video') return null;

    const asset = new Asset(item.localId);
    const [favorite, subtypes, albums, location, photoKit] = await Promise.all([
      quietly(() => asset.getFavorite(), false),
      quietly(() => asset.getMediaSubtypes(), [] as MediaSubtype[]),
      quietly(() => asset.getAlbums(), []),
      quietly(() => asset.getLocation(), null),
      quietly<PhotoKitFacts | null>(() => photoKitFacts(item.localId), null),
    ]);

    const titles = await Promise.all(albums.map((album) => quietly(() => album.getTitle(), '')));

    return {
      favorite,
      subtypes: subtypes.map((subtype) => String(subtype)),
      albums: titles.filter((title) => title.trim() !== ''),
      location: assetLocation(location, photoKit?.location ?? null),
      photoKit,
    };
  }

  private async openOriginal(item: QueueItem, opts: { hash: boolean }): Promise<OpenedAsset> {
    let uri: string;
    try {
      uri = await new Asset(item.localId).getUri();
    } catch (e) {
      throw new SyncError(`could not resolve the original: ${errorText(e)}`, 'item');
    }

    // PhotoKit appends a "#<base64 plist>" fragment. Phase 0 confirmed the
    // stripped path still reads, and a bare path is far less surprising anywhere
    // it gets used as a string.
    const file = new File(stripFragment(uri));
    if (!file.exists) {
      throw new SyncError(`the original is not readable at ${file.uri}`, 'item');
    }

    return {
      uri: file.uri,
      size: file.size ?? 0,
      md5: opts.hash ? await digest(file) : null,
      // Nothing to release: this points straight at the library.
      release: async () => {},
    };
  }

  /**
   * Extracts a Live Photo's paired video, moves it somewhere we control, and
   * hands back a release() that deletes it.
   */
  private async openPairedVideo(item: QueueItem, opts: { hash: boolean }): Promise<OpenedAsset> {
    const parentId = item.parentLocalId;
    if (!parentId) {
      throw new SyncError('a paired video has no parent asset recorded', 'item');
    }

    let extractedUri: string | null;
    try {
      extractedUri = await new Asset(parentId).getLivePhotoVideoUri();
    } catch (e) {
      throw new SyncError(`could not extract the paired video: ${errorText(e)}`, 'item');
    }
    if (!extractedUri) {
      throw new SyncError('the parent asset is no longer a Live Photo', 'item');
    }

    const extracted = new File(stripFragment(extractedUri));
    if (!extracted.exists) {
      throw new SyncError(`the extracted video is missing at ${extracted.uri}`, 'item');
    }

    const directory = liveDirectory();
    directory.create({ intermediates: true, idempotent: true });
    const destination = new File(directory, cacheName(item.localId));

    try {
      // Moving into a directory we own is what makes cleanup possible at all;
      // the extraction path cannot be enumerated later.
      await extracted.move(destination, { overwrite: true });
    } catch (e) {
      // Fall back to reading it where it landed. One un-sweepable file is better
      // than a Live Photo that never backs up; iOS purges its own temp directory.
      return {
        uri: extracted.uri,
        size: extracted.size ?? 0,
        md5: opts.hash ? await digest(extracted) : null,
        release: async () => {
          deleteQuietly(extracted);
        },
      };
    }

    const moved = new File(destination.uri);
    return {
      uri: moved.uri,
      size: moved.size ?? 0,
      md5: opts.hash ? await digest(moved) : null,
      release: async () => {
        deleteQuietly(moved);
      },
    };
  }

  /**
   * Deletes temporary files a previous run left behind: extracted Live Photo
   * videos, and any chunk the resumable uploader staged before it was killed.
   *
   * Both leak 8MB-to-100MB at a time on a device with finite space, and neither
   * is enumerable from anywhere else once the run that created it is gone.
   */
  async sweep(): Promise<number> {
    let removed = sweepChunks();

    const directory = liveDirectory();
    if (!directory.exists) return removed;

    for (const entry of directory.list()) {
      if (entry instanceof File && deleteQuietly(entry)) removed += 1;
    }
    return removed;
  }
}

function liveDirectory(): Directory {
  return new Directory(Paths.cache, LIVE_DIRECTORY);
}

/**
 * One location out of the two readings of the same CLLocation.
 *
 * The native module's is preferred where there is one, because it is that
 * CLLocation with the rest of itself still attached — altitude, accuracies, the
 * time the fix was taken — where expo-media-library reduces it to a pair of
 * numbers on the way through. They never disagree about the coordinates.
 */
function assetLocation(
  reported: { latitude: number; longitude: number } | null,
  full: PhotoKitLocation | null
): AssetLocation | null {
  if (full) return full;
  return reported ? { latitude: reported.latitude, longitude: reported.longitude } : null;
}

/** Runs a library lookup, falling back rather than throwing. */
async function quietly<T>(read: () => Promise<T>, fallback: T): Promise<T> {
  try {
    return (await read()) ?? fallback;
  } catch {
    return fallback;
  }
}

/**
 * The listing as it was before the native enumerator: expo-media-library for the
 * metadata, then one round-trip per image to ask about Live Photos.
 *
 * Kept because enumeration is the one step of a run that cannot degrade — an
 * empty list is a backup that does nothing — and a dev client older than the
 * native enumerator is an ordinary thing to be running. It is slow in exactly
 * the way described above, and only ever reached on such a build.
 */
async function listViaMediaLibrary(maxItems: number): Promise<PhotoKitAsset[]> {
  let query = new Query()
    .within(AssetField.MEDIA_TYPE, [MediaType.IMAGE, MediaType.VIDEO])
    .orderBy({ key: AssetField.CREATION_TIME, ascending: false });
  if (maxItems > 0) query = query.limit(maxItems);

  const metadata: AssetMetadata[] = await query.exeForMetadata();

  const listed: PhotoKitAsset[] = [];
  for (const entry of metadata) {
    listed.push({
      localId: entry.id,
      kind: entry.mediaType === MediaType.VIDEO ? 'video' : 'still',
      filename: entry.filename,
      createdAt: entry.creationTime,
      modifiedAt: entry.modificationTime,
      isLive: entry.mediaType === MediaType.IMAGE ? await isLivePhoto(entry.id) : false,
    });
  }
  return listed;
}

async function isLivePhoto(localId: string): Promise<boolean> {
  try {
    const subtypes = await new Asset(localId).getMediaSubtypes();
    return subtypes.includes(MediaSubtype.LIVE_PHOTO);
  } catch {
    // A subtype lookup that fails should not drop the still from the queue.
    return false;
  }
}

/**
 * Reads the digest, off the JS thread where this build can.
 *
 * The fallback is expo-file-system's `File.md5`, a synchronous JSI property that
 * blocks the JS thread for the length of the hash — around 2.4s for a 1GB file
 * per Phase 0. Both produce the same digest over the same bytes; the difference
 * is only whether the rest of the app, the upload in flight included, keeps
 * running while it happens. A build old enough to take the fallback still backs
 * up correctly, in the stop-start way it always did.
 */
async function digest(file: File): Promise<string> {
  const native = await photoKitMd5(file.uri);
  if (native) return native;

  const md5 = file.md5;
  if (!md5) throw new SyncError(`could not hash ${file.uri}`, 'item');
  return md5;
}

function deleteQuietly(file: File): boolean {
  try {
    if (!file.exists) return false;
    file.delete();
    return true;
  } catch {
    return false;
  }
}

function stripFragment(uri: string): string {
  const hash = uri.indexOf('#');
  return hash === -1 ? uri : uri.slice(0, hash);
}

function pairedVideoName(stillFilename: string): string {
  const dot = stillFilename.lastIndexOf('.');
  const stem = dot === -1 ? stillFilename : stillFilename.slice(0, dot);
  return `${stem}.MOV`;
}

/** A filesystem-safe name; PhotoKit identifiers contain slashes. */
function cacheName(localId: string): string {
  return `${localId.replace(/[^A-Za-z0-9._-]/g, '_')}.mov`;
}
