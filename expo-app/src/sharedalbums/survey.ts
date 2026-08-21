/**
 * What is in the iCloud Shared Albums on this phone.
 *
 * This began as a probe, and the question it was built to answer has been
 * answered: Apple's copies are worth keeping — 4,400 of 4,700 shared stills came
 * back above the documented 2,048-pixel cap, so what is in a shared album is not
 * the downscale the documentation describes. The queue knows about them now, and
 * the albums ticked on the screen this feeds are the ones the next run archives.
 *
 * What is left here is still only reading. It enqueues nothing and writes no
 * file: it is the listing the picker is drawn from, and it is instant and
 * offline because everything it reports is metadata PhotoKit already holds.
 *
 * The byte fetch that sits beside it on the screen has stayed a probe on
 * purpose — see fetch.ts. It is the dry run: a way to find out what iCloud does
 * to a phone asking for hundreds of assets in a row without committing an
 * upload to the answer.
 */

import { photoKitSharedAlbums } from '../../modules/photo-facts';
import type { SharedLibrary } from './summary';

/**
 * Every shared album on the phone.
 *
 * A build with no shared-album enumerator in it comes back `supported: false`
 * rather than empty, because a dev client older than this file is the ordinary
 * case here and "there are no shared albums" is a completely different finding
 * from "this binary cannot see them".
 *
 * The albums are handed back whole, assets and all, rather than summarized on
 * the way through. Summarizing is what happens to a *selection* of them, and the
 * selection changes every time a checkbox does — re-reading the library for that
 * would be a PhotoKit fetch per tap.
 */
export async function surveySharedAlbums(): Promise<SharedLibrary> {
  const albums = await photoKitSharedAlbums();
  if (albums === null) return { supported: false, albums: [] };
  return { supported: true, albums };
}
