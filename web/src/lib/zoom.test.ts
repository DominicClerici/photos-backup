// The interruption behaviour is the whole point of the curve in zoom.ts, and it
// is the one part of the zoom that can be checked without a browser: drive the
// clock and the frame loop by hand and read the value back every frame.

import { test } from "node:test";
import assert from "node:assert/strict";

import { Zoom, ZOOM_MS } from "./zoom.ts";
import { MAX_ZOOM } from "./layout.ts";

const FRAME = 1000 / 60;

const host = globalThis as unknown as {
  performance: { now(): number };
  requestAnimationFrame(fn: (t: number) => void): number;
  cancelAnimationFrame(id: number): void;
};

let clock = 0;
let nextID = 1;
const queued = new Map<number, (t: number) => void>();

host.performance = { now: () => clock };
host.requestAnimationFrame = (fn) => {
  const id = nextID++;
  queued.set(id, fn);
  return id;
};
host.cancelAnimationFrame = (id) => {
  queued.delete(id);
};

function fresh(level: number): Zoom {
  queued.clear();
  clock = 0;
  return new Zoom(level);
}

/** Advances one animation frame and reports where the zoom landed. */
function frame(zoom: Zoom): number {
  clock += FRAME;
  const due = [...queued.values()];
  queued.clear();
  for (const fn of due) fn(clock);
  return zoom.value;
}

/** Every value the zoom passes through over `ms`, one entry per frame. */
function run(zoom: Zoom, ms: number): number[] {
  const path: number[] = [];
  for (let t = 0; t < ms; t += FRAME) path.push(frame(zoom));
  return path;
}

function biggestStep(path: number[]): number {
  let worst = 0;
  for (let i = 1; i < path.length; i++) {
    worst = Math.max(worst, Math.abs(path[i] - path[i - 1]));
  }
  return worst;
}

test("a step takes the full duration and lands exactly on the level", () => {
  const zoom = fresh(2);
  zoom.to(3);

  run(zoom, ZOOM_MS - 3 * FRAME);
  assert.ok(zoom.value < 3, `arrived early, at ${zoom.value}`);
  assert.ok(zoom.value > 2.9, `still at ${zoom.value} with a frame to go`);

  run(zoom, 4 * FRAME);
  assert.equal(zoom.value, 3);
  assert.equal(zoom.moving, false);
});

test("a step eases in and out rather than running at a constant rate", () => {
  const zoom = fresh(2);
  zoom.to(3);
  const path = run(zoom, ZOOM_MS);

  const mid = Math.floor(path.length / 2);
  const first = path[1] - path[0];
  const middle = path[mid] - path[mid - 1];
  const last = path[path.length - 1] - path[path.length - 2];
  assert.ok(middle > first * 2, "should still be accelerating out of the start");
  assert.ok(middle > last * 2, "should be settling into the end");
});

test("nothing teleports, even crossing the whole scale at once", () => {
  const zoom = fresh(0);
  zoom.to(MAX_ZOOM);
  const path = run(zoom, ZOOM_MS + 100);

  // An ease-in-out peaks at 1.5x its average rate, so anything much past that
  // is a jump rather than a fast frame.
  const even = (MAX_ZOOM * FRAME) / ZOOM_MS;
  assert.ok(
    biggestStep(path) < even * 1.6,
    `a frame jumped ${biggestStep(path)} levels against an even ${even}`,
  );
});

test("reversing mid-transition carries the old speed through the turn", () => {
  const zoom = fresh(2);
  zoom.to(3);
  const out = run(zoom, 200);
  const turned = out[out.length - 1];
  assert.ok(turned > 2 && turned < 3, `expected to be mid-flight, got ${turned}`);

  zoom.step(-1);
  const back = run(zoom, ZOOM_MS + 100);

  // The frames straight after the reversal still travel the *old* way: a curve
  // that stopped dead and restarted would show a negative first step here.
  assert.ok(back[0] > turned, `stopped dead at the reversal (${turned} -> ${back[0]})`);
  assert.ok(biggestStep([turned, ...back]) < 0.35, "the turn was not smooth");

  // It comes back where it started without ever overshooting the level it had
  // been heading for.
  assert.equal(zoom.value, 2);
  assert.ok(Math.max(...back) < 3, `overshot past the abandoned target: ${Math.max(...back)}`);
  assert.ok(Math.min(...back) >= 2, `undershot below the destination: ${Math.min(...back)}`);
});

