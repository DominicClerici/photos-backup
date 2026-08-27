import {
  DEFAULT_VIEW,
  type DayTable,
  type TimelineFilter,
  type TimelineItem,
  type View,
} from '@photobackup/core';
import {
  onAlbumsChanged,
  useTimeline,
  type TimelineState,
  type TimelineStore,
} from '@photobackup/core/react';
import * as SQLite from 'expo-sqlite';
import { useCallback, useMemo } from 'react';

const DATABASE = 'photobackup-gallery.db';

/**
 * How long a cached answer is worth drawing.
 *
 * A week, because the thing being cached is the *shape* of an archive rather
 * than its contents: how many photographs were taken on each day of the last
 * fifteen years does not move much, and a table a few days out of date draws a
 * grid whose geometry is right everywhere except the top. Anything older than
 * this is a phone that has been away from the archive long enough that showing
 * it last month's timeline would be a lie about what is in there now.
 */
const TTL_MS = 7 * 24 * 60 * 60 * 1000;

/**
 * How many collections are kept, most recently opened first.
 *
 * A dozen is the library in its default order plus a handful of albums, a
 * person or two and whatever was being sorted differently. Past that it is
 * ground somebody walked once, and a phone should not be carrying a copy of
 * every album it has ever opened.
 */
const MOST_KEYS = 12;

/**
 * How many pages are kept per collection. Forty is eight thousand items —
 * comfortably more than a session's scrolling, and small enough that the
 * database stays a few megabytes rather than a few hundred.
 */
const MOST_PAGES = 40;

/** How many page writes go by between prunes. See `prune`. */
const PRUNE_EVERY = 20;

/**
 * Where the gallery keeps what it has already seen.
 *
 * The browser has no offline story and needs none — it is on the machine's
 * network or it is not open. A phone is routinely out of reach of the archive,
 * and a gallery that is a blank screen on the train is not a gallery. So this
 * is the store `useTimeline` has taken since Phase 3 and passed nothing for
 * ever since: the day table paints the geometry before the network answers, and
 * pages already fetched come back while their refetch is in flight.
 *
 * Four tables, all of them disposable. Nothing here is ever the truth about the
 * archive — the archive is — so every read may answer null, every write may
 * quietly not happen, and no failure in this file is allowed to reach a screen.
 * See TimelineStore in core.
 *
 * **Nothing from the vault is ever written here.** A hidden photograph's
 * filename, its capture date and how many of them there are are exactly the
 * facts the vault exists to keep off the disk, and a cache that held them would
 * be a plaintext copy of the sealed documents sitting in the app's sandbox
 * after the server had re-locked. `storeFor` hands the vault no store at all,
 * and every method below refuses a vault key besides — the belt is for the day
 * somebody adds a fifth call site and forgets the braces.
 */
let opening: Promise<SQLite.SQLiteDatabase | null> | null = null;

function db(): Promise<SQLite.SQLiteDatabase | null> {
  opening ??= SQLite.openDatabaseAsync(DATABASE)
    .then(async (handle) => {
      await handle.execAsync(`
        pragma journal_mode = WAL;
        create table if not exists days (
          key text primary key,
          json text not null,
          saved_at integer not null
        );
        create table if not exists pages (
          key text not null,
          page integer not null,
          json text not null,
          saved_at integer not null,
          primary key (key, page)
        );
        create table if not exists seen (
          key text primary key,
          seen_at integer not null
        );
        create table if not exists documents (
          key text primary key,
          json text not null,
          saved_at integer not null
        );
      `);
      return handle;
    })
    .catch(() => {
      // A database that will not open is a gallery that is merely as slow as
      // the browser's. It is not a gallery that fails to start.
      return null;
    });
  return opening;
}

/** Whether this key names a timeline inside the vault. See the note above. */
function sealed(key: string): boolean {
  return key.startsWith('vault:');
}

function fresh(savedAt: number): boolean {
  return Date.now() - savedAt < TTL_MS;
}

let writes = 0;

/**
 * Bumped the moment a write happens, before the delete it starts has run.
 *
 * The reload that follows a write re-enters `useTimeline`, which asks this
 * store for the day table again — in the same tick, and long before SQLite has
 * finished emptying itself. Without an epoch it would be handed the table the
 * write had just invalidated and paint it for as long as the archive took to
 * answer. Every read takes a copy on the way in and answers null if the number
 * moved underneath it; every write does the same and drops what it was going
 * to save. One integer, checked in seven places, and the alternative is a
 * promise chain through a function whose whole contract is that it never
 * rejects.
 */
