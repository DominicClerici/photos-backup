"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  fetchTimeline,
  fetchTimelineDays,
  fetchTimelineIndex,
  type TimelineFilter,
  type TimelineItem,
  type View,
} from "@/lib/api";
import { DEFAULT_VIEW, viewKey } from "@/lib/view";
import {
  countOf,
  dayIndexOf,
  dayKeyOf,
  daysFrom,
  type Day,
  type ItemRange,
} from "@/lib/layout";

/**
 * Items per request. Also the granularity everything else is aligned to: a page
 * is identified by its number, so the same stretch of timeline is never fetched
 * twice under two different offsets.
 */
const PAGE_SIZE = 200;

/**
 * How many pages may be in flight at once.
 *
 * At the smallest zoom a screenful plus overscan is a few thousand tiles — a
 * dozen pages — and firing all of them at a server that is also generating
 * thumbnails helps nobody. Three keeps the pipe full while leaving the ordering
 * below room to matter.
 */
const MAX_IN_FLIGHT = 3;

/**
 * How many times a disagreement between the day table and the pages drawn from
 * it may be resolved by starting over.
 *
 * A photo uploaded while the gallery is open shifts every index after it, and
 * one resync is the honest fix. A cap is here because the alternative to a
 * bounded number of wasted fetches is an infinite number of them, and if the
 * two genuinely cannot be reconciled, a timeline that is slightly wrong beats a
 * browser tab pinned at 100%.
 */
const MAX_RESYNCS = 3;

export interface TimelineState {
  /** Every heading the collection will draw, with its size and its position. */
  days: Day[];
  /** How many items the collection holds — known before any of them arrive. */
  total: number;
  /** False until the day table lands. There is no geometry before that. */
  ready: boolean;
  /**
   * Whether a day table is in flight. True on the first load and again whenever
   * the collection is reordered, refiltered or reloaded after a delete.
   *
   * Which is the window in which `days` describes a timeline nobody is looking
   * at any more — the old table is deliberately kept until the new one lands,
   * so the grid does not collapse and flicker on every reload. Anything reading
   * a position out of it has to know that, and the calendar does.
   */
  loading: boolean;
  error: string | null;
  retry: () => void;
  /** The item at a position, or undefined while it is still being fetched. */
  at: (index: number) => TimelineItem | undefined;
  /** Where an item sits, or -1 if it is not in hand. */
  indexOf: (id: string) => number;
  /**
   * Where an item sits, asking the server when it is not in hand. -1 means this
   * timeline does not hold it at all.
   *
   * The counterpart to `indexOf` for the one case it cannot answer: a link
   * somebody shared names a photograph that may be five years and two hundred
   * pages down. One count on the server settles it.
   */
  locate: (id: string, signal?: AbortSignal) => Promise<number>;
  /**
   * Declares a range this caller needs; the store fetches whatever it lacks.
   *
   * Keyed because there is more than one caller and they are looking at
   * different things: the grid wants what is under the viewport, the viewer
   * wants the photo it is showing and the two either side of it. The union of
   * the standing ranges is what gets fetched, and an empty range retires a key
   * — a viewer that has closed should not still be pinning a page.
   */
  request: (key: string, start: number, end: number) => void;
  /** Replaces items by id, for tiles whose derivatives finished after loading. */
  patch: (updated: TimelineItem[]) => void;
}

/**
 * A timeline addressed by position rather than by arrival order.
 *
 * The old shape of this hook was a list that grew: the grid could only be as
 * tall as what had been fetched, so scrolling met a wall and the scrollbar
 * shrank under the pointer every time a page landed. This one starts from the
 * server's day table — the size and order of every heading in the collection —
 * which fixes the geometry of the whole thing before a single photograph is
 * requested. `items` is then a sparse array over that geometry: index 4,812 is
 * a real place in the grid whether or not anything has been fetched for it.
 *
 * Which is what makes the fetching demand-driven rather than sequential. The
 * grid says which range it is looking at; this asks for the pages covering it,
 * nearest the middle first, three at a time, and abandons requests for ground
 * the user has already scrolled past. Paging down still walks a keyset cursor,
 * because a page that continues from the one before it can say so; only a jump
 * into unvisited territory pays for a row offset.
 *
 * The filter is read through a ref rather than a dependency, because it is an
 * object literal at every call site. A collection only changes when the route
 * does, at which point the hook is remounted anyway.
 *
 * The view is read the same way and is not the same case: it changes under
 * somebody's hands, without the route moving and without a remount. So it is
 * the one thing here with a dependency of its own — a string rather than the
 * object, so that an equal view spelled twice does not refetch the archive.
 * Everything else about a reorder is a reload, which is what the day table
 * already knows how to be.
 */
