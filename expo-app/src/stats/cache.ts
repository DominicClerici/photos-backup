import { File, Paths } from 'expo-file-system';

import type { Stats } from '../gallery/types';

const CACHE_FILE = new File(Paths.document, 'photobackup-stats.json');

/**
 * The last stats photod answered, and when.
 *
 * This is a cache, not a source of truth, and the distinction is the whole point
 * of moving these numbers server-side: the archive knows what it holds, and this
 * file only remembers what it last said. It is never added to, never counted
 * against, and never repaired — a bad entry is fixed by the next successful
 * fetch overwriting it.
 *
 * It exists so an unreachable server shows yesterday's figures marked stale
 * rather than a blank card, which is the more honest of the two: the photos are
 * still archived whether or not the phone can reach the server to say so.
 */
export type CachedStats = {
  /** Device clock, from Date.now(), at the moment the response arrived. */
  fetchedAt: number;
  stats: Stats;
};

export function loadCachedStats(): CachedStats | null {
  try {
    if (!CACHE_FILE.exists) return null;
    const parsed = JSON.parse(CACHE_FILE.textSync()) as Partial<CachedStats>;
    if (!parsed?.stats?.device || !parsed.stats.archive) return null;
    if (typeof parsed.fetchedAt !== 'number' || !Number.isFinite(parsed.fetchedAt)) return null;
    return { fetchedAt: parsed.fetchedAt, stats: parsed.stats };
  } catch {
    return null;
  }
}

export function saveCachedStats(entry: CachedStats) {
  try {
    if (!CACHE_FILE.exists) CACHE_FILE.create({ overwrite: true });
    CACHE_FILE.write(JSON.stringify(entry));
  } catch {}
}

/**
 * Forgets the cached numbers. Called when a pairing is dropped: they describe
 * what *that* device had archived, and showing them beside a pairing form would
 * credit a new device with another one's backup.
 */
export function clearCachedStats() {
  try {
    if (CACHE_FILE.exists) CACHE_FILE.delete();
  } catch {}
}
