// The timeline's geometry, kept pure so it can be tested without a DOM.
//
// The grid is virtualized against a row model rather than against the item list.
// A row is either a date heading or one line of tiles, every row's height is
// known before anything renders, and so the total scroll height is exact from
// the first frame — the scrollbar never jumps as content loads, and any scroll
// position resolves to a row with two binary searches.

import type { TimelineItem } from "./api";

export const DEFAULT_GAP = 4;
export const DEFAULT_HEADER_HEIGHT = 52;
/**
 * Tiles aim for this width and stretch to divide the container evenly. Stored
 * thumbnails are 256px square, so a cell much past ~170 CSS px starts to look
 * soft on a 2x display.
 */
export const DEFAULT_TARGET_CELL = 160;

export interface GridMetrics {
  columns: number;
  cellSize: number;
  gap: number;
  headerHeight: number;
}

export type Row =
  | {
      kind: "header";
      key: string;
      label: string;
      count: number;
      top: number;
      height: number;
    }
  | {
      kind: "tiles";
      key: string;
      items: TimelineItem[];
      top: number;
      height: number;
    };

/** A day's full extent, used to float the current date over the grid. */
export interface Section {
  key: string;
  label: string;
  top: number;
  bottom: number;
}

export interface RowModel {
  rows: Row[];
  sections: Section[];
  totalHeight: number;
}

export interface VisibleRange {
  /** Inclusive. */
  start: number;
  /** Exclusive. */
  end: number;
}

export function metricsFor(
  width: number,
  opts: {
    gap?: number;
    headerHeight?: number;
    targetCell?: number;
  } = {},
): GridMetrics {
  const gap = opts.gap ?? DEFAULT_GAP;
  const headerHeight = opts.headerHeight ?? DEFAULT_HEADER_HEIGHT;
  const target = opts.targetCell ?? DEFAULT_TARGET_CELL;

  const usable = Math.max(width, 1);
  // Rounding rather than flooring: flooring always errs toward fewer, larger
  // cells, which on a wide window pushes tiles well past the size the stored
  // thumbnail can fill sharply.
  const columns = Math.max(1, Math.round((usable + gap) / (target + gap)));
  const cellSize = (usable - gap * (columns - 1)) / columns;
  return { columns, cellSize, gap, headerHeight };
}

/**
 * The calendar day a photo belongs under.
 *
 * When the file recorded its own UTC offset we use it, so a photo taken at
 * 23:50 in Vermont files under that day rather than under the next one for a
 * viewer in Berlin. With no recorded offset there is nothing better to do than
 * use the viewer's timezone.
 */
export function dayKeyOf(takenAt: string, offsetMinutes?: number | null): string {
  const t = new Date(takenAt);
  if (offsetMinutes == null) {
    return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())}`;
  }
  const shifted = new Date(t.getTime() + offsetMinutes * 60_000);
  return `${shifted.getUTCFullYear()}-${pad(shifted.getUTCMonth() + 1)}-${pad(shifted.getUTCDate())}`;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

// buildRows runs again on every page append, so without this a full scroll
// through a large library re-parses every timestamp it has already seen —
// quadratic work for a list that only ever grows at the end. Item objects are
// stable across rebuilds, so keying on identity hits nearly always.
const dayKeyCache = new WeakMap<TimelineItem, string>();

function cachedDayKey(it: TimelineItem): string {
  let key = dayKeyCache.get(it);
  if (key === undefined) {
    key = dayKeyOf(it.taken_at, it.offset_minutes);
    dayKeyCache.set(it, key);
  }
  return key;
}

/** Renders a YYYY-MM-DD key for display, in UTC so the key cannot drift. */
export function dayLabelOf(dayKey: string, now: Date = new Date()): string {
  const [y, m, d] = dayKey.split("-").map(Number);
  const date = new Date(Date.UTC(y, m - 1, d));
  const thisYear = date.getUTCFullYear() === now.getFullYear();
  return date.toLocaleDateString(undefined, {
    timeZone: "UTC",
    weekday: "short",
    month: "long",
    day: "numeric",
    ...(thisYear ? {} : { year: "numeric" }),
  });
}

/**
 * Turns a flat, already-sorted item list into positioned rows.
 *
 * Items must arrive newest-first, which is the order the timeline endpoint
 * returns. Days are detected by scanning for a change in day key rather than by
 * bucketing into a map, so paging in more items only ever appends.
 */
export function buildRows(items: TimelineItem[], m: GridMetrics): RowModel {
  const rows: Row[] = [];
  const sections: Section[] = [];
  const rowHeight = m.cellSize + m.gap;

  let top = 0;
  let index = 0;

  while (index < items.length) {
    const key = cachedDayKey(items[index]);

    let end = index;
    while (end < items.length && cachedDayKey(items[end]) === key) {
      end++;
    }

    const dayItems = items.slice(index, end);
    const sectionTop = top;

    rows.push({
      kind: "header",
      key: `h:${key}`,
      label: dayLabelOf(key),
      count: dayItems.length,
      top,
      height: m.headerHeight,
    });
    top += m.headerHeight;

    for (let i = 0; i < dayItems.length; i += m.columns) {
      rows.push({
        kind: "tiles",
        key: `r:${key}:${i}`,
        items: dayItems.slice(i, i + m.columns),
        top,
        height: rowHeight,
      });
      top += rowHeight;
    }

    sections.push({ key, label: dayLabelOf(key), top: sectionTop, bottom: top });
    index = end;
  }

  return { rows, sections, totalHeight: top };
}

/**
 * The slice of rows worth rendering for a scroll position.
 *
 * Overscan is in pixels rather than rows because rows are two different heights;
 * a fixed row count would overscan unevenly depending on where the headings land.
 */
export function visibleRange(
  rows: Row[],
  scrollTop: number,
  viewportHeight: number,
  overscanPx = 0,
): VisibleRange {
  if (rows.length === 0) return { start: 0, end: 0 };

  const top = scrollTop - overscanPx;
  const bottom = scrollTop + viewportHeight + overscanPx;

  const start = lowerBound(rows, top);
  const end = upperBound(rows, bottom);
  return { start, end: Math.max(end, start) };
}

/** First row whose bottom edge is past `y`. */
function lowerBound(rows: Row[], y: number): number {
  let lo = 0;
  let hi = rows.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (rows[mid].top + rows[mid].height > y) hi = mid;
    else lo = mid + 1;
  }
  return lo;
}

/** One past the last row that begins before `y`. */
function upperBound(rows: Row[], y: number): number {
  let lo = 0;
  let hi = rows.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (rows[mid].top < y) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/** The day heading that belongs pinned at the top for a scroll position. */
export function sectionAt(sections: Section[], scrollTop: number): Section | null {
  if (sections.length === 0) return null;

  let lo = 0;
  let hi = sections.length - 1;
  let found = 0;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (sections[mid].top <= scrollTop) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return sections[found];
}

/** Every item currently rendered, which is what the state poller watches. */
export function itemsInRange(rows: Row[], range: VisibleRange): TimelineItem[] {
  const out: TimelineItem[] = [];
  for (let i = range.start; i < range.end && i < rows.length; i++) {
    const row = rows[i];
    if (row.kind === "tiles") out.push(...row.items);
  }
  return out;
}
