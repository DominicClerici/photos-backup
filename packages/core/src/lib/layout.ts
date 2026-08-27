// The timeline's geometry, kept pure so it can be tested without a DOM.
//
// The grid is virtualized against the flat item list rather than against a row
// model, because zoom has to blend two row shapes at once. At any moment the
// grid is a mix of the layout at one zoom level and the layout at the next, and
// every tile's position is a plain lerp between where it sits in each. Levels
// disagree about how many columns there are, so an item's row and column change
// under it — interpolating the *result* rather than the inputs is what keeps
// tiles sliding to their new places instead of jumping there.
//
// Each level is stored as one number per day (where that day's heading starts),
// which makes any tile's position a couple of multiply-adds and keeps the total
// scroll height exact from the first frame: the scrollbar never jumps as pages
// load, and any scroll position resolves to an item with a binary search.
//
// "From the first frame" is meant literally. The day model is built from the
// server's day table — every heading in the collection and how many tiles hang
// under it — rather than from the items that happen to have arrived, so the
// grid is laid out at full size before a single photograph has been fetched.
// Everything below therefore describes positions the timeline *has*, whether or
// not anything is drawn in them yet, and an index into it is a stable address
// for a photo the client may not be holding. That is what lets a placeholder
// and the tile that replaces it occupy exactly the same square, and what a
// selection spanning off-screen photos will eventually be expressed in.

import type { DayRun } from "../wire/api.ts";

export const DEFAULT_GAP = 4;
export const DEFAULT_HEADER_HEIGHT = 52;

/**
 * The cell sizes the grid settles on, smallest first, in CSS pixels.
 *
 * A level is a ceiling rather than an exact size: cells still shrink below it so
 * that a row divides the container evenly, the same way the grid has always
 * responded to window width.
 */
export const ZOOM_LEVELS = [64, 96, 160, 208, 256, 384, 512];
export const MAX_ZOOM = ZOOM_LEVELS.length - 1;
/** 160px — the nearest level to what the grid used before zoom existed. */
export const DEFAULT_ZOOM = 2;

/**
 * The square sizes photod stores a thumbnail and a Live Photo's motion at,
 * smallest first. Mirrors `derivstore.ThumbSizes` on the server.
 */
export const THUMB_SIZES = [96, 256, 512] as const;
export type ThumbSize = (typeof THUMB_SIZES)[number];
/** The size served by the unsized route, and what everything falls back to. */
export const BASE_THUMB_SIZE: ThumbSize = 256;

/**
 * The stored size a zoom level draws from: the smallest one that can fill the
 * cell without being stretched.
 *
 * Which works out as the sizes the pipeline was built for — 96 for the two
 * smallest levels, 256 for the three in the middle, 512 for the two largest —
 * but says why rather than listing it, so adding a level or a size cannot leave
 * the two tables disagreeing. A level is a ceiling on the cell, so a size that
 * covers the ceiling covers the cell.
 *
 * Downscaling is free and upscaling is not, which is why this rounds up. The
 * asymmetry is the whole reason for the extra sizes: at 64px the old 256px file
 * was fifteen times the pixels the screen would use, and at 512px it was the one
 * place the grid visibly softened.
 */
export function thumbSizeFor(level: number): ThumbSize {
  const cell = ZOOM_LEVELS[clamp(Math.round(level), 0, MAX_ZOOM)];
  return THUMB_SIZES.find((size) => size >= cell) ?? THUMB_SIZES[THUMB_SIZES.length - 1];
}

/**
 * The sizes to try for one rendition, best first, when the wanted one may not be
 * stored: what was asked for, then everything else in the order it would be
 * settled for.
 *
 * A library ingested before a size existed has only the base one until a backfill
 * runs, so a 404 is a gap in what is stored rather than a missing asset, and the
 * next-best file is a better answer than nothing. Larger before smaller, for the
 * reason thumbSizeFor rounds up: downscaling costs nothing and upscaling shows.
 */
export function thumbSizeFallbacks(size: ThumbSize): ThumbSize[] {
  const rest = THUMB_SIZES.filter((other) => other !== size);
  return [
    size,
    ...rest.filter((other) => other > size),
    ...rest.filter((other) => other < size).reverse(),
  ];
}

export interface GridMetrics {
  columns: number;
  cellSize: number;
  gap: number;
  headerHeight: number;
}

/** A run of consecutive items sharing a calendar day, as indices into the list. */
export interface Day {
  /**
   * Identity of the run, unique across the list.
   *
   * The date is not unique on its own. Items are ordered by instant but filed
   * under their own local day, so a photo taken across a timezone boundary can
   * put a date either side of another one and leave that date split into two
   * runs — which as a React key means two siblings claiming to be the same
   * node. The start index disambiguates them.
   */
  id: string;
  /** The YYYY-MM-DD this run falls under; shared by every item in it. */
  key: string;
  label: string;
  start: number;
  count: number;
}

