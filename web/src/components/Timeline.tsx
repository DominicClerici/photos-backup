"use client";

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
} from "react";

import { fetchStates, type TimelineItem } from "@/lib/api";
import {
  buildDays,
  dayAt,
  dayIndexOf,
  frameAt,
  headerY,
  itemAtPoint,
  layoutLevel,
  metricsFor,
  thumbSizeFor,
  tileRect,
  visibleItems,
  DEFAULT_ZOOM,
  MAX_ZOOM,
  ZOOM_LEVELS,
  type Day,
  type Frame,
  type ItemRange,
} from "@/lib/layout";
import { Zoom, savedZoom } from "@/lib/zoom";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { Tile } from "./Tile";
import { ZoomSlider } from "./ZoomSlider";

/** How often unfinished tiles ask whether their derivative landed. */
const POLL_MS = 4000;
/** Start the next page this many viewport-heights from the bottom. */
const PREFETCH_VIEWPORTS = 1.5;
/** Rendered above and below the viewport at rest, as a fraction of its height. */
const OVERSCAN = 1;
/** Less while zooming: the far end of the range holds seven times as many tiles. */
const ZOOM_OVERSCAN = 0.35;
/** How far a ctrl-wheel gesture has to travel to buy each further level. */
const WHEEL_PER_LEVEL = 150;
/** No ctrl-wheel event for this long ends the gesture and re-anchors the travel. */
const WHEEL_IDLE_MS = 220;
/**
 * Pixels per line for wheels that report in lines rather than pixels.
 *
 * Firefox sends three lines per detent where Chrome sends 100 pixels, so this
 * is what makes one notch of the same wheel cost the same zoom in both.
 */
const WHEEL_LINE_PX = 33;

const NOTICE =
  "flex items-center justify-center gap-3 px-3 py-7 text-sm text-muted-foreground";

/** A mounted element, tagged with everything the frame loop needs to place it. */
interface Slot {
  el: HTMLElement;
  day: number;
  offset: number;
}

/**
 * The point held still while the grid re-flows underneath it.
 *
 * A tile rather than a scroll offset, because the offset means something
 * different at every zoom level: the whole timeline is nearly three times taller
 * at the smallest cell size than at the largest.
 */
