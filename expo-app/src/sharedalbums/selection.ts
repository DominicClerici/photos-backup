/**
 * Which shared albums are being backed up.
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
 * Nothing is in until it is ticked, and an empty list is the honest starting
 * state rather than a missing answer. That is a change from what this file said
 * while the shared albums were only being surveyed, where an unanswered question
 * defaulted to "all of them" because looking at everything cost nothing. Ticking
 * an album now means uploading it, and a default that quietly enrolled every
 * album on the phone — including one joined months from now, by somebody else —
 * would be a backup nobody asked for. The cost is the honest one: an album
 * joined later is not backed up until it is ticked.
 */

import { File, Paths } from 'expo-file-system';

const SELECTION_FILE = new File(Paths.document, 'photobackup-shared-albums.json');

type Stored = { albumIds: string[] };

/** The chosen album ids, and empty when nothing has been chosen. */
export function loadSelection(): string[] {
  try {
    if (!SELECTION_FILE.exists) return [];
    const parsed = JSON.parse(SELECTION_FILE.textSync()) as Partial<Stored>;
    if (!Array.isArray(parsed?.albumIds)) return [];
    return parsed.albumIds.filter((id): id is string => typeof id === 'string');
  } catch {
    return [];
  }
}

export function saveSelection(albumIds: string[]) {
  try {
    if (!SELECTION_FILE.exists) SELECTION_FILE.create({ overwrite: true });
    SELECTION_FILE.write(JSON.stringify({ albumIds } satisfies Stored));
  } catch {}
}
