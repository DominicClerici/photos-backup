// Run with TZ=UTC (see package.json). The day-grouping tests compare the file's
// own timezone against the viewer's, and they only prove anything if the
// viewer's is known.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  buildDays,
  dayAt,
  dayIndexOf,
  dayKeyOf,
  frameAt,
  headerY,
  itemAtPoint,
  itemTop,
  layoutLevel,
  metricsFor,
  thumbSizeFallbacks,
  thumbSizeFor,
  tileRect,
  visibleItems,
  DEFAULT_ZOOM,
  MAX_ZOOM,
  THUMB_SIZES,
  ZOOM_LEVELS,
  type Day,
  type LevelLayout,
} from "./layout.ts";
import type { TimelineItem } from "./api.ts";

function item(takenAt: string, offsetMinutes?: number): TimelineItem {
  return {
    id: `${takenAt}:${offsetMinutes ?? "local"}`,
    kind: "image",
    taken_at: takenAt,
    offset_minutes: offsetMinutes,
    state: "ready",
  };
}

/** A run of `n` items all on the same day, newest first. */
function sameDay(n: number): TimelineItem[] {
  return Array.from({ length: n }, (_, i) =>
    item(`2026-08-05T${String(23 - (i % 24)).padStart(2, "0")}:${pad(i)}:00Z`),
  ).map((it, i) => ({ ...it, id: `i${i}` }));
}

function pad(n: number): string {
  return String(n % 60).padStart(2, "0");
}

/** The stack of level layouts a live grid would build for one container width. */
function stack(days: Day[], width: number): LevelLayout[] {
  return ZOOM_LEVELS.map((cap) => layoutLevel(days, metricsFor(width, cap)));
}

test("metricsFor divides the container exactly", () => {
  for (const width of [375, 500, 768, 1200, 1600, 2560]) {
    for (const cap of ZOOM_LEVELS) {
      const m = metricsFor(width, cap);
      const covered = m.cellSize * m.columns + m.gap * (m.columns - 1);
      assert.ok(
        Math.abs(covered - width) < 0.001,
        `width ${width} cap ${cap}: ${m.columns} cells of ${m.cellSize} covered ${covered}`,
      );
      assert.ok(m.columns >= 1);
    }
  }
});

test("a zoom level is a ceiling on the cell size, never exceeded", () => {
  for (const width of [320, 768, 1024, 1440, 1920, 2560, 3840]) {
    for (const cap of ZOOM_LEVELS) {
      const { cellSize } = metricsFor(width, cap);
      assert.ok(
        cellSize <= cap + 0.001,
        `width ${width} produced a ${cellSize}px cell under a ${cap}px cap`,
      );
    }
  }
});

test("cells still fill most of their cap, so the grid stays responsive not sparse", () => {
  for (const width of [1024, 1440, 1920, 2560]) {
    for (const cap of ZOOM_LEVELS) {
      const { cellSize } = metricsFor(width, cap);
      assert.ok(
        cellSize > cap * 0.65,
        `width ${width} cap ${cap} collapsed to ${cellSize}px`,
      );
    }
  }
});

test("the levels are distinct and ordered", () => {
  for (let i = 1; i < ZOOM_LEVELS.length; i++) {
    assert.ok(
      ZOOM_LEVELS[i] > ZOOM_LEVELS[i - 1],
      `level ${i} (${ZOOM_LEVELS[i]}) does not grow on level ${i - 1}`,
    );
  }
  assert.equal(MAX_ZOOM, ZOOM_LEVELS.length - 1);
  assert.ok(DEFAULT_ZOOM >= 0 && DEFAULT_ZOOM <= MAX_ZOOM);
});

test("the default level stays near the grid the app shipped with", () => {
  for (const width of [1200, 1600, 1920]) {
    const { cellSize } = metricsFor(width, ZOOM_LEVELS[DEFAULT_ZOOM]);
    assert.ok(cellSize >= 140 && cellSize <= 160, `width ${width} gave ${cellSize}px`);
  }
});