export function useTimeline(filter?: TimelineFilter, view: View = DEFAULT_VIEW): TimelineState {
  const [days, setDays] = useState<Day[]>([]);
  const [total, setTotal] = useState(0);
  const [ready, setReady] = useState(false);
  const [refreshing, setRefreshing] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Bumped whenever a page lands. The item array itself lives in a ref: it is
  // one slot per photo in the archive, and copying it to announce that 200 of
  // them changed would be the most expensive thing this hook does.
  const [version, bump] = useState(0);

  const asked = useRef(filter);
  asked.current = filter;
  const looking = useRef(view);
  looking.current = view;
  const key = viewKey(view);

  const items = useRef<(TimelineItem | undefined)[]>([]);
  const byID = useRef(new Map<string, number>());

  /** Pages already in hand, and the cursor that starts each one we can reach. */
  const done = useRef(new Set<number>());
  const cursors = useRef(new Map<number, string>());
  const inFlight = useRef(new Map<number, AbortController>());
  /**
   * What each caller says it is looking at, in item indices. Seeded with the
   * first page so it starts arriving the moment the day table does, rather than
   * waiting for the grid to measure itself and ask.
   */
  const want = useRef(new Map<string, ItemRange>([["grid", { start: 0, end: 1 }]]));

  const model = useRef<Day[]>([]);
  const resyncs = useRef(0);
  const resync = useRef<(() => void) | null>(null);
  // Held rather than left to the effect's cleanup, because a resync calls load
  // directly and there is nobody to hand a cleanup to. Two tables racing would
  // otherwise be settled by whichever server response happened to be slower.
  const loading = useRef<AbortController | null>(null);

  const clear = useCallback(() => {
    for (const controller of inFlight.current.values()) controller.abort();
    inFlight.current.clear();
    done.current.clear();
    cursors.current.clear();
    byID.current.clear();
    items.current = [];
  }, []);

  /**
   * Starts the day table over, because the one in hand describes a timeline
   * that no longer exists. Reports whether it did — once the budget is spent it
   * refuses, and the caller has to make do with the table it has.
   */
  const stale = useCallback((): boolean => {
    if (resyncs.current >= MAX_RESYNCS) return false;
    resyncs.current++;
    resync.current?.();
    return true;
  }, []);

  /**
   * Writes a page into its slots, unless it does not belong in them. Returns
   * whether it was kept.
   */
  const accept = useCallback(
    (page: number, fetched: TimelineItem[]): boolean => {
      const start = page * PAGE_SIZE;

      // Dropped only if starting over is still on the table. With the budget
      // spent it is written where it landed instead: a page that keeps being
      // rejected is a page that keeps being refetched, and a grid that is a
      // little wrong beats one that never stops asking.
      if (misplaced(model.current, start, fetched) && stale()) return false;

      for (let n = 0; n < fetched.length; n++) {
        items.current[start + n] = fetched[n];
        byID.current.set(fetched[n].id, start + n);
      }
      return true;
    },
    [stale],
  );

  /**
   * Fills the pages covering the wanted range, nearest its middle first.
   *
   * Re-entered after every page lands and every time the grid moves, which is
   * what makes it a scheduler rather than a loop: the cheapest way to stop
   * fetching a stretch of timeline is for it to stop being wanted.
   */
  const pump = useCallback(() => {
    const days = model.current;
    if (days.length === 0) return;

    const count = countOf(days);

    // Every page any caller is asking for, and how far it sits from the middle
    // of the nearest thing asking for it — which is the order they are worth
    // fetching in, because the middle of a range is what somebody is looking at.
    const distance = new Map<number, number>();
    for (const range of want.current.values()) {
      const from = Math.max(0, Math.min(range.start, count - 1));
      const to = Math.max(from + 1, Math.min(range.end, count));
      const middle = (from + to) / 2 / PAGE_SIZE;
      for (
        let page = Math.floor(from / PAGE_SIZE);
        page <= Math.floor((to - 1) / PAGE_SIZE);
        page++
      ) {
        const away = Math.abs(page - middle);
        const held = distance.get(page);
        if (held === undefined || away < held) distance.set(page, away);
      }
    }

    // A request for ground the user has scrolled away from is worth less than
    // the slot it occupies. Dropping it is free — nothing has been written yet.
    for (const [page, controller] of inFlight.current) {
      if (!distance.has(page)) {
        controller.abort();
        inFlight.current.delete(page);
      }
    }

    const queue = [...distance.keys()].filter(
      (page) => !done.current.has(page) && !inFlight.current.has(page),
    );
    queue.sort((a, b) => distance.get(a)! - distance.get(b)!);

    for (const page of queue) {
      if (inFlight.current.size >= MAX_IN_FLIGHT) break;
      fetchPage(page);
    }

    function fetchPage(page: number) {
      const controller = new AbortController();
      inFlight.current.set(page, controller);

      // A cursor when the page before this one handed us one, which is every
      // page reached by scrolling down; a row offset only for a page nothing
      // has walked to. See PageStart in lib/api.
      const cursor = cursors.current.get(page);
      const start = cursor ? { cursor } : { skip: page * PAGE_SIZE };

      fetchTimeline(start, PAGE_SIZE, asked.current, looking.current, controller.signal)
        .then((fetched) => {
          if (inFlight.current.get(page) !== controller) return;
          inFlight.current.delete(page);
          if (!accept(page, fetched.items)) return;
          done.current.add(page);
          if (fetched.next_cursor && fetched.items.length === PAGE_SIZE) {
            cursors.current.set(page + 1, fetched.next_cursor);
          }
          // A page that worked clears a page that did not: the notice is about
          // whether the timeline is reachable, and it plainly is.
          setError(null);
          bump((n) => n + 1);
          pump();
        })
        .catch((err: unknown) => {
          if (inFlight.current.get(page) !== controller) return;
          inFlight.current.delete(page);
          if (controller.signal.aborted) return;
          // Surfaced without retrying. A page that failed is a stretch of
          // placeholders that stays placeholders, which is a far smaller
          // failure than the grid used to have — and scrolling away and back
          // asks again, because the page is no longer marked as in flight.
          setError(err instanceof Error ? err.message : "could not load the timeline");
        });
    }
  }, [accept]);

  // The day table, and everything hanging off it. Refetched from scratch rather
  // than reconciled: it is one request, and the alternative is a merge that has
  // to be right about a timeline that just changed underneath it.
  const load = useCallback(() => {
    const controller = new AbortController();
    loading.current?.abort();
    loading.current = controller;
    clear();
    setError(null);
    setRefreshing(true);

    fetchTimelineDays(asked.current, looking.current, controller.signal)
      .then((table) => {
        if (controller.signal.aborted) return;
        const built = daysFrom(table.days);
        model.current = built;
        items.current = new Array(table.total);
        setDays(built);
        setTotal(table.total);
        setReady(true);
        setRefreshing(false);
        bump((n) => n + 1);
        pump();
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // Fatal in a way a missing page is not: with no table there is no grid
        // to put placeholders in, so this is the one error worth stopping for.
        setError(err instanceof Error ? err.message : "could not load the timeline");
        setReady(true);
        setRefreshing(false);
      });

    return () => controller.abort();
  }, [clear, pump]);

  resync.current = load;

  // `key` rather than `view`: an equal view spelled as a second object literal
  // is the same timeline, and refetching it would cost a day table and every
  // page on screen for nothing.
  useEffect(() => load(), [load, key]);
  useEffect(() => clear, [clear]);

  const request = useCallback(
    (key: string, start: number, end: number) => {
      const held = want.current.get(key);
      if (end <= start) {
        if (!held) return;
        want.current.delete(key);
      } else {
        if (held && held.start === start && held.end === end) return;
        want.current.set(key, { start, end });
      }
      pump();
    },
    [pump],
  );

  const retry = useCallback(() => {
    resyncs.current = 0;
    load();
  }, [load]);

  const at = useCallback((index: number) => items.current[index], []);
  const indexOf = useCallback((id: string) => byID.current.get(id) ?? -1, []);

  const locate = useCallback(async (id: string, signal?: AbortSignal) => {
    const held = byID.current.get(id);
    return held ?? fetchTimelineIndex(id, asked.current, looking.current, signal);
  }, []);

  const patch = useCallback((updated: TimelineItem[]) => {
    let changed = false;
    for (const fresh of updated) {
      const index = byID.current.get(fresh.id);
      if (index === undefined) continue;
      const held = items.current[index];
      if (!held || held.state === fresh.state) continue;
      items.current[index] = fresh;
      changed = true;
    }
    if (changed) bump((n) => n + 1);
  }, []);

  // `version` is in here so that a landed page reaches the grid: the items it
  // wrote are behind a ref, and this object's identity is the only thing React
  // can see change.
  return useMemo(
    () => ({
      days,
      total,
      ready,
      loading: refreshing,
      error,
      retry,
      at,
      indexOf,
      locate,
      request,
      patch,
    }),
    [days, total, ready, refreshing, error, retry, at, indexOf, locate, request, patch, version],
  );
}