export interface LevelLayout {
  metrics: GridMetrics;
  /** `tops[d]` is where day `d`'s heading starts; the extra last entry is the full height. */
  tops: number[];
}

/** The grid frozen at one continuous zoom position, between two levels. */
export interface Frame {
  a: LevelLayout;
  b: LevelLayout;
  /** How far from `a` to `b`, 0–1. */
  f: number;
  cellSize: number;
  gap: number;
  headerHeight: number;
  totalHeight: number;
}

export interface Rect {
  x: number;
  y: number;
  size: number;
}

export interface ItemRange {
  /** Inclusive. */
  start: number;
  /** Exclusive. */
  end: number;
}

// Exact at both ends, so a settled grid draws tiles at precisely their cell size
// rather than a rounding error away from it — the difference between `scale(1)`,
// which costs nothing, and `scale(0.9999999)`, which resamples every thumbnail.
function lerp(a: number, b: number, t: number): number {
  return t === 0 ? a : t === 1 ? b : a + (b - a) * t;
}

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v;
}

export function metricsFor(
  width: number,
  maxCell: number = ZOOM_LEVELS[DEFAULT_ZOOM],
  opts: { gap?: number; headerHeight?: number } = {},
): GridMetrics {
  const gap = opts.gap ?? DEFAULT_GAP;
  const headerHeight = opts.headerHeight ?? DEFAULT_HEADER_HEIGHT;

  const usable = Math.max(width, 1);
  // Ceiling, so `maxCell` is a promise: one column fewer would push cells past
  // the size their stored thumbnail can fill sharply.
  const columns = Math.max(1, Math.ceil((usable + gap) / (maxCell + gap)));
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
 * Turns the server's day table into the grid's day model.
 *
 * The only thing added is `start`, the running total — which is what turns a
 * table of counts into an address space. Every item in the collection has an
 * index in it before any of them are loaded, so a heading, a placeholder and
 * the photograph that eventually fills it all agree on where they belong.
 *
 * Runs arrive in timeline order and are never re-sorted. A date can appear more
 * than once and the dates need not descend: items are ordered by instant and
 * filed under their own local day, so a photo taken either side of a timezone
 * hop can put a date on both sides of another one. The table describes the
 * timeline's shape rather than tidying it.
 *
 * A run with no date is a run with no heading, and is left unlabelled rather
 * than given a date it does not have. See headless.
 */
export function daysFrom(runs: DayRun[]): Day[] {
  const days: Day[] = new Array(runs.length);
  let start = 0;
  for (let i = 0; i < runs.length; i++) {
    days[i] = {
      id: `${runs[i].day}#${start}`,
      key: runs[i].day,
      label: runs[i].day ? dayLabelOf(runs[i].day) : "",
      start,
      count: runs[i].count,
    };
    start += runs[i].count;
  }
  return days;
}

/**
 * Whether this timeline has no days in it: one run, no date, every tile under
 * it.
 *
 * Which is what an order other than newest-or-oldest produces. The dates are
 * still in there — every photograph has one — but they fall in an order that
 * has nothing to do with the calendar, so a heading per tile would be a ruin of
 * that shape rather than a description of it. The grid answers by drawing no
 * headings and reserving no room for them, which is a flat wall of tiles.
 */
export function headless(days: Day[]): boolean {
  return days.length > 0 && days[0].key === "";
}

/** How many items the day model accounts for. */
export function countOf(days: Day[]): number {
  if (days.length === 0) return 0;
  const last = days[days.length - 1];
  return last.start + last.count;
}

/** Stacks the days into one column of headings and tile rows at a fixed cell size. */
export function layoutLevel(days: Day[], metrics: GridMetrics): LevelLayout {
  const tops = new Array<number>(days.length + 1);
  const rowHeight = metrics.cellSize + metrics.gap;

  let y = 0;
  for (let d = 0; d < days.length; d++) {
    tops[d] = y;
    y += metrics.headerHeight + Math.ceil(days[d].count / metrics.columns) * rowHeight;
  }
  tops[days.length] = y;

  return { metrics, tops };
}

/**
 * The grid at a continuous zoom position, where integers are the settled levels.
 *
 * Positions are continuous across a level boundary — blending a→b at f=1 and
 * b→c at f=0 both describe exactly layout b — so an animation can cross as many
 * levels as it likes without a seam.
 */
export function frameAt(levels: LevelLayout[], z: number): Frame {
  const at = clamp(z, 0, levels.length - 1);
  const i = Math.min(Math.floor(at), levels.length - 2);
  const a = levels[i];
  const b = levels[i + 1];
  const f = at - i;
  const last = a.tops.length - 1;

  return {
    a,
    b,
    f,
    cellSize: lerp(a.metrics.cellSize, b.metrics.cellSize, f),
    gap: lerp(a.metrics.gap, b.metrics.gap, f),
    headerHeight: lerp(a.metrics.headerHeight, b.metrics.headerHeight, f),
    totalHeight: lerp(a.tops[last], b.tops[last], f),
  };
}

/**
 * Where one tile sits at a single settled level, before any blending.
 *
 * Exported because it is the whole of what a caller needs to place a tile
 * without holding the day model: the seven rects a tile can occupy are seven
 * calls to this, and from then on its position is a lerp between two of them
 * and nothing else. The browser has no use for that — it recomputes from the
 * frame on every pass of its own loop — but the phone runs the zoom on a
 * separate thread from React, where a table of numbers crosses cheaply and a
 * day model of ten thousand entries does not. See expo-app/src/grid/geometry.
 */
export function tileRectAt(layout: LevelLayout, day: number, offset: number): Rect {
  const m = layout.metrics;
  const step = m.cellSize + m.gap;
  const row = Math.floor(offset / m.columns);
  const col = offset - row * m.columns;
  return {
    x: col * step,
    y: layout.tops[day] + m.headerHeight + row * step,
    size: m.cellSize,
  };
}

/** Where one tile sits, given its day and its position inside that day. */
export function tileRect(frame: Frame, day: number, offset: number): Rect {
  const a = tileRectAt(frame.a, day, offset);
  const b = tileRectAt(frame.b, day, offset);
  return {
    x: lerp(a.x, b.x, frame.f),
    y: lerp(a.y, b.y, frame.f),
    size: lerp(a.size, b.size, frame.f),
  };
}

export function headerY(frame: Frame, day: number): number {
  return lerp(frame.a.tops[day], frame.b.tops[day], frame.f);
}

/** The day an item belongs to. */
export function dayIndexOf(days: Day[], index: number): number {
  let lo = 0;
  let hi = days.length - 1;
  let found = 0;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (days[mid].start <= index) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found;
}

export function itemTop(days: Day[], frame: Frame, index: number): number {
  const d = dayIndexOf(days, index);
  return tileRect(frame, d, index - days[d].start).y;
}

/**
 * The items worth rendering for a scroll position.
 *
 * Tile tops only ever increase with index, in both bracketing layouts and so in
 * the blend of them, which is what makes the two binary searches valid at any
 * point in a transition. Overscan is in pixels because a "row" is a different
 * height at every zoom level.
 */
export function visibleItems(
  days: Day[],
  count: number,
  frame: Frame,
  scrollTop: number,
  viewportHeight: number,
  overscanPx = 0,
): ItemRange {
  if (count === 0 || days.length === 0) return { start: 0, end: 0 };

  const top = scrollTop - overscanPx;
  const bottom = scrollTop + viewportHeight + overscanPx;

  let lo = 0;
  let hi = count;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (itemTop(days, frame, mid) + frame.cellSize > top) hi = mid;
    else lo = mid + 1;
  }
  const start = lo;

  hi = count;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (itemTop(days, frame, mid) < bottom) lo = mid + 1;
    else hi = mid;
  }
  return { start, end: Math.max(lo, start) };
}

