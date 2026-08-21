/**
 * The native half of a fetch run: the same loop run.ts describes, wired to the
 * phone.
 *
 * There is nothing here but the wiring, and that is the point of the split. Every
 * decision the run makes — how long to wait, when to try again, when iCloud has
 * clearly had enough — lives in run.ts where a test can drive it without a phone,
 * an Apple ID or a network. What is left over is this file, which cannot be
 * tested and does not need to be.
 */

import {
  photoKitOnSharedFetchProgress,
  photoKitReadSharedResource,
  type SharedAsset,
} from '../../modules/photo-facts';
import { systemClock } from '../sync/types';
import { runFetches, type FetchRun } from './run';

/**
 * Fetches these assets from iCloud, reporting itself as it goes.
 *
 * `cancelled` is asked rather than told: the caller keeps a ref and flips it,
 * because a run started from a React callback closes over the state as it was
 * when the button was pressed, and a stop flag read out of that closure would be
 * permanently false.
 */
export function fetchSharedAssets(
  assets: SharedAsset[],
  onProgress: (run: FetchRun) => void,
  cancelled: () => boolean
): Promise<FetchRun> {
  return runFetches(assets, {
    read: photoKitReadSharedResource,
    clock: systemClock,
    onProgress,
    cancelled,
    subscribe: photoKitOnSharedFetchProgress,
  });
}
