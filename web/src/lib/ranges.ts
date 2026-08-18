// A selection over a timeline, stored as the runs of indices it covers.
//
// Indices rather than ids, because the grid is addressed by index: the day
// table fixes a place for every item in the collection before a single one has
// been fetched, so index 48,000 is a real, nameable photograph whether or not
// the client is holding it. A selection of ids could only ever cover what had
// been paged in, which would make "drag from here to the bottom of 2019" mean
// something different depending on how fast you dragged.
//
// Runs rather than a set of indices, because the gestures that produce a
// selection are runs. A drag across five rows is one interval, not two hundred
// numbers, and dragging back the way you came shrinks it rather than
// remembering which of the two hundred to take out again. The whole library
// selected is one entry.
//
// Every function here returns a fresh list and never mutates its input, so a
// list can be held in React state and compared by identity.

/** A run of item indices, `start` inclusive and `end` exclusive. */
export interface Range {
  start: number;
  end: number;
}

/**
 * Runs in ascending order, disjoint, and never merely touching: [0,2) and [2,5)
 * are always stored as [0,5). Every function below preserves that, which is
 * what makes `has` a binary search and equality a comparison of lengths.
 */
export type Ranges = readonly Range[];

export const NONE: Ranges = [];

/** Whether a gesture is laying selection down or taking it away. */
export type SelectMode = "add" | "remove";

export function has(ranges: Ranges, index: number): boolean {
  let lo = 0;
  let hi = ranges.length - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (ranges[mid].end <= index) lo = mid + 1;
    else if (ranges[mid].start > index) hi = mid - 1;
    else return true;
  }
  return false;
}

/** How many items the selection holds. */
export function count(ranges: Ranges): number {
  let n = 0;
  for (const run of ranges) n += run.end - run.start;
  return n;
}

export function add(ranges: Ranges, start: number, end: number): Ranges {
  if (end <= start) return ranges;

  const out: Range[] = [];
  let i = 0;
  // `<` rather than `<=`: a run ending exactly where this one begins is
  // adjacent, and adjacent runs are one run.
  while (i < ranges.length && ranges[i].end < start) out.push(ranges[i++]);

  let from = start;
  let to = end;
  while (i < ranges.length && ranges[i].start <= to) {
    from = Math.min(from, ranges[i].start);
    to = Math.max(to, ranges[i].end);
    i++;
  }
  out.push({ start: from, end: to });

  while (i < ranges.length) out.push(ranges[i++]);
  return out;
}

export function remove(ranges: Ranges, start: number, end: number): Ranges {
  if (end <= start) return ranges;

  const out: Range[] = [];
  for (const run of ranges) {
    if (run.end <= start || run.start >= end) {
      out.push(run);
      continue;
    }
    // What is left of this run on either side of the hole. A run cut through
    // the middle leaves both.
    if (run.start < start) out.push({ start: run.start, end: start });
    if (run.end > end) out.push({ start: end, end: run.end });
  }
  return out;
}

/** One gesture's worth of change: a run, laid down or taken away. */
export function apply(
  ranges: Ranges,
  start: number,
  end: number,
  mode: SelectMode,
): Ranges {
  return mode === "add" ? add(ranges, start, end) : remove(ranges, start, end);
}

/**
 * The run two tiles bracket, in the order the grid reads.
 *
 * Which end the gesture started from does not matter: a drag from row nine up
 * to row four covers the same photographs as one from four down to nine, and
 * both are the tiles between them counted left to right through the rows.
 */
export function between(a: number, b: number): Range {
  return a <= b ? { start: a, end: b + 1 } : { start: b, end: a + 1 };
}