test("zooming further mid-transition keeps going without a hitch", () => {
  const zoom = fresh(4);
  zoom.to(3);
  const path = run(zoom, 200);
  const at = path[path.length - 1];
  assert.ok(at > 3 && at < 4, `expected to be mid-flight, got ${at}`);

  zoom.step(-1);
  const rest = run(zoom, ZOOM_MS + 100);

  const whole = [...path, ...rest];
  for (let i = 1; i < whole.length; i++) {
    assert.ok(whole[i] <= whole[i - 1] + 1e-9, `bounced back up at frame ${i}`);
  }
  assert.ok(biggestStep(whole) < 0.35, "the extension was not smooth");
  assert.equal(zoom.value, 2);
});

test("repeated reversals stay put, converge, and never break stride", () => {
  // Carrying speed through a turn means the value can coast a hair past the
  // level it is returning to before the curve pulls it back. A sweep over
  // reversal intervals from one frame to a full transition puts the worst case
  // at under a hundredth of a level — a third of a pixel of cell size — so it
  // costs nothing, and buying it back would mean braking at every reversal.
  const SLACK = 0.02;

  for (const gap of [FRAME, 60, 120, 250, 380]) {
    const zoom = fresh(2);
    const seen: number[] = [];
    for (let i = 0; i < 8; i++) {
      zoom.step(i % 2 === 0 ? 1 : -1);
      seen.push(...run(zoom, gap));
    }
    seen.push(...run(zoom, 2 * ZOOM_MS));

    assert.ok(Math.min(...seen) > 2 - SLACK, `dipped to ${Math.min(...seen)} at ${gap}ms`);
    assert.ok(Math.max(...seen) < 3 + SLACK, `rose to ${Math.max(...seen)} at ${gap}ms`);
    assert.ok(biggestStep(seen) < 0.35, `a ${gap}ms reversal chain broke the motion`);
    assert.equal(zoom.value, zoom.target, `never settled after ${gap}ms reversals`);
  }
});

test("the ends of the range hold", () => {
  const bottom = fresh(0);
  bottom.step(-1);
  run(bottom, ZOOM_MS + 100);
  assert.equal(bottom.value, 0);

  const top = fresh(MAX_ZOOM);
  top.step(1);
  run(top, ZOOM_MS + 100);
  assert.equal(top.value, MAX_ZOOM);
});

test("steps compound off where the zoom is heading, not where it is", () => {
  const zoom = fresh(0);
  zoom.to(1);
  run(zoom, 100);
  zoom.step(1);
  assert.equal(zoom.target, 2);
  zoom.step(1);
  assert.equal(zoom.target, 3);
  run(zoom, 2 * ZOOM_MS);
  assert.equal(zoom.value, 3);
});

test("a drag moves one-to-one and releases onto the nearest level", () => {
  const zoom = fresh(2);
  zoom.scrub(2.62);
  assert.equal(zoom.value, 2.62);
  assert.equal(zoom.moving, true, "a held drag still counts as moving geometry");

  zoom.scrub(2.71);
  assert.equal(zoom.value, 2.71);

  zoom.settle();
  const path = run(zoom, ZOOM_MS + 100);
  assert.equal(zoom.value, 3);
  assert.ok(biggestStep([2.71, ...path]) < 0.35, "the release snapped rather than eased");
});

test("subscribers hear about every frame, and stop hearing after disposal", () => {
  const zoom = fresh(2);
  let beats = 0;
  const off = zoom.subscribe(() => beats++);

  zoom.to(3);
  const during = beats;
  run(zoom, ZOOM_MS + 100);
  const expected = ZOOM_MS / FRAME;
  assert.ok(
    beats - during > expected * 0.8,
    `only ${beats - during} frames were announced, expected about ${expected}`,
  );

  off();
  const quiet = beats;
  zoom.to(2);
  run(zoom, ZOOM_MS);
  assert.equal(beats, quiet);
});
