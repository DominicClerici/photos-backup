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

type PhotoFactsNativeModule = {
  factsForAssetAsync(localId: string): Promise<PhotoKitFacts | null>;
};

const native = requireOptionalNativeModule<PhotoFactsNativeModule>('PhotoFacts');

/** Whether this build carries the native module at all. */
export const hasPhotoFacts = native != null;

/**
 * Everything PhotoKit knows about one asset, or null when this build has no
 * native module, when the identifier no longer names anything, or when the photo
 * library is not readable.
 */
export async function photoKitFacts(localId: string): Promise<PhotoKitFacts | null> {
  if (!native) return null;
  return (await native.factsForAssetAsync(localId)) ?? null;
}
