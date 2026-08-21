/**
 * Which shared albums are meant to be imported.
 *
 * A decision rather than a cache, which is what separates this from
 * src/stats/cache.ts: nothing recomputes it, and losing the file loses something
 * a person chose. It is stored next to the stats cache and the config all the
 * same, because it is small, not secret, and useless on another phone.
 *
 * The album identifiers are PHAssetCollection's, so they survive a rename and do
 * not survive the album being left and rejoined. Stale ones are harmless: an id
 * that matches no album on the phone selects nothing, and is dropped the next
 * time a selection is saved.
 *
 * Null and empty are different answers and the file preserves both. Nobody has
 * chosen yet means every album is in, which is the right default for a backup —
 * a new album should not be silently excluded because it appeared after the
 * choosing. Somebody has chosen nothing means nothing, and must not be quietly
 * turned back into everything.
 */

import { File, Paths } from 'expo-file-system';

const SELECTION_FILE = new File(Paths.document, 'photobackup-shared-albums.json');

type Stored = { albumIds: string[] };

/** The chosen album ids, or null when no choice has ever been made. */
export function loadSelection(): string[] | null {
  try {
    if (!SELECTION_FILE.exists) return null;
    const parsed = JSON.parse(SELECTION_FILE.textSync()) as Partial<Stored>;
    if (!Array.isArray(parsed?.albumIds)) return null;
    return parsed.albumIds.filter((id): id is string => typeof id === 'string');
  } catch {
    return null;
  }
}

export function saveSelection(albumIds: string[]) {
  try {
    if (!SELECTION_FILE.exists) SELECTION_FILE.create({ overwrite: true });
    SELECTION_FILE.write(JSON.stringify({ albumIds } satisfies Stored));
  } catch {}
}