// Two levels close enough together to divide a common window into the same
// number of columns would render identically, costing a click of the slider.
test("adjacent levels really do change the grid, at ordinary window widths", () => {
  for (const width of [1024, 1280, 1440, 1600, 1920, 2560]) {
    const sizes = ZOOM_LEVELS.map((cap) => metricsFor(width, cap).cellSize);
    for (let i = 1; i < sizes.length; i++) {
      assert.ok(
        sizes[i] - sizes[i - 1] > 1,
        `at ${width}px, levels ${i - 1} and ${i} both draw ${sizes[i]}px cells`,
      );
    }
  }
});

test("a photo files under its own local day, not the viewer's", () => {
  // 23:50 on August 4th in Vermont. A viewer in UTC would otherwise file this
  // under the 5th.
  assert.equal(dayKeyOf("2026-08-05T03:50:00Z", -240), "2026-08-04");
  assert.equal(dayKeyOf("2026-08-05T03:50:00Z", undefined), "2026-08-05");
});

test("a file with no recorded zone falls back to the viewer's day", () => {
  assert.equal(dayKeyOf("2026-08-05T03:50:00Z", null), "2026-08-05");
});

test("buildDays groups consecutive days and indexes them into the flat list", () => {
  const items = [
    item("2026-08-05T12:00:00Z"),
    item("2026-08-05T11:00:00Z"),
    item("2026-08-05T10:00:00Z"),
    item("2026-08-04T12:00:00Z"),
  ];
  const days = buildDays(items);

  assert.deepEqual(
    days.map((d) => [d.key, d.start, d.count]),
    [
      ["2026-08-05", 0, 3],
      ["2026-08-04", 3, 1],
    ],
  );
});

test("buildDays splits a day boundary using each file's own offset", () => {
  // Same instant-ordering, but the offsets put them on different local days.
  const items = [item("2026-08-05T03:50:00Z", -240), item("2026-08-05T02:00:00Z", 0)];
  assert.deepEqual(
    buildDays(items).map((d) => d.key),
    ["2026-08-04", "2026-08-05"],
  );
});

// The ids are React keys for the headings, and a date on both sides of a
// timezone hop is a date that appears twice.
test("buildDays gives every run its own id, even when a date recurs", () => {
  const items = [
    item("2026-08-05T02:00:00Z", 0),
    // 21:00 the previous evening in Vermont, taken between the two UTC photos.
    item("2026-08-05T01:00:00Z", -240),
    item("2026-08-05T00:30:00Z", 0),
  ];
  const days = buildDays(items);

  assert.deepEqual(
    days.map((d) => d.key),
    ["2026-08-05", "2026-08-04", "2026-08-05"],
  );
  assert.equal(new Set(days.map((d) => d.id)).size, days.length);
});

test("buildDays ids stay put as later pages append", () => {
  const first = buildDays(sameDay(4));
  const grown = buildDays([...sameDay(4), item("2026-08-04T12:00:00Z")]);

  assert.deepEqual(
    grown.slice(0, first.length).map((d) => d.id),
    first.map((d) => d.id),
  );
});

test("buildDays handles an empty timeline", () => {
  assert.deepEqual(buildDays([]), []);
});

test("layoutLevel stacks days without gaps and reports an exact height", () => {
  const days = buildDays([
    ...sameDay(4),
    { ...item("2026-08-04T12:00:00Z"), id: "prev" },
  ]);
  const metrics = { columns: 3, cellSize: 100, gap: 0, headerHeight: 50 };
  const { tops } = layoutLevel(days, metrics);

  // Aug 5: heading + two rows of a three-wide grid. Aug 4: heading + one row.
  assert.deepEqual(tops, [0, 250, 400]);
});

test("dayIndexOf finds the day holding any item", () => {
  const days = buildDays([...sameDay(5), { ...item("2026-08-04T12:00:00Z"), id: "p" }]);
  assert.deepEqual(
    [0, 1, 4, 5].map((i) => dayIndexOf(days, i)),
    [0, 0, 0, 1],
  );
});

