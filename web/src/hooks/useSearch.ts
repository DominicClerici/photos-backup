"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  fetchSearch,
  type ParsedQuery,
  type SearchResult,
  type Target,
  type TimelineItem,
} from "@/lib/api";
import { asks, explicitParams } from "@/lib/search";
import type { Day, ItemRange } from "@/lib/layout";
import type { SelectionActions } from "./useSelection";
import { useTrashActions } from "./useTrash";
import type { TimelineState } from "./useTimeline";

/**
 * Results per request.
 *
 * Smaller than the timeline's 200 because every row here carries its evidence —
 * a caption, its tags, and a headline cut out of the recognised text — and
 * because a ranked answer is read from the top rather than scrolled through.
 */
const PAGE_SIZE = 100;

/** How many pages may be in flight at once. See useTimeline. */
const MAX_IN_FLIGHT = 3;

/**
 * How far a walk through the ranking will go looking for one photograph.
 *
 * Only two things ask: a link naming an asset that was not on the first page,
 * and a selection covering positions nobody has scrolled to. Both are worth a
 * few requests and neither is worth paging through a structural query that
 * matched eleven thousand photographs — where the answer, if it is not near the
 * top, was not what the ranking was for.
 */
const WALK_LIMIT = 1000;

export interface SearchState extends TimelineState {
  /**
   * What the server thought the question was, or null before the first page
   * lands. What the chips are drawn from.
   */
  query: ParsedQuery | null;
  /** What this search could not do, in a sentence. Empty when nothing was lost. */
  degraded: string;
  /** One result with its evidence, or undefined while it is still being fetched. */
  resultAt: (index: number) => SearchResult | undefined;
  /**
   * Positions in this ranking, as the ids the server understands.
   *
   * Every other grid in the app names a selection by position, because the day
   * table gives every photograph in a collection a place before any of them are
   * downloaded and "everything below here" is then one interval rather than
   * forty thousand identifiers. A ranking has no such table: its order is
   * computed from the query, exists on no row, and would be a different order
   * by the time the request arrived. So a selection made here is spelled out,
   * which costs the pages it covers and is exact.
   */
  idsIn: (ranges: readonly ItemRange[]) => Promise<string[]>;
}

/**
 * A ranked answer, shaped like a timeline so the gallery's grid can draw it.
 *
 * The same contract as useTimeline and deliberately so — days, total, `at`,
 * `request`, `patch` — because ML_IMAGES.md §8 asks for the existing tiles
 * without the day headings, and a second grid to keep in step with the first is
 * the thing worth not building. The day table is one run with no date, which
 * lib/layout.headless already draws as a flat wall of tiles.
 *
 * Three things are genuinely different underneath, and each is a property of
 * ranking rather than an omission:
 *
 *   - **Paged by offset, never by cursor.** A cursor is the sort key of the last
 *     row, and this order's key is a fused rank computed from the query. See
 *     db.Search.
 *   - **The first page settles the question.** It comes back with the server's
 *     reading of the sentence, and every page after it is asked for in that
 *     reading's own terms — `parse=0` and the fields spelled out — so the parser
 *     runs once per search rather than once per page, and page four cannot
 *     disagree with page one about what was being looked for.
 *   - **No resync.** A photograph uploaded mid-search does not shift a ranking
 *     it has no place in, and a caption written under it would change the answer
 *     rather than move it. There is nothing to reconcile with.
 */