let epoch = 0;

/**
 * Drops what has fallen out of the budget: the collections nobody has opened
 * lately, and the pages furthest from the last thing read in the ones that are
 * left.
 *
 * Run after a day table lands — once per timeline load — and every twentieth
 * page, because a long scroll through a large archive writes hundreds of pages
 * between one day table and the next.
 */
async function prune(handle: SQLite.SQLiteDatabase): Promise<void> {
  // Expiry first, so that a phone which browsed twelve collections a fortnight
  // ago and has not opened the app since does not keep paying for them.
  const cutoff = Date.now() - TTL_MS;
  await handle.runAsync('delete from days where saved_at < ?', [cutoff]);
  await handle.runAsync('delete from pages where saved_at < ?', [cutoff]);
  await handle.runAsync('delete from documents where saved_at < ?', [cutoff]);

  await handle.execAsync(`
    delete from seen where key not in (
      select key from seen order by seen_at desc limit ${MOST_KEYS}
    );
    delete from seen where key not in (select key from days);
    delete from days where key not in (select key from seen);
    delete from pages where key not in (select key from seen);
    delete from pages where rowid not in (
      select rowid from pages p
        where (select count(*) from pages q
                where q.key = p.key and q.saved_at > p.saved_at) < ${MOST_PAGES}
    );
  `);
}

async function touch(handle: SQLite.SQLiteDatabase, key: string): Promise<void> {
  await handle.runAsync(
    'insert into seen (key, seen_at) values (?, ?) on conflict(key) do update set seen_at = excluded.seen_at',
    [key, Date.now()]
  );
}

/**
 * The store, as `useTimeline` wants one.
 *
 * Every method swallows its own failures rather than rejecting: a cache that
 * can throw is a cache that can take the gallery down with it, and there is
 * nothing a caller could usefully do about a corrupt row anyway.
 */
export const timelineCache: TimelineStore = {
  async days(key: string): Promise<DayTable | null> {
    if (sealed(key)) return null;
    const at = epoch;
    try {
      const handle = await db();
      if (!handle) return null;
      const row = await handle.getFirstAsync<{ json: string; saved_at: number }>(
        'select json, saved_at from days where key = ?',
        [key]
      );
      if (!row || !fresh(row.saved_at) || at !== epoch) return null;
      void touch(handle, key);
      return JSON.parse(row.json) as DayTable;
    } catch {
      return null;
    }
  },

  async page(key: string, page: number): Promise<TimelineItem[] | null> {
    if (sealed(key)) return null;
    const at = epoch;
    try {
      const handle = await db();
      if (!handle) return null;
      const row = await handle.getFirstAsync<{ json: string; saved_at: number }>(
        'select json, saved_at from pages where key = ? and page = ?',
        [key, page]
      );
      if (!row || !fresh(row.saved_at) || at !== epoch) return null;
      return JSON.parse(row.json) as TimelineItem[];
    } catch {
      return null;
    }
  },

  saveDays(key: string, table: DayTable): void {
    if (sealed(key)) return;
    const at = epoch;
    void (async () => {
      try {
        const handle = await db();
        if (!handle || at !== epoch) return;
        await handle.runAsync(
          'insert into days (key, json, saved_at) values (?, ?, ?) on conflict(key) do update set json = excluded.json, saved_at = excluded.saved_at',
          [key, JSON.stringify(table), Date.now()]
        );
        await touch(handle, key);
        await prune(handle);
      } catch {
        // Nothing on screen depends on this having happened.
      }
    })();
  },

  savePage(key: string, page: number, items: TimelineItem[]): void {
    if (sealed(key)) return;
    const at = epoch;
    void (async () => {
      try {
        const handle = await db();
        if (!handle || at !== epoch) return;
        await handle.runAsync(
          'insert into pages (key, page, json, saved_at) values (?, ?, ?, ?) on conflict(key, page) do update set json = excluded.json, saved_at = excluded.saved_at',
          [key, page, JSON.stringify(items), Date.now()]
        );
        if (++writes % PRUNE_EVERY === 0) await prune(handle);
      } catch {
        // Likewise.
      }
    })();
  },
};

/**
 * Which store a timeline gets: the cache, or nothing at all for the vault.
 *
 * Exported so that the one screen that must not cache says so by calling this
 * with its own filter rather than by remembering not to. See the note on the
 * module.
 */