test("tiles never overlap and rows advance by exactly one cell plus the gap", () => {
  const days = buildDays(sameDay(9));
  const levels = stack(days, 1000);
  const frame = frameAt(levels, DEFAULT_ZOOM);
  const columns = frame.a.metrics.columns;

  for (let offset = 0; offset < 9; offset++) {
    const r = tileRect(frame, 0, offset);
    assert.equal(r.size, frame.cellSize);
    if (offset >= columns) {
      const above = tileRect(frame, 0, offset - columns);
      assert.equal(r.x, above.x);
      assert.ok(Math.abs(r.y - above.y - (frame.cellSize + frame.gap)) < 1e-9);
    }
  }
});

test("a settled level is drawn exactly, from either side of the blend", () => {
  const days = buildDays(sameDay(40));
  const levels = stack(days, 1400);

  for (let level = 0; level <= MAX_ZOOM; level++) {
    const frame = frameAt(levels, level);
    assert.equal(
      frame.cellSize,
      levels[level].metrics.cellSize,
      `level ${level} did not land on its own cell size`,
    );
    assert.equal(frame.totalHeight, levels[level].tops[days.length]);
    assert.equal(tileRect(frame, 0, 7).y, tileRect(frameAt(levels, level), 0, 7).y);
  }
});

test("geometry is continuous across a level boundary", () => {
  const days = buildDays([...sameDay(30), { ...item("2026-08-04T12:00:00Z"), id: "p" }]);
  const levels = stack(days, 1400);

  for (let level = 1; level < MAX_ZOOM; level++) {
    const below = frameAt(levels, level - 1e-9);
    const above = frameAt(levels, level + 1e-9);
    for (const offset of [0, 5, 17, 29]) {
      const a = tileRect(below, 0, offset);
      const b = tileRect(above, 0, offset);
      assert.ok(
        Math.abs(a.x - b.x) < 1e-4 && Math.abs(a.y - b.y) < 1e-4,
        `tile ${offset} jumped crossing level ${level}`,
      );
    }
    assert.ok(Math.abs(headerY(below, 1) - headerY(above, 1)) < 1e-4);
    assert.ok(Math.abs(below.totalHeight - above.totalHeight) < 1e-4);
  }
});

test("tile tops only ever increase, which is what the binary searches assume", () => {
  const days = buildDays([...sameDay(37), { ...item("2026-08-04T12:00:00Z"), id: "p" }]);
  const levels = stack(days, 1400);

  for (const z of [0, 0.5, 1.3, 2, 2.75, 3.5, 4]) {
    const frame = frameAt(levels, z);
    let previous = -Infinity;
    for (let i = 0; i < 38; i++) {
      const y = itemTop(days, frame, i);
      assert.ok(y >= previous - 1e-9, `item ${i} sits above item ${i - 1} at z=${z}`);
      previous = y;
    }
  }
});

test("visibleItems selects exactly the tiles touching the viewport", () => {
  const days = buildDays(sameDay(60));
  const levels = stack(days, 1000);
  const frame = frameAt(levels, 1.4);
  const scrollTop = 300;
  const height = 400;

  const range = visibleItems(days, 60, frame, scrollTop, height);
  for (let i = 0; i < 60; i++) {
    const top = itemTop(days, frame, i);
    const touches = top < scrollTop + height && top + frame.cellSize > scrollTop;
    const included = i >= range.start && i < range.end;
    assert.equal(included, touches, `item ${i} at ${top} was ${included ? "" : "not "}included`);
  }
});

test("visibleItems widens by the overscan and clamps at both ends", () => {
  // Long enough that a viewport in the middle has room to overscan both ways
  // at every level, including the one with the tallest rows.
  const count = 400;
  const days = buildDays(sameDay(count));
  const levels = stack(days, 1000);

  for (let level = 0; level <= MAX_ZOOM; level++) {
    const frame = frameAt(levels, level);
    const middle = frame.totalHeight / 2;

    const tight = visibleItems(days, count, frame, middle, 400);
    const loose = visibleItems(days, count, frame, middle, 400, 500);
    assert.ok(loose.start < tight.start, `level ${level}: no earlier tiles reached`);
    assert.ok(loose.end > tight.end, `level ${level}: no later tiles reached`);

    assert.equal(visibleItems(days, count, frame, 0, 400, 500_000).start, 0);
    const past = visibleItems(days, count, frame, 1_000_000, 400);
    assert.ok(past.end <= count && past.start <= past.end);
  }
});

