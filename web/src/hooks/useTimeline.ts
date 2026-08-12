"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { fetchTimeline, type TimelineItem } from "@/lib/api";

const PAGE_SIZE = 200;

export interface TimelineState {
  items: TimelineItem[];
  loading: boolean;
  hasMore: boolean;
  error: string | null;
  loadMore: () => void;
  /** Replaces items by id, for tiles whose derivatives finished after loading. */
  patch: (updated: TimelineItem[]) => void;
  retry: () => void;
}

export function useTimeline(): TimelineState {
  const [items, setItems] = useState<TimelineItem[]>([]);
  const [loading, setLoading] = useState(false);
  const [hasMore, setHasMore] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Paging position lives in refs, not state: loadMore is called from a scroll
  // effect that fires far more often than React re-renders, and reading the
  // cursor from a closure would happily request the same page twice.
  const cursor = useRef<string | undefined>(undefined);
  const inFlight = useRef(false);
  const exhausted = useRef(false);
  // Keyset paging cannot serve the same asset twice on its own, but an import
  // that rewrites a photo's date moves it in the ordering the cursor is walking,
  // and a page fetched after that can hand back something an earlier one
  // already did. Two tiles for one asset would both key on its id, so the ids
  // seen so far decide what a page is allowed to append.
  const seen = useRef(new Set<string>());

  const loadMore = useCallback(() => {
    if (inFlight.current || exhausted.current) return;
    inFlight.current = true;
    setLoading(true);

    fetchTimeline(cursor.current, PAGE_SIZE)
      .then((page) => {
        const fresh = page.items.filter((it) => !seen.current.has(it.id));
        for (const it of fresh) seen.current.add(it.id);
        setItems((prev) => (fresh.length ? [...prev, ...fresh] : prev));
        cursor.current = page.next_cursor;
        if (!page.next_cursor) {
          exhausted.current = true;
          setHasMore(false);
        }
        setError(null);
      })
      .catch((err: unknown) => {
        // Stop paging on failure rather than retrying on every scroll event,
        // which would turn one unreachable server into a request storm.
        exhausted.current = true;
        setHasMore(false);
        setError(err instanceof Error ? err.message : "could not load the timeline");
      })
      .finally(() => {
        inFlight.current = false;
        setLoading(false);
      });
  }, []);

  const retry = useCallback(() => {
    exhausted.current = false;
    setHasMore(true);
    setError(null);
    loadMore();
  }, [loadMore]);

  const patch = useCallback((updated: TimelineItem[]) => {
    if (updated.length === 0) return;
    setItems((prev) => {
      const byID = new Map(updated.map((it) => [it.id, it]));
      let changed = false;
      const next = prev.map((it) => {
        const fresh = byID.get(it.id);
        if (!fresh || fresh.state === it.state) return it;
        changed = true;
        return fresh;
      });
      // Returning prev unchanged keeps a poll that found nothing new from
      // rebuilding the entire row model.
      return changed ? next : prev;
    });
  }, []);

  useEffect(() => {
    loadMore();
  }, [loadMore]);

  return { items, loading, hasMore, error, loadMore, patch, retry };
}
