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
 * own button means the counts can be looked at without downloading anything —
 * and means the albums worth fetching from can be chosen first. See fetch.ts.
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