/**
 * The item under a point in grid coordinates, used to pin a spot while zooming.
 *
 * The column matters even though only the vertical axis scrolls: two tiles in
 * the same row at one zoom level land in different rows at the next, so which
 * one the pointer is over decides what stays put.
 */
export function itemAtPoint(
  days: Day[],
  count: number,
  frame: Frame,
  x: number,
  y: number,
): number {
  if (count === 0 || days.length === 0) return 0;

  // Items sharing a row share a top, so the first item whose bottom edge is
  // past `y` is the leftmost tile of the row the point falls in.
  let lo = 0;
  let hi = count;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (itemTop(days, frame, mid) + frame.cellSize > y) hi = mid;
    else lo = mid + 1;
  }
  const first = Math.min(lo, count - 1);

  const day = days[dayIndexOf(days, first)];
  const columns = frame.f < 0.5 ? frame.a.metrics.columns : frame.b.metrics.columns;
  const col = clamp(Math.floor(x / (frame.cellSize + frame.gap)), 0, columns - 1);
  return Math.min(first + col, day.start + day.count - 1);
}

/** The day heading that belongs pinned at the top for a scroll position. */
export function dayAt(
  days: Day[],
  frame: Frame,
  scrollTop: number,
): { label: string; top: number } | null {
  if (days.length === 0) return null;

  let lo = 0;
  let hi = days.length - 1;
  let found = 0;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (headerY(frame, mid) <= scrollTop) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return { label: days[found].label, top: headerY(frame, found) };
}