export function useSearch(request: URLSearchParams): SearchState {
  const [total, setTotal] = useState(0);
  const [ready, setReady] = useState(false);
  const [refreshing, setRefreshing] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [query, setQuery] = useState<ParsedQuery | null>(null);
  const [degraded, setDegraded] = useState("");
  // Bumped whenever a page lands; the results themselves live in a ref. See
  // useTimeline for why announcing a hundred new rows by copying the array is
  // the most expensive thing a hook like this can do.
  const [version, bump] = useState(0);

  const key = request.toString();
  const wanted = useRef(request);
  wanted.current = request;

  const items = useRef<(SearchResult | undefined)[]>([]);
  const byID = useRef(new Map<string, number>());
  const done = useRef(new Set<number>());
  const inFlight = useRef(new Map<number, AbortController>());
  const waiting = useRef(new Map<number, Promise<void>>());
  const want = useRef(new Map<string, ItemRange>([["grid", { start: 0, end: 1 }]]));
  const count = useRef(0);

  /**
   * The parameters every page after the first is asked for in: the server's own
   * reading, written back out. Null until the first page has landed, which is
   * what holds the rest of them until there is a reading to share.
   */
  const settled = useRef<URLSearchParams | null>(null);
  const loading = useRef<AbortController | null>(null);

  const clear = useCallback(() => {
    for (const controller of inFlight.current.values()) controller.abort();
    inFlight.current.clear();
    waiting.current.clear();
    done.current.clear();
    byID.current.clear();
    items.current = [];
    settled.current = null;
    count.current = 0;
  }, []);

  /**
   * Writes a page into its slots, one photograph to one slot.
   *
   * A page is a separate execution of the ranking — a fresh embedding, a fresh
   * approximate walk of the vector index, over an archive the vision worker may
   * have written to since — so two pages can disagree by a place about where the
   * row on the boundary between them sits, and a photograph that drifts across
   * one arrives twice. db.Search is where that is kept rare; this is where it is
   * kept harmless, because a photograph in two slots at once is not a cosmetic
   * problem: the grid gives every tile its id for a key, a selection is spelled
   * out by reading these slots, and the second copy is standing in the place of
   * the row that drifted the other way.
   *
   * The earlier position wins, whichever page lands first. It is the better of
   * the two answers — nearer the top of a ranking that was read from the top —
   * and it is the one somebody may already be looking at. The slot it leaves
   * empty stays a square: the ranking cannot say what belongs there, and it is
   * one square in a page of a hundred.
   */
  const accept = useCallback((page: number, fetched: SearchResult[]) => {
    const start = page * PAGE_SIZE;
    for (let n = 0; n < fetched.length; n++) {
      const at = start + n;
      const result = fetched[n];
      const held = byID.current.get(result.id);
      if (held !== undefined && held !== at) {
        if (held < at) continue;
        items.current[held] = undefined;
      }
      items.current[at] = result;
      byID.current.set(result.id, at);
    }
    done.current.add(page);
  }, []);

  /**
   * Fetches one page, or hands back the fetch already running for it.
   *
   * Shared because two very different callers want the same page and neither
   * can see the other: the grid, which asks for what is under the viewport, and
   * `idsIn`, which walks a selection that may cover ground nobody has looked
   * at. One promise per page is what keeps a right-click on a long selection
   * from re-requesting what is already arriving.
   */
  const fetchPage = useCallback(
    (page: number): Promise<void> => {
      const held = waiting.current.get(page);
      if (held) return held;
      if (done.current.has(page)) return Promise.resolve();

      const params = page === 0 ? wanted.current : settled.current;
      // Nothing to ask with yet. The first page is what produces the reading
      // every other page is asked in, so this is a page that will be asked for
      // again the moment it lands. See `pump`.
      if (!params) return Promise.resolve();

      const controller = new AbortController();
      inFlight.current.set(page, controller);

      const run = fetchSearch(params, PAGE_SIZE, page * PAGE_SIZE, controller.signal)
        .then((answer) => {
          if (inFlight.current.get(page) !== controller) return;
          inFlight.current.delete(page);
          waiting.current.delete(page);

          if (page === 0) {
            // The reading, and the terms every later page is asked in.
            settled.current = explicitParams(answer.query);
            setQuery(answer.query);
            setDegraded(answer.degraded ?? "");
            count.current = answer.total;
            items.current = new Array(answer.total);
            setTotal(answer.total);
            setReady(true);
            setRefreshing(false);
          }
          accept(page, answer.items);
          setError(null);
          bump((n) => n + 1);
        })
        .catch((err: unknown) => {
          if (inFlight.current.get(page) !== controller) return;
          inFlight.current.delete(page);
          waiting.current.delete(page);
          if (controller.signal.aborted) return;
          setError(err instanceof Error ? err.message : "could not run the search");
          // A first page that failed is a search with no shape at all, so the
          // grid stops waiting for one. A later page is a stretch of
          // placeholders that stays placeholders, and scrolling away and back
          // asks again.
          if (page === 0) {
            setReady(true);
            setRefreshing(false);
          }
        });

      waiting.current.set(page, run);
      return run;
    },
    [accept],
  );

  /** Fills the pages covering what anyone is looking at, nearest the middle first. */
  const pump = useCallback(() => {
    if (!ready && !inFlight.current.has(0) && !done.current.has(0)) {
      void fetchPage(0);
      return;
    }
    if (count.current === 0) return;

    const distance = new Map<number, number>();
    for (const range of want.current.values()) {
      const from = Math.max(0, Math.min(range.start, count.current - 1));
      const to = Math.max(from + 1, Math.min(range.end, count.current));
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

    // A page nobody is looking at any more is worth less than the slot it is
    // holding, and dropping it costs nothing: nothing has been written yet.
    for (const [page, controller] of inFlight.current) {
      if (page !== 0 && !distance.has(page)) {
        controller.abort();
        inFlight.current.delete(page);
        waiting.current.delete(page);
      }
    }

    const queue = [...distance.keys()].filter(
      (page) => !done.current.has(page) && !inFlight.current.has(page),
    );
    queue.sort((a, b) => distance.get(a)! - distance.get(b)!);

    for (const page of queue) {
      if (inFlight.current.size >= MAX_IN_FLIGHT) break;
      void fetchPage(page).then(pump);
    }
  }, [fetchPage, ready]);

  const load = useCallback(() => {
    const controller = new AbortController();
    loading.current?.abort();
    loading.current = controller;
    clear();
    setError(null);
    setQuery(null);
    setDegraded("");
    setTotal(0);
    setReady(false);
    setRefreshing(true);
    bump((n) => n + 1);

    // A search box with nothing in it is not a failed search. It answers
    // immediately, with nothing, and asks the server nothing at all.
    if (!asks(wanted.current)) {
      setReady(true);
      setRefreshing(false);
      return;
    }
    void fetchPage(0).then(pump);
  }, [clear, fetchPage, pump]);

  // `key` rather than the request object, which is a fresh URLSearchParams on
  // every render of the page above: a request spelled twice is one search, and
  // refetching it would cost the whole ranking — a parse, an embedding and a
  // fused scan — for nothing. `load` is deliberately not a dependency for the
  // same reason; it closes over refs and reads the request through one.
  const reload = useRef(load);
  reload.current = load;
  useEffect(() => reload.current(), [key]);
  useEffect(() => clear, [clear]);

  const request_ = useCallback(
    (name: string, start: number, end: number) => {
      const held = want.current.get(name);
      if (end <= start) {
        if (!held) return;
        want.current.delete(name);
      } else {
        if (held && held.start === start && held.end === end) return;
        want.current.set(name, { start, end });
      }
      pump();
    },
    [pump],
  );

  /**
   * Walks the ranking until a stretch of it is in hand.
   *
   * Sequential rather than parallel, and bounded: the callers are a link and a
   * selection, both of which are one gesture rather than a scroll, and neither
   * is worth flooding the GPU service with re-embeddings of the same phrase.
   */
  const walk = useCallback(
    async (start: number, end: number): Promise<void> => {
      if (!settled.current) await fetchPage(0);
      const limit = Math.min(end, count.current);
      for (let page = Math.floor(start / PAGE_SIZE); page * PAGE_SIZE < limit; page++) {
        if (done.current.has(page)) continue;
        await fetchPage(page);
      }
    },
    [fetchPage],
  );

  const idsIn = useCallback(
    async (ranges: readonly ItemRange[]): Promise<string[]> => {
      for (const range of ranges) await walk(range.start, range.end);
      const ids: string[] = [];
      for (const range of ranges) {
        for (let i = range.start; i < Math.min(range.end, items.current.length); i++) {
          const held = items.current[i];
          if (held) ids.push(held.id);
        }
      }
      return ids;
    },
    [walk],
  );

  const at = useCallback((index: number): TimelineItem | undefined => items.current[index], []);
  const resultAt = useCallback((index: number) => items.current[index], []);
  const indexOf = useCallback((id: string) => byID.current.get(id) ?? -1, []);

  const locate = useCallback(
    async (id: string): Promise<number> => {
      const held = byID.current.get(id);
      if (held !== undefined) return held;
      // A link into a ranking is the one thing here with no cheap answer: the
      // server can say where a photograph sits in a timeline because a timeline
      // has an order that exists without asking, and a ranking does not. So it
      // is walked, as far as it is worth walking. See WALK_LIMIT.
      await walk(0, Math.min(count.current, WALK_LIMIT));
      return byID.current.get(id) ?? -1;
    },
    [walk],
  );

  const patch = useCallback((updated: TimelineItem[]) => {
    let changed = false;
    for (const fresh of updated) {
      const index = byID.current.get(fresh.id);
      if (index === undefined) continue;
      const held = items.current[index];
      if (!held || held.state === fresh.state) continue;
      items.current[index] = { ...held, ...fresh };
      changed = true;
    }
    if (changed) bump((n) => n + 1);
  }, []);

  // One run, no date, every tile under it — which lib/layout.headless draws as
  // a flat wall with no headings and no room reserved for them. Relevance is
  // the answer to the question that was asked, and chronology is not.
  const days = useMemo<Day[]>(
    () => (total > 0 ? [{ id: "ranked#0", key: "", label: "", start: 0, count: total }] : []),
    [total],
  );

  return useMemo<SearchState>(
    () => ({
      days,
      total,
      ready,
      loading: refreshing,
      error,
      retry: load,
      at,
      indexOf,
      locate,
      request: request_,
      patch,
      query,
      degraded,
      resultAt,
      idsIn,
    }),
    [
      days,
      total,
      ready,
      refreshing,
      error,
      load,
      at,
      indexOf,
      locate,
      request_,
      patch,
      query,
      degraded,
      resultAt,
      idsIn,
      version,
    ],
  );
}

/**
 * What a selection of search results can have done to it.
 *
 * The library's own actions, with one thing inserted in front of every one of
 * them: the positions are spelled out into ids before they leave. A range is
 * meaningless off this page — index 2 of "phoenix at the beach" is index 2 of
 * nothing the server can reconstruct — so sending one would name whichever two
 * photographs happen to sit at the top of the library instead. See
 * SearchState.idsIn.
 */
export function useSearchActions(search: SearchState): SelectionActions {
  const { idsIn, retry } = search;
  const base = useTrashActions(undefined, retry);

  return useMemo<SelectionActions>(() => {
    const resolve = async (target: Target): Promise<Target> => {
      if (!target.ranges?.length) return { ids: target.ids ?? [] };
      return { ids: await idsIn(target.ranges) };
    };

    return {
      ...base,
      // No filter and no view travel with these, and the absence is the point:
      // both exist to make a *position* mean something, and by the time any of
      // this reaches the server there are no positions left in it.
      filter: undefined,
      view: undefined,
      resolve,
      remove: async (target, noun) => base.remove(await resolve(target), noun),
      hide: async (bucket, target, noun) => base.hide(bucket, await resolve(target), noun),
      restore: async (target, noun) => base.restore(await resolve(target), noun),
      purge: async (target, noun) => base.purge(await resolve(target), noun),
      file: async (album, target, noun) => base.file(album, await resolve(target), noun),
      unfile: async (album, target, noun) => base.unfile(album, await resolve(target), noun),
    };
  }, [base, idsIn]);
}
