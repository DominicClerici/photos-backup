/**
 * The shared-album probe: what is in the iCloud Shared Albums on this phone, and
 * what actually comes back when you ask iCloud for one.
 *
 * Scaffolding, in the same sense src/gallery/check.ts is scaffolding — it exists
 * to answer a question before anything is built on the answer. Backing up shared
 * albums means teaching the queue about assets that have no local original,
 * which is a schema change, a third branch in MediaSource.open(), and a
 * network-dependent upload path. All of that is worth doing only if Apple's
 * copies are worth keeping, and nothing in this repository knows yet whether
 * they are.
 *
 * So this reads and reports. It enqueues nothing, uploads nothing, and writes no
 * file: the survey is metadata PhotoKit already holds, and the fetch counts the
 * bytes it receives rather than keeping them.
 *
 * The two halves are separate calls on purpose. Surveying is instant and
 * offline; fetching goes to Apple over the network, and putting it behind its
 * own button means the counts can be looked at without downloading anything.
 */

import {
  photoKitReadSharedResource,
  photoKitSharedAlbums,
  type SharedAsset,
  type SharedResourceRead,
} from '../../modules/photo-facts';
import { errorText } from '../sync/types';
import { emptySurvey, summarize, type SharedSurvey } from './summary';

/** One sample asset, and what asking iCloud for it produced. */
export type SampleRead = {
  asset: SharedAsset;
  read: SharedResourceRead | null;
  /** Null when the fetch succeeded. Set to whatever went wrong when it did not. */
  error: string | null;
};

/**
 * Every shared album on the phone, folded into one description.
 *
 * A build with no shared-album enumerator in it comes back `supported: false`
 * rather than empty, because a dev client older than this file is the ordinary
 * case here and "there are no shared albums" is a completely different finding
 * from "this binary cannot see them".
 */
export async function surveySharedAlbums(): Promise<SharedSurvey> {
  const albums = await photoKitSharedAlbums();
  if (albums === null) return emptySurvey(false);
  return summarize(albums);
}

/**
 * Reads each sample asset's bytes from iCloud, in turn.
 *
 * One at a time, and that is the whole reason this is a loop rather than a
 * Promise.all: the elapsed time is the point of the exercise, and three fetches
 * sharing one connection would each report the sum of the others' contention.
 * Serial is slower and is the only version whose numbers mean anything.
 *
 * A failure is recorded against its asset and the rest still run. The survey is
 * trying to find out how this fails, so one asset that has been withdrawn from
 * its album must not take the readings for the two that have not.
 */
export async function readSamples(sample: SharedAsset[]): Promise<SampleRead[]> {
  const results: SampleRead[] = [];

  for (const asset of sample) {
    try {
      const read = await photoKitReadSharedResource(asset.localId);
      results.push({
        asset,
        read,
        // Null from a build that carries the function is PhotoKit saying the
        // identifier names nothing any more, which is a result and not an error
        // — but it is not a successful read either, and must not be shown as one.
        error: read === null ? 'the asset is no longer in the library' : null,
      });
    } catch (e) {
      results.push({ asset, read: null, error: errorText(e) });
    }
  }

  return results;
}
