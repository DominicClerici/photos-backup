import { tileRectAt, type LevelLayout, type Rect } from '@photobackup/core';

/**
 * The grid's geometry, in a shape that crosses to the UI thread.
 *
 * The browser runs its zoom in a requestAnimationFrame loop that reads
 * `frameAt` and `tileRect` directly and writes DOM transforms — one thread, so
 * the day model is simply *there*. A phone has two, and the animation belongs
 * on the one React is not on: a pinch that re-renders a screenful of tiles
 * sixty times a second is a pinch that stutters.
 *
 * Which makes the day model the problem. `tileRect(frame, day, offset)` needs
 * `frame`, and a frame carries two `LevelLayout`s whose `tops` arrays hold one
 * number per heading in the collection — thirty thousand numbers for a decade
 * of photographs, captured into every tile's worklet and copied across on every
 * render. So the model is not sent at all. It is evaluated once per render, on
 * the JS thread, into the seven places a thing can be, and what crosses is that
 * table: twenty-one numbers for a tile, seven for a heading, seven for the
 * whole timeline's height.
 *
 * From there a position at any continuous zoom is a lerp between two entries
 * and nothing else, which is what the functions below do and the whole of what
 * runs per frame. The arithmetic is the same arithmetic `layout.ts` states —
 * `tileRectAt` is exported from core precisely so these tables are built by it
 * rather than beside it — and every one of these is exact at both ends, so a
 * settled grid draws at precisely its cell size rather than a rounding error
 * away from it.
 */

/** How many numbers one level contributes to a tile's table: x, y, size. */
const STRIDE = 3;

/**
 * One number per zoom level, in level order. What every table here is.
 *
 * A plain array rather than a shared value: it is captured by the worklets that
 * read it, so it crosses when the component that built it re-renders — which is
 * when the day table, the width or the mounted range changed, and never during
 * a gesture.
 */
export type Series = readonly number[];

/** The seven places one tile can occupy, flattened. See rectAt. */
export type Places = readonly number[];

/** The seven places one tile occupies, one per settled level. */
export function placesFor(levels: LevelLayout[], day: number, offset: number): number[] {
  const out = new Array<number>(levels.length * STRIDE);
  for (let i = 0; i < levels.length; i++) {
    const r = tileRectAt(levels[i], day, offset);
    out[i * STRIDE] = r.x;
    out[i * STRIDE + 1] = r.y;
    out[i * STRIDE + 2] = r.size;
  }
  return out;
}

/** Where one heading starts, at each level. */
export function topsFor(levels: LevelLayout[], day: number): number[] {
  return levels.map((level) => level.tops[day]);
}

/** The whole timeline's height, at each level. */
export function heightsOf(levels: LevelLayout[]): number[] {
  return levels.map((level) => level.tops[level.tops.length - 1]);
}

/** The cell size each level settles on — the actual one, after the width divides. */
export function cellsOf(levels: LevelLayout[]): number[] {
  return levels.map((level) => level.metrics.cellSize);
}

export function headerHeightsOf(levels: LevelLayout[]): number[] {
  return levels.map((level) => level.metrics.headerHeight);
}

export function clamp(v: number, lo: number, hi: number): number {
  'worklet';
  return v < lo ? lo : v > hi ? hi : v;
}

/**
 * Which two levels a continuous position falls between.
 *
 * The same bracketing `frameAt` does, and for the same reason it clamps the way
 * it does: blending a→b at f=1 and b→c at f=0 both describe exactly layout b,
 * so a gesture can cross as many levels as it likes without a seam.
 */
function bracket(count: number, z: number): { i: number; f: number } {
  'worklet';
  const at = clamp(z, 0, count - 1);
  const i = Math.min(Math.floor(at), count - 2);
  return { i, f: at - i };
}

// Exact at both ends, for the reason lib/layout gives: the difference between
// scale(1), which costs nothing, and scale(0.9999999), which resamples every
// thumbnail on screen.
function mix(a: number, b: number, t: number): number {
  'worklet';
  return t === 0 ? a : t === 1 ? b : a + (b - a) * t;
}

/** A per-level series read at a continuous zoom position. */
export function valueAt(series: Series, z: number): number {
  'worklet';
  if (series.length === 0) return 0;
  if (series.length === 1) return series[0];
  const { i, f } = bracket(series.length, z);
  return mix(series[i], series[i + 1], f);
}

/** A tile's place at a continuous zoom position. */
export function rectAt(places: Places, z: number): Rect {
  'worklet';
  const levels = places.length / STRIDE;
  if (levels < 2) return { x: places[0] ?? 0, y: places[1] ?? 0, size: places[2] ?? 0 };
  const { i, f } = bracket(levels, z);
  const a = i * STRIDE;
  const b = a + STRIDE;
  return {
    x: mix(places[a], places[b], f),
    y: mix(places[a + 1], places[b + 1], f),
    size: mix(places[a + 2], places[b + 2], f),
  };
}

/**
 * The zoom position whose cells are a given size — the inverse of `valueAt`
 * over the cell-size series.
 *
 * This is what makes a pinch physical rather than a rate. The gesture reports a
 * scale, the cells the fingers started on had a size, and the product of the
 * two is the size the fingers are now asking for; this says where on the scale
 * that is. The grid tracks the hand instead of drifting behind it, and letting
 * go and starting again picks up exactly where it left off.
 *
 * The cell sizes rise with the level and are not evenly spaced — 64, 96, 160,
 * 208, 256, 384, 512 before the width divides them — so this walks the series
 * rather than assuming a ratio. Seven entries, once per frame.
 */
export function zoomForCell(cells: Series, size: number): number {
  'worklet';
  if (cells.length === 0) return 0;
  if (size <= cells[0]) return 0;
  for (let i = 0; i < cells.length - 1; i++) {
    if (size <= cells[i + 1]) {
      const span = cells[i + 1] - cells[i];
      return span <= 0 ? i : i + (size - cells[i]) / span;
    }
  }
  return cells.length - 1;
}
