// Run with TZ=UTC (see package.json). The day-grouping tests compare the file's
// own timezone against the viewer's, and they only prove anything if the
// viewer's is known.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  buildRows,
  dayKeyOf,
  itemsInRange,
  metricsFor,
  sectionAt,
  visibleRange,
  type GridMetrics,
  type Row,
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

const metrics: GridMetrics = {
  columns: 3,
  cellSize: 100,
  gap: 0,
  headerHeight: 50,
};

test("metricsFor divides the container exactly", () => {
  for (const width of [375, 500, 768, 1200, 1600, 2560]) {
    const m = metricsFor(width);
    const covered = m.cellSize * m.columns + m.gap * (m.columns - 1);
    assert.ok(
      Math.abs(covered - width) < 0.001,
      `width ${width}: ${m.columns} cells of ${m.cellSize} covered ${covered}`,
    );
    assert.ok(m.columns >= 1);
  }
});

test("metricsFor keeps cells near the size a 256px thumbnail can fill", () => {
  for (const width of [768, 1024, 1440, 1920, 2560]) {
    const { cellSize } = metricsFor(width);
    assert.ok(
      cellSize >= 120 && cellSize <= 200,
      `width ${width} produced a ${cellSize}px cell`,
    );
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

test("buildRows groups consecutive days and stacks rows without gaps", () => {
  const items = [
    item("2026-08-05T12:00:00Z"),
    item("2026-08-05T11:00:00Z"),
    item("2026-08-05T10:00:00Z"),
    item("2026-08-05T09:00:00Z"),
    item("2026-08-04T12:00:00Z"),
  ];

  const { rows, sections, totalHeight } = buildRows(items, metrics);

  // Aug 5: header + two rows (4 items over 3 columns). Aug 4: header + one row.
  assert.deepEqual(
    rows.map((r) => r.kind),
    ["header", "tiles", "tiles", "header", "tiles"],
  );
  assert.equal(sections.length, 2);

  let expectedTop = 0;
  for (const row of rows) {
    assert.equal(row.top, expectedTop, `row ${row.key} starts at the wrong offset`);
    expectedTop += row.height;
  }
  assert.equal(totalHeight, expectedTop);

  const firstHeader = rows[0] as Extract<Row, { kind: "header" }>;
  assert.equal(firstHeader.count, 4);

  // The last row of a day is short rather than borrowing from the next day.
  const secondRow = rows[2] as Extract<Row, { kind: "tiles" }>;
  assert.equal(secondRow.items.length, 1);
});

test("buildRows splits a day boundary using each file's own offset", () => {
  // Same instant-ordering, but the offsets put them on different local days.
  const items = [item("2026-08-05T03:50:00Z", -240), item("2026-08-05T02:00:00Z", 0)];
  const { sections } = buildRows(items, metrics);

  assert.deepEqual(
    sections.map((s) => s.key),
    ["2026-08-04", "2026-08-05"],
  );
});

test("buildRows handles an empty timeline", () => {
  const model = buildRows([], metrics);
  assert.deepEqual(model.rows, []);
  assert.deepEqual(model.sections, []);
  assert.equal(model.totalHeight, 0);
});

test("visibleRange selects exactly the rows touching the viewport", () => {
  const items = Array.from({ length: 30 }, (_, i) =>
    item(`2026-08-05T${String(23 - i).padStart(2, "0")}:00:00Z`),
  );
  const { rows } = buildRows(items, metrics);
  // header 50, then ten rows of 100: tops are 50, 150, 250, ...

  const range = visibleRange(rows, 250, 200);
  for (let i = range.start; i < range.end; i++) {
    const row = rows[i];
    assert.ok(
      row.top < 450 && row.top + row.height > 250,
      `row at ${row.top} does not touch [250, 450)`,
    );
  }
  // Nothing outside the range may touch the viewport.
  for (let i = 0; i < rows.length; i++) {
    if (i >= range.start && i < range.end) continue;
    const row = rows[i];
    assert.ok(
      row.top >= 450 || row.top + row.height <= 250,
      `row at ${row.top} was excluded but is visible`,
    );
  }
});

test("visibleRange widens by the overscan and clamps at both ends", () => {
  const items = Array.from({ length: 30 }, (_, i) =>
    item(`2026-08-05T${String(23 - i).padStart(2, "0")}:00:00Z`),
  );
  const { rows } = buildRows(items, metrics);

  const tight = visibleRange(rows, 500, 200);
  const loose = visibleRange(rows, 500, 200, 300);
  assert.ok(loose.start < tight.start, "overscan should reach earlier rows");
  assert.ok(loose.end > tight.end, "overscan should reach later rows");

  // At the very top and very bottom the range must stay inside the array.
  const top = visibleRange(rows, 0, 200, 1000);
  assert.equal(top.start, 0);
  const bottom = visibleRange(rows, 100_000, 200);
  assert.ok(bottom.end <= rows.length);
  assert.ok(bottom.start <= bottom.end);
});

test("visibleRange on an empty model renders nothing", () => {
  assert.deepEqual(visibleRange([], 0, 800, 200), { start: 0, end: 0 });
});

test("sectionAt reports the day covering the scroll position", () => {
  const items = [
    item("2026-08-05T12:00:00Z"),
    item("2026-08-04T12:00:00Z"),
    item("2026-08-03T12:00:00Z"),
  ];
  const { sections } = buildRows(items, metrics);

  assert.equal(sectionAt(sections, 0)?.key, "2026-08-05");
  assert.equal(sectionAt(sections, sections[1].top)?.key, "2026-08-04");
  assert.equal(sectionAt(sections, sections[1].bottom - 1)?.key, "2026-08-04");
  assert.equal(sectionAt(sections, 999_999)?.key, "2026-08-03");
  assert.equal(sectionAt([], 0), null);
});

test("itemsInRange returns only tiles that are actually rendered", () => {
  const items = Array.from({ length: 9 }, (_, i) =>
    item(`2026-08-05T${String(9 - i).padStart(2, "0")}:00:00Z`),
  );
  const { rows } = buildRows(items, metrics);

  const visible = itemsInRange(rows, visibleRange(rows, 0, 150));
  assert.ok(visible.length > 0 && visible.length < items.length);
  assert.deepEqual(
    visible.map((i) => i.id),
    items.slice(0, visible.length).map((i) => i.id),
  );
});
