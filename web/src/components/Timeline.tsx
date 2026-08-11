"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { fetchStates, type TimelineItem } from "@/lib/api";
import {
  buildRows,
  itemsInRange,
  metricsFor,
  sectionAt,
  visibleRange,
} from "@/lib/layout";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { Tile } from "./Tile";

/** How often unfinished tiles ask whether their derivative landed. */
const POLL_MS = 4000;
/** Start the next page this many viewport-heights from the bottom. */
const PREFETCH_VIEWPORTS = 1.5;

const NOTICE =
  "flex items-center justify-center gap-3 px-3 py-7 text-sm text-muted-foreground";

interface Props {
  items: TimelineItem[];
  loading: boolean;
  hasMore: boolean;
  error: string | null;
  loadMore: () => void;
  patch: (items: TimelineItem[]) => void;
  retry: () => void;
  onOpen: (id: string) => void;
}

export function Timeline({
  items,
  loading,
  hasMore,
  error,
  loadMore,
  patch,
  retry,
  onOpen,
}: Props) {
  const scroller = useRef<HTMLDivElement>(null);
  const [size, setSize] = useState({ width: 0, height: 0 });
  const [scrollTop, setScrollTop] = useState(0);

  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect;
      setSize((prev) =>
        prev.width === width && prev.height === height ? prev : { width, height },
      );
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // Scroll fires far more often than the screen refreshes, and every handler
  // here ends in a re-render, so coalesce to one per frame.
  const frame = useRef(0);
  const onScroll = useCallback(() => {
    if (frame.current) return;
    frame.current = requestAnimationFrame(() => {
      frame.current = 0;
      if (scroller.current) setScrollTop(scroller.current.scrollTop);
    });
  }, []);
  useEffect(() => () => cancelAnimationFrame(frame.current), []);

  const metrics = useMemo(() => metricsFor(size.width), [size.width]);
  const model = useMemo(() => buildRows(items, metrics), [items, metrics]);
  const range = useMemo(
    // Overscanning by a full viewport in each direction means a fast flick
    // lands on tiles that already started decoding.
    () => visibleRange(model.rows, scrollTop, size.height, size.height),
    [model.rows, scrollTop, size.height],
  );

  useEffect(() => {
    if (!hasMore || loading || size.height === 0) return;
    const remaining = model.totalHeight - (scrollTop + size.height);
    if (remaining < size.height * PREFETCH_VIEWPORTS) loadMore();
  }, [hasMore, loading, scrollTop, size.height, model.totalHeight, loadMore]);

  const visible = useMemo(() => itemsInRange(model.rows, range), [model.rows, range]);
  const pendingKey = useMemo(
    () =>
      visible
        .filter((it) => it.state === "pending")
        .map((it) => it.id)
        .join(","),
    [visible],
  );

  // Only tiles actually on screen are polled. During a backfill the library can
  // be tens of thousands of pending items, and asking about all of them every
  // few seconds would cost more than generating the thumbnails.
  useEffect(() => {
    if (!pendingKey) return;
    const ids = pendingKey.split(",");
    const controller = new AbortController();

    const id = setInterval(() => {
      fetchStates(ids, controller.signal)
        .then(patch)
        .catch(() => {
          // A dropped poll is not worth surfacing; the next tick retries.
        });
    }, POLL_MS);

    return () => {
      controller.abort();
      clearInterval(id);
    };
  }, [pendingKey, patch]);

  const section = sectionAt(model.sections, scrollTop);
  // Show the floating date only once its own heading has scrolled away, so it
  // never sits directly above a heading saying the same thing.
  const showFloating = section != null && scrollTop > section.top + metrics.headerHeight;

  return (
    <div className="relative min-h-0 flex-1">
      {showFloating ? (
        <div className="pointer-events-none absolute top-2.5 left-[22px] z-[5] rounded-full border bg-card/[0.88] px-3 py-[5px] text-xs font-semibold backdrop-blur-[8px]">
          {section.label}
        </div>
      ) : null}

      <div
        className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-3 pb-16"
        ref={scroller}
        onScroll={onScroll}
      >
        <div className="relative w-full" style={{ height: model.totalHeight }}>
          {model.rows.slice(range.start, range.end).map((row) =>
            row.kind === "header" ? (
              // Rows are positioned by transform rather than `top`: the browser can
              // skip layout entirely when only a transform changes, which matters
              // when every scroll frame repositions a screenful of them.
              <div
                key={row.key}
                className="absolute top-0 left-0 flex w-full items-end gap-2.5 pb-2.5 will-change-transform"
                style={{ transform: `translateY(${row.top}px)`, height: row.height }}
              >
                <h2 className="text-[15px] font-semibold">{row.label}</h2>
                <span className="text-xs text-faint">{row.count}</span>
              </div>
            ) : (
              <div
                key={row.key}
                className="absolute top-0 left-0 flex w-full will-change-transform"
                style={{
                  transform: `translateY(${row.top}px)`,
                  height: row.height,
                  gap: metrics.gap,
                }}
              >
                {row.items.map((item) => (
                  <Tile
                    key={item.id}
                    item={item}
                    size={metrics.cellSize}
                    onOpen={onOpen}
                  />
                ))}
              </div>
            ),
          )}
        </div>

        {error ? (
          <div className={cn(NOTICE, "text-destructive")}>
            <span>{error}</span>
            <Button type="button" variant="outline" size="sm" onClick={retry}>
              Retry
            </Button>
          </div>
        ) : null}

        {items.length === 0 && !loading && !error ? (
          <div className={NOTICE}>
            <span>Nothing here yet. Run a backup from the phone.</span>
          </div>
        ) : null}

        {loading ? (
          <div className={cn(NOTICE, "text-[13px] text-faint")}>Loading…</div>
        ) : null}
      </div>
    </div>
  );
}