/**
 * Whether a page belongs anywhere but where its indices put it.
 *
 * Every item is checked against the heading it would land under. They can
 * disagree: a photo uploaded while the gallery is open is inserted at the top
 * and pushes every index after it down by one, so the table the grid is drawn
 * from describes a timeline that no longer exists. Checking here rather than
 * polling for it costs one string comparison per item on a page that was being
 * decoded anyway, and it is exact for the case that matters — a page about to
 * be drawn under the wrong dates.
 *
 * It is not exact for every case, and deliberately so: a shift entirely inside
 * one long day moves photos without moving the heading they sit under, and
 * nothing here can see that. The consequence is a tile a square from where it
 * belongs until the next mount, which is not worth a poll to find.
 */
function misplaced(days: Day[], start: number, fetched: TimelineItem[]): boolean {
  for (let n = 0; n < fetched.length; n++) {
    const index = start + n;
    const day = days[dayIndexOf(days, index)];
    if (!day || index >= day.start + day.count) return true;
    // A run with no date makes no claim about which day anything falls under —
    // see lib/layout.headless — so the bounds check above is the whole of what
    // can be checked, and it is still worth checking: an upload during a
    // reorder changes the count as surely as it changes the headings.
    if (day.key === "") continue;
    const item = fetched[n];
    if (dayKeyOf(item.taken_at, item.offset_minutes) !== day.key) return true;
  }
  return false;
}