export function storeFor(filter?: TimelineFilter): TimelineStore | null {
  return filter?.kind === 'vault' ? null : timelineCache;
}

/**
 * How long a rendition's bytes may be kept, as `expo-image` spells it.
 *
 * The other half of the rule this module is named for, and it lives here so
 * that both halves are stated in one file. A thumbnail or a preview out of the
 * vault is decrypted on its way through photod and looks exactly like an
 * ordinary one — so left to the default it would be written into expo-image's
 * disk cache and would still be sitting there, in the clear, long after the
 * server had re-locked. `memory` is the whole fix: the tiles and the viewer are
 * as fast as ever for as long as somebody is looking, and there is nothing left
 * behind when they are not.
 */
export type MediaCache = 'memory' | 'memory-disk';

export function mediaCacheFor(sealed: boolean): MediaCache {
  return sealed ? 'memory' : 'memory-disk';
}

// The small documents: the collections index, and an album's own title.
//
// Not timelines, and kept apart from them for that reason — they are one row
// each, they answer a screen that would otherwise be an error message, and they
// are dropped on exactly the same events. The alternative is a Collections tab
// that is a spinner and then a failure while every album behind it has a week
// of cached geometry, which is a stranger thing for an app to be than either
// half on its own.

/** What is cached under each key. Two kinds so far; the shapes are the wire's. */
export const COLLECTIONS = 'collections';
export const albumKey = (id: string) => `album:${id}`;

export async function recall<T>(key: string): Promise<T | null> {
  const at = epoch;
  try {
    const handle = await db();
    if (!handle) return null;
    const row = await handle.getFirstAsync<{ json: string; saved_at: number }>(
      'select json, saved_at from documents where key = ?',
      [key]
    );
    if (!row || !fresh(row.saved_at) || at !== epoch) return null;
    return JSON.parse(row.json) as T;
  } catch {
    return null;
  }
}

export function remember(key: string, value: unknown): void {
  const at = epoch;
  void (async () => {
    try {
      const handle = await db();
      if (!handle || at !== epoch) return;
      await handle.runAsync(
        'insert into documents (key, json, saved_at) values (?, ?, ?) on conflict(key) do update set json = excluded.json, saved_at = excluded.saved_at',
        [key, JSON.stringify(value), Date.now()]
      );
    } catch {
      // Same bargain as everything else here.
    }
  })();
}

/**
 * Everything cached, gone.
 *
 * Deliberately the whole cache rather than one key, and deliberately not a
 * merge — the same reasoning `useTimeline` gives for refetching the day table
 * rather than reconciling it, taken one step coarser. A delete moves every
 * index after it, in the library and in every album that held one of those
 * photographs; a hide takes them out of all of them at once; filing changes
 * what is in an album and what its cover is. Working out which of a dozen
 * cached collections a write touched is a calculation whose wrong answers are
 * silent and whose right answer saves a few hundred milliseconds of refetching
 * once. Dropping the lot is one statement that is never wrong.
 *
 * Called from two places: the reload every write already performs, through
 * `useCachedTimeline` below, and core's album broadcast, which is what covers
 * the writes that change an album without changing the timeline on screen.
 */
export function archiveChanged(): void {
  epoch++;
  void (async () => {
    try {
      const handle = await db();
      if (!handle) return;
      await handle.execAsync(
        'delete from days; delete from pages; delete from seen; delete from documents;'
      );
    } catch {
      // A cache that could not be emptied is a cache with a TTL on it.
    }
  })();
}

/** Wired once, by the root layout, before the first screen mounts. */
export function installCacheInvalidation(): () => void {
  return onAlbumsChanged(archiveChanged);
}

/**
 * A timeline read through the cache, and the reload that empties it.
 *
 * Every grid in the app takes its timeline from here rather than calling
 * `useTimeline` directly, which is what makes the two rules above hold
 * everywhere at once: the vault gets no store, and the write that reloads a
 * grid is the write that drops the copy.
 */
export function useCachedTimeline(
  filter?: TimelineFilter,
  view: View = DEFAULT_VIEW
): { timeline: TimelineState; reload: () => void } {
  const store = useMemo(() => storeFor(filter), [filter?.kind]);
  const timeline = useTimeline(filter, view, store);
  const { retry } = timeline;

  const reload = useCallback(() => {
    archiveChanged();
    retry();
  }, [retry]);

  return { timeline, reload };
}
