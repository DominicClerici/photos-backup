// The selection model, checked without a grid. Everything a drag or a
// shift-click can do to a selection ends up as one call to `apply`, so the
// interesting cases are all here: overlapping a run, splitting one, closing the
// gap between two, and taking away ground that was never held.

import { test } from "node:test";
import assert from "node:assert/strict";

import { add, apply, between, count, has, NONE, remove, type Ranges } from "./ranges.ts";

/** Compact spelling of a selection, for readable assertions. */
const spell = (ranges: Ranges) => ranges.map((r) => `${r.start}-${r.end}`).join(",");

test("an empty selection holds nothing", () => {
  assert.equal(count(NONE), 0);
  assert.equal(has(NONE, 0), false);
});

test("adding a run", () => {
  const one = add(NONE, 4, 7);
  assert.equal(spell(one), "4-7");
  assert.equal(count(one), 3);
  assert.equal(has(one, 3), false);
  assert.equal(has(one, 4), true);
  assert.equal(has(one, 6), true);
  assert.equal(has(one, 7), false);
});

test("an empty run changes nothing, and does not copy", () => {
  const one = add(NONE, 4, 7);
  assert.equal(add(one, 5, 5), one);
  assert.equal(remove(one, 5, 4), one);
});

test("runs stay sorted and disjoint however they arrive", () => {
  let sel = add(NONE, 20, 24);
  sel = add(sel, 0, 4);
  sel = add(sel, 10, 12);
  assert.equal(spell(sel), "0-4,10-12,20-24");
  assert.equal(count(sel), 10);
});

test("touching runs become one", () => {
  const sel = add(add(NONE, 0, 4), 4, 8);
  assert.equal(spell(sel), "0-8");
});

test("a run spanning several closes the gaps between them", () => {
  let sel = add(add(add(NONE, 0, 2), 6, 8), 20, 22);
  sel = add(sel, 1, 7);
  assert.equal(spell(sel), "0-8,20-22");
});

test("removing the middle of a run splits it", () => {
  const sel = remove(add(NONE, 0, 10), 4, 6);
  assert.equal(spell(sel), "0-4,6-10");
  assert.equal(count(sel), 8);
  assert.equal(has(sel, 5), false);
});

test("removing across several runs clears every one it covers", () => {
  let sel = add(add(add(NONE, 0, 4), 6, 8), 20, 22);
  sel = remove(sel, 2, 21);
  assert.equal(spell(sel), "0-2,21-22");
});

test("removing ground that was never held is a no-op", () => {
  const sel = add(NONE, 0, 4);
  assert.equal(spell(remove(sel, 10, 20)), "0-4");
});

test("a drag reads the same in either direction", () => {
  assert.deepEqual(between(9, 4), between(4, 9));
  assert.deepEqual(between(4, 9), { start: 4, end: 10 });
  assert.deepEqual(between(7, 7), { start: 7, end: 8 });
});

test("a drag that reverses gives back what it took", () => {
  // Five rows dragged over and then dragged back: the preview is always the run
  // between the tile the drag started on and the one under the pointer, so the
  // committed selection never has to remember what to undo.
  const held = add(NONE, 100, 104);
  const forward = apply(held, ...spread(between(40, 90)), "add");
  assert.equal(count(forward), 55);
  const back = apply(held, ...spread(between(40, 45)), "add");
  assert.equal(spell(back), "40-46,100-104");
});

test("a drag begun on a selected tile takes away instead", () => {
  const held = add(NONE, 0, 100);
  const dragged = apply(held, ...spread(between(10, 19)), "remove");
  assert.equal(spell(dragged), "0-10,20-100");
  // Tiles in the run that were not selected to begin with stay that way: the
  // gesture is "take this run away", not "invert it".
  const gapped = apply(remove(held, 12, 14), ...spread(between(10, 19)), "remove");
  assert.equal(spell(gapped), "0-10,20-100");
});

test("a selection of a hundred thousand items is one run", () => {
  const all = add(NONE, 0, 100_000);
  assert.equal(all.length, 1);
  assert.equal(count(all), 100_000);
  assert.equal(has(all, 99_999), true);
  assert.equal(has(all, 100_000), false);
});

function spread(r: { start: number; end: number }): [number, number] {
  return [r.start, r.end];
}