interface Anchor {
  day: number;
  offset: number;
  /** How far down that tile the held point sits, 0–1. */
  frac: number;
  /** Where the held point should stay, measured from the top of the viewport. */
  screenY: number;
}

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
  const board = useRef<HTMLDivElement>(null);
  const pill = useRef<HTMLDivElement>(null);

  const [size, setSize] = useState({ width: 0, height: 0 });
  const [mounted, setMounted] = useState(false);
  const [tick, invalidate] = useReducer((n: number) => n + 1, 0);

  const held = useRef<Zoom>(null);
  held.current ??= new Zoom(savedZoom());
  const zoom = held.current;
  useEffect(() => () => zoom.dispose(), [zoom]);
  useEffect(() => setMounted(true), []);

  const days = useMemo(() => buildDays(items), [items]);
  const levels = useMemo(
    () => ZOOM_LEVELS.map((cap) => layoutLevel(days, metricsFor(size.width, cap))),
    [days, size.width],
  );

  // Everything the per-frame loop reads, refreshed from each render by the
  // layout effect below. During a zoom the loop runs without React: re-rendering
  // a screenful of tiles sixty times a second would cost more than the animation
  // it is trying to draw, so React owns *which* tiles exist and the loop owns
  // where they sit.
  const env = useRef({
    days,
    levels,
    count: items.length,
    height: 0,
    box: 1,
    boxLevel: DEFAULT_ZOOM,
    scrollTop: 0,
    /** The last scroll offset this component wrote, to tell ours from the user's. */
    applied: -1,
    window: null as ItemRange | null,
    anchor: null as Anchor | null,
    tiles: [] as Slot[],
    heads: [] as Slot[],
  });

  // Read during render rather than held in state. The zoom advances every frame
  // and the grid must agree with it on frames React never sees, so the animated
  // value is the single source and both paths read it directly.
  const z = zoom.value;
  const settled = !zoom.moving;
  const frame = frameAt(levels, z);
  const boxLevel = clamp(settled ? Math.round(zoom.target) : env.current.boxLevel, 0, MAX_ZOOM);
  const box = levels[boxLevel].metrics.cellSize;
  // The rendition follows the box for the same reason the box exists: it is the
  // largest size this transition will draw, so zooming in upgrades the picture
  // as soon as the tiles are laid out for it, and zooming out keeps the larger
  // one until the grid has settled at the smaller cell.
  const thumb = thumbSizeFor(boxLevel);

  const needed = visibleItems(
    days,
    items.length,
    frame,
    env.current.scrollTop,
    size.height,
    size.height * (settled ? OVERSCAN : ZOOM_OVERSCAN),
  );
  const range = settled ? needed : (env.current.window ?? needed);

  const updatePill = useCallback(
    (at?: Frame) => {
      const label = pill.current;
      const e = env.current;
      if (!label) return;
      const on = at ?? frameAt(e.levels, zoom.value);
      const day = dayAt(e.days, on, e.scrollTop);
      // Show the floating date only once its own heading has scrolled away, so
      // it never sits directly above a heading saying the same thing.
      const show = day != null && e.scrollTop > day.top + on.headerHeight;
      label.style.opacity = show ? "1" : "0";
      if (day) label.textContent = day.label;
    },
    [zoom],
  );

  /** Pins the tile under a point in the viewport, keeping that point where it is. */
  const anchorAt = useCallback(
    (clientX: number, clientY: number) => {
      const el = scroller.current;
      const canvas = board.current;
      const e = env.current;
      if (!el || !canvas || e.count === 0) {
        e.anchor = null;
        return;
      }
      const rect = canvas.getBoundingClientRect();
      const x = clientX - rect.left;
      const y = clientY - rect.top;

      const at = frameAt(e.levels, zoom.value);
      const index = itemAtPoint(e.days, e.count, at, x, y);
      const day = dayIndexOf(e.days, index);
      const offset = index - e.days[day].start;
      const r = tileRect(at, day, offset);

      e.anchor = {
        day,
        offset,
        frac: clamp((y - r.y) / (r.size || 1), 0, 1),
        screenY: y - el.scrollTop,
      };
      e.applied = el.scrollTop;
    },
    [zoom],
  );

  const anchorCentre = useCallback(() => {
    const el = scroller.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    anchorAt(rect.left + rect.width / 2, rect.top + rect.height / 2);
  }, [anchorAt]);

  /**
   * One frame of zoom: the scroll offset, the spacer, and every tile's place.
   *
   * Only transforms are written. The tiles are already laid out at the largest
   * box they will reach during this transition, so scaling them down is a
   * composite rather than a re-layout and a fresh rasterisation of every
   * thumbnail on screen.
   */
  const applyFrame = useCallback(() => {
    const el = scroller.current;
    const canvas = board.current;
    const e = env.current;
    if (!el || !canvas) return;

    const moving = zoom.moving;
    if (moving && !e.anchor) anchorCentre();

    const at = frameAt(e.levels, zoom.value);
    canvas.style.height = `${at.totalHeight}px`;

    const anchor = e.anchor;
    if (anchor && e.count > 0) {
      const held = tileRect(at, anchor.day, anchor.offset).y + anchor.frac * at.cellSize;
      // Scrolling during a zoom wins: adopt where the user went and re-hang the
      // anchor there rather than dragging them back to it.
      if (e.applied >= 0 && Math.abs(el.scrollTop - e.applied) > 0.5) {
        anchor.screenY = held - el.scrollTop;
      }
      const limit = Math.max(0, at.totalHeight - e.height);
      el.scrollTop = clamp(held - anchor.screenY, 0, limit);
      e.applied = el.scrollTop;
    }
    e.scrollTop = el.scrollTop;

    for (const slot of e.tiles) {
      const r = tileRect(at, slot.day, slot.offset);
      slot.el.style.transform = `translate(${r.x}px,${r.y}px) scale(${r.size / e.box})`;
    }
    for (const slot of e.heads) {
      slot.el.style.transform = `translateY(${headerY(at, slot.day)}px)`;
    }
    updatePill(at);

    // React is only pulled back in when the set of mounted tiles no longer
    // covers the screen, or when the boxes they sit in are about to be too
    // small to scale down from — a handful of renders per transition, not one
    // per frame.
    let stale = false;
    if (moving) {
      const need = visibleItems(
        e.days,
        e.count,
        at,
        e.scrollTop,
        e.height,
        e.height * ZOOM_OVERSCAN,
      );
      const win = e.window;
      if (!win) {
        e.window = need;
        e.boxLevel = boxLevelFor(zoom);
        stale = true;
      } else {
        if (need.start < win.start || need.end > win.end) {
          e.window = {
            start: Math.min(win.start, need.start),
            end: Math.max(win.end, need.end),
          };
          stale = true;
        }
        const want = boxLevelFor(zoom);
        if (want > e.boxLevel) {
          e.boxLevel = want;
          stale = true;
        }
      }
    } else if (e.window) {
      // Settled: hand the grid back at its native cell size, with no scale left
      // on it, so the thumbnails rasterise sharp again.
      e.window = null;
      e.anchor = null;
      stale = true;
    }
    if (stale) invalidate();
  }, [anchorCentre, updatePill, zoom]);

  useEffect(() => zoom.subscribe(applyFrame), [applyFrame, zoom]);

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
  const pending = useRef(0);
  const onScroll = useCallback(() => {
    if (pending.current) return;
    pending.current = requestAnimationFrame(() => {
      pending.current = 0;
      const el = scroller.current;
      if (!el) return;
      env.current.scrollTop = el.scrollTop;
      if (zoom.moving) applyFrame();
      else invalidate();
    });
  }, [applyFrame, zoom]);
  useEffect(() => () => cancelAnimationFrame(pending.current), []);

  // A ctrl-wheel gesture is one continuous travel rather than a series of
  // independent notches: the level is a function of how far the gesture has come
  // from where it began, so reaching a level costs the same 150px whether that
  // took one flick or twenty, and scrolling back the way you came unwinds it
  // exactly. This is what keeps a fast flick from spending its whole length
  // retargeting a transition that has not visibly started yet — the keyboard,
  // which cannot run away like that, still steps one level per press.
  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const gesture = { travel: 0, at: -Infinity, base: 0 };

    const onWheel = (ev: WheelEvent) => {
      if (!ev.ctrlKey && !ev.metaKey) return;
      // Also what stops the browser zooming the whole page underneath us.
      ev.preventDefault();

      if (ev.timeStamp - gesture.at > WHEEL_IDLE_MS) {
        gesture.travel = 0;
        gesture.base = Math.round(zoom.target);
      }
      gesture.at = ev.timeStamp;

      const unit =
        ev.deltaMode === 1 ? WHEEL_LINE_PX : ev.deltaMode === 2 ? el.clientHeight : 1;
      // Scrolling up zooms in, so travel runs against the wheel.
      gesture.travel -= ev.deltaY * unit;
      // Held to the ends of the scale, so pushing on past the largest tiles
      // never banks travel that has to be paid back before anything moves.
      gesture.travel = clamp(
        gesture.travel,
        -gesture.base * WHEEL_PER_LEVEL,
        (MAX_ZOOM - gesture.base) * WHEEL_PER_LEVEL,
      );

      const level = gesture.base + Math.trunc(gesture.travel / WHEEL_PER_LEVEL);
      if (level === Math.round(zoom.target)) return;

      anchorAt(ev.clientX, ev.clientY);
      zoom.to(level);
    };

    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [anchorAt, zoom]);

  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      if (!(ev.ctrlKey || ev.metaKey) || ev.altKey) return;
      const target = ev.target as HTMLElement | null;
      if (target?.isContentEditable) return;
      if (target && /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName)) return;

      let act: (() => void) | null = null;
      if (ev.key === "+" || ev.key === "=") act = () => zoom.step(1);
      else if (ev.key === "-" || ev.key === "_") act = () => zoom.step(-1);
      else if (ev.key === "0") act = () => zoom.to(DEFAULT_ZOOM);
      if (!act) return;

      ev.preventDefault();
      anchorCentre();
      act();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [anchorCentre, zoom]);

  // Refreshed after every render, before the browser paints, so the frame loop
  // never places a tile that has just been unmounted.
  useLayoutEffect(() => {
    const e = env.current;
    e.days = days;
    e.levels = levels;
    e.count = items.length;
    e.height = size.height;
    e.box = box;

    const canvas = board.current;
    if (canvas) {
      const tiles: Slot[] = [];
      const heads: Slot[] = [];
      for (const child of canvas.children) {
        const el = child as HTMLElement;
        const day = Number(el.dataset.day);
        const offset = el.dataset.tile;
        if (offset === undefined) heads.push({ el, day, offset: 0 });
        else tiles.push({ el, day, offset: Number(offset) });
      }
      e.tiles = tiles;
      e.heads = heads;
    }
    updatePill();
  });

  useEffect(() => {
    if (!hasMore || loading || size.height === 0) return;
    const remaining = frame.totalHeight - (env.current.scrollTop + size.height);
    if (remaining < size.height * PREFETCH_VIEWPORTS) loadMore();
  }, [hasMore, loading, size.height, frame.totalHeight, loadMore, tick]);

  const visible = useMemo(
    () => items.slice(range.start, range.end),
    [items, range.start, range.end],
  );

  // A Live Photo's motion is queued separately from its thumbnail, so a tile
  // can be drawn and still be waiting to come to life.
  const pendingKey = useMemo(
    () =>
      visible
        .filter((it) => it.state === "pending" || it.live === "pending")
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

  const headings = headingsFor(days, range);

  return (
    <div className="relative min-h-0 flex-1">
      {/* Left empty for React: the frame loop owns this element's text and
          opacity, and reparenting a node React thinks it manages would leave
          the two writing to different places. */}
      <div
        ref={pill}
        className="pointer-events-none absolute top-2.5 left-[22px] z-[5] rounded-full border bg-card/[0.88] px-3 py-[5px] text-xs font-semibold opacity-0 backdrop-blur-[8px] transition-opacity duration-150"
      />

      <div
        className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-3 pb-16"
        ref={scroller}
        onScroll={onScroll}
      >
        {/* Rows are placed by transform rather than `top`: the browser can skip
            layout entirely when only a transform changes, which is what makes
            both scrolling and zooming cost a composite instead of a reflow. */}
        <div ref={board} className="relative w-full" style={{ height: frame.totalHeight }}>
          {headings.map((d) => (
            <div
              key={days[d].key}
              data-day={d}
              className="absolute top-0 left-0 flex w-full items-end gap-2.5 pb-2.5"
              style={{
                height: frame.headerHeight,
                transform: `translateY(${headerY(frame, d)}px)`,
              }}
            >
              <h2 className="text-[15px] font-semibold">{days[d].label}</h2>
              <span className="text-xs text-faint">{days[d].count}</span>
            </div>
          ))}

          {visible.map((item, i) => {
            const index = range.start + i;
            const day = dayIndexOf(days, index);
            const offset = index - days[day].start;
            const r = tileRect(frame, day, offset);
            return (
              <Tile
                key={item.id}
                item={item}
                size={box}
                thumbSize={thumb}
                day={day}
                offset={offset}
                transform={`translate(${r.x}px,${r.y}px) scale(${r.size / box})`}
                onOpen={onOpen}
              />
            );
          })}
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

      {mounted ? <ZoomSlider zoom={zoom} /> : null}
    </div>
  );
}

/** The largest level the running transition still has to draw. */
function boxLevelFor(zoom: Zoom): number {
  return Math.min(MAX_ZOOM, Math.ceil(Math.max(zoom.value, zoom.target)));
}

/** Every heading that could be on screen: the visible days, plus the next one down. */
function headingsFor(days: Day[], range: ItemRange): number[] {
  if (days.length === 0 || range.end <= range.start) return [];
  const first = dayIndexOf(days, range.start);
  const last = Math.min(dayIndexOf(days, range.end - 1) + 1, days.length - 1);
  const out: number[] = [];
  for (let d = first; d <= last; d++) out.push(d);
  return out;
}

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v;
}