test("visibleItems on an empty timeline renders nothing", () => {
  const levels = stack([], 1000);
  assert.deepEqual(visibleItems([], 0, frameAt(levels, 2), 0, 800, 200), {
    start: 0,
    end: 0,
  });
});

test("itemAtPoint picks the tile under the pointer, column included", () => {
  const days = buildDays(sameDay(30));
  const levels = stack(days, 1000);
  const frame = frameAt(levels, DEFAULT_ZOOM);

  for (const offset of [0, 1, 4, 11, 29]) {
    const r = tileRect(frame, 0, offset);
    assert.equal(
      itemAtPoint(days, 30, frame, r.x + r.size / 2, r.y + r.size / 2),
      offset,
      `missed the tile at offset ${offset}`,
    );
  }
});

test("itemAtPoint stays inside the day whose row it landed in", () => {
  // Four items on the newer day, so its last row is short and a pointer off its
  // right-hand end must not select into the day below.
  const days = buildDays([...sameDay(4), { ...item("2026-08-04T12:00:00Z"), id: "p" }]);
  const levels = stack(days, 1000);
  const frame = frameAt(levels, DEFAULT_ZOOM);
  const last = tileRect(frame, 0, 3);

  assert.equal(itemAtPoint(days, 5, frame, 990, last.y + last.size / 2), 3);
});

test("dayAt reports the day covering a scroll position", () => {
  const days = buildDays([
    item("2026-08-05T12:00:00Z"),
    item("2026-08-04T12:00:00Z"),
    item("2026-08-03T12:00:00Z"),
  ]);
  const levels = stack(days, 1000);
  const frame = frameAt(levels, DEFAULT_ZOOM);

  assert.equal(dayAt(days, frame, 0)?.label, days[0].label);
  assert.equal(dayAt(days, frame, headerY(frame, 1))?.label, days[1].label);
  assert.equal(dayAt(days, frame, headerY(frame, 2) - 1)?.label, days[1].label);
  assert.equal(dayAt(days, frame, 999_999)?.label, days[2].label);
  assert.equal(dayAt([], frame, 0), null);
});

test("thumbSizeFor picks the smallest rendition that fills the cell", () => {
  // The mapping the pipeline was built for: 96 for the two smallest levels, the
  // base for the three in the middle, 512 for the two largest.
  assert.deepEqual(
    ZOOM_LEVELS.map((_, level) => thumbSizeFor(level)),
    [96, 96, 256, 256, 256, 512, 512],
  );
});

test("thumbSizeFor never draws a cell larger than the rendition it picks", () => {
  for (let level = 0; level <= MAX_ZOOM; level++) {
    assert.ok(
      thumbSizeFor(level) >= ZOOM_LEVELS[level],
      `level ${level} draws a ${ZOOM_LEVELS[level]}px cell from a ${thumbSizeFor(level)}px file`,
    );
  }
});

// The zoom is a continuous scalar and the grid asks for a rendition mid-flight,
// so a fractional level has to resolve to one of the sizes rather than to
// undefined.
test("thumbSizeFor handles positions between and beyond the levels", () => {
  assert.equal(thumbSizeFor(0.4), 96);
  assert.equal(thumbSizeFor(1.6), 256);
  assert.equal(thumbSizeFor(-3), 96);
  assert.equal(thumbSizeFor(MAX_ZOOM + 3), 512);
});

test("thumbSizeFallbacks asks for what it wants, then settles downwards", () => {
  assert.deepEqual(thumbSizeFallbacks(512), [512, 256, 96]);
  assert.deepEqual(thumbSizeFallbacks(96), [96, 256, 512]);
  // Bigger before smaller: 512 is further from 256 in pixels than 96 is, but it
  // is the one that does not have to be stretched to fill the cell.
  assert.deepEqual(thumbSizeFallbacks(256), [256, 512, 96]);
});

test("thumbSizeFallbacks offers every size exactly once", () => {
  for (const size of THUMB_SIZES) {
    const chain = thumbSizeFallbacks(size);
    assert.equal(chain.length, THUMB_SIZES.length);
    assert.equal(new Set(chain).size, THUMB_SIZES.length);
    assert.equal(chain[0], size);
  }
});
