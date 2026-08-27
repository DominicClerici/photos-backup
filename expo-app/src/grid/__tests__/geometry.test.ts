import {
  daysFrom,
  frameAt,
  headerY,
  layoutLevel,
  metricsFor,
  tileRect,
  ZOOM_LEVELS,
  type LevelLayout,
} from '@photobackup/core';

import {
  cellsOf,
  headerHeightsOf,
  heightsOf,
  placesFor,
  rectAt,
  topsFor,
  valueAt,
  zoomForCell,
} from '../geometry';

/**
 * The UI thread's copy of the geometry has to be the geometry.
 *
 * Everything in `grid/geometry` exists to say the same thing `layout.ts` says
 * without carrying the day model across to the other thread to say it. That is
 * only worth doing if the two agree exactly, so this compares them: same
 * positions, same heights, same blend, at every level and between them.
 *
 * The comparisons are exact rather than approximate on purpose. Both sides lerp
 * with the same short-circuits at t=0 and t=1, and a settled grid drawing at
 * scale(0.9999999) instead of scale(1) resamples every thumbnail on screen —
 * which is a difference worth a test that would notice it.
 */

const RUNS = [
  { day: '2024-05-03', count: 7 },
  { day: '2024-05-02', count: 41 },
  { day: '2024-05-01', count: 1 },
  { day: '2024-04-28', count: 260 },
];

function build(width = 390): { days: ReturnType<typeof daysFrom>; levels: LevelLayout[] } {
  const days = daysFrom(RUNS);
  return { days, levels: ZOOM_LEVELS.map((cap) => layoutLevel(days, metricsFor(width, cap))) };
}

/** Every settled level, and a spread of positions between them. */
const POSITIONS = [0, 0.25, 0.5, 1, 1.5, 2, 2.5, 3, 3.999, 4, 5, 5.5, 6];

test('a tile is where core says it is, at every zoom position', () => {
  const { days, levels } = build();

  for (let d = 0; d < days.length; d++) {
    for (const offset of [0, 1, 6, 40, 259].filter((o) => o < days[d].count)) {
      const places = placesFor(levels, d, offset);
      for (const z of POSITIONS) {
        expect(rectAt(places, z)).toEqual(tileRect(frameAt(levels, z), d, offset));
      }
    }
  }
});

test('a heading and the whole board are where core says they are', () => {
  const { days, levels } = build();
  const heights = heightsOf(levels);
  const headers = headerHeightsOf(levels);

  for (const z of POSITIONS) {
    const frame = frameAt(levels, z);
    expect(valueAt(heights, z)).toBe(frame.totalHeight);
    expect(valueAt(headers, z)).toBe(frame.headerHeight);
    expect(valueAt(cellsOf(levels), z)).toBe(frame.cellSize);

    // Where a day's label goes, which is the same lerp `headerY` is.
    for (let d = 0; d < days.length; d++) {
      expect(valueAt(topsFor(levels, d), z)).toBe(headerY(frame, d));
    }
  }
});

test('a position outside the scale is the end of it, not past it', () => {
  const { levels } = build();
  const places = placesFor(levels, 1, 3);

  expect(rectAt(places, -2)).toEqual(rectAt(places, 0));
  expect(rectAt(places, 99)).toEqual(rectAt(places, levels.length - 1));
  expect(valueAt(heightsOf(levels), -2)).toBe(valueAt(heightsOf(levels), 0));
});

test('zoomForCell inverts the cell-size scale, so a pinch tracks the fingers', () => {
  const { levels } = build();
  const cells = cellsOf(levels);

  // A pinch that returns to the size it started at returns to a grid drawn at
  // that size. Stated as the cell rather than the level because the scale is
  // not injective — see the plateau test below — and the cell is the thing the
  // fingers are actually asking for.
  for (const z of POSITIONS) {
    const size = valueAt(cells, z);
    expect(valueAt(cells, zoomForCell(cells, size))).toBeCloseTo(size, 9);
  }

  // Asking for cells smaller or larger than the scale holds is held to its ends
  // rather than banking travel that has to be paid back before anything moves.
  expect(zoomForCell(cells, 1)).toBe(0);
  expect(zoomForCell(cells, 99999)).toBe(cells.length - 1);
});

test('the cell sizes never fall, which is what the inverse needs', () => {
  for (const width of [320, 390, 430, 834]) {
    const cells = cellsOf(build(width).levels);
    for (let i = 1; i < cells.length; i++) {
      expect(cells[i]).toBeGreaterThanOrEqual(cells[i - 1]);
    }
  }
});

test('two levels that draw the same grid are one step of the pinch', () => {
  // A phone is narrow enough that adjacent caps land on the same column count:
  // at 390 points the 208 and the 256 level are both two columns, and so are
  // the 384 and the 512. The scale is therefore not injective, and the pinch
  // skips the level it cannot tell apart rather than stalling on it — which is
  // invisible, because the two are the same grid. What is not invisible is that
  // the higher one draws a larger rendition into the same cell, so passing
  // through is what sharpens it.
  const cells = cellsOf(build(390).levels);
  const flats = cells.filter((cell, i) => i > 0 && cell === cells[i - 1]);
  expect(flats.length).toBeGreaterThan(0);

  // Monotone regardless: a larger cell never asks for a smaller level.
  let last = -Infinity;
  for (let size = 20; size < 600; size += 3) {
    const z = zoomForCell(cells, size);
    expect(z).toBeGreaterThanOrEqual(last);
    last = z;
  }
});

test('a timeline with no headings still places its tiles', () => {
  const days = daysFrom([{ day: '', count: 500 }]);
  const levels = ZOOM_LEVELS.map((cap) =>
    layoutLevel(days, metricsFor(390, cap, { headerHeight: 0 })),
  );

  expect(valueAt(headerHeightsOf(levels), 2.5)).toBe(0);
  for (const z of POSITIONS) {
    expect(rectAt(placesFor(levels, 0, 137), z)).toEqual(tileRect(frameAt(levels, z), 0, 137));
  }
});
