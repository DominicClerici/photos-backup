"use client";

// Extension included on purpose: `node --test` runs this file directly (see
// zoom.test.ts) and resolves ESM specifiers the way the platform does, without
// a bundler to guess at it.
import { DEFAULT_ZOOM, MAX_ZOOM } from "./layout.ts";

/** One step of zoom, whatever else is already in flight. */
export const ZOOM_MS = 300;
/** Retargeting a step away costs less than a full step; below this it feels slack. */
const MIN_MS = 120;

const STORAGE_KEY = "photos.zoom";

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v;
}

export function savedZoom(): number {
  if (typeof window === "undefined") return DEFAULT_ZOOM;
  const raw = window.localStorage?.getItem(STORAGE_KEY);
  const level = raw == null ? NaN : Number(raw);
  return Number.isInteger(level) ? clamp(level, 0, MAX_ZOOM) : DEFAULT_ZOOM;
}

/**
 * The zoom position, as a single animated scalar along `ZOOM_LEVELS`.
 *
 * Everything the grid draws is a function of this one number, which is what
 * keeps the grid coherent: there are no per-tile animations to fall out of step
 * with each other, and the slider handle is not synchronised with the zoom so
 * much as it *is* the zoom, read a second time.
 *
 * Retargeting mid-flight — zooming out while a zoom in is still running, or
 * zooming out further than the transition already heading there — restarts the
 * curve from the position *and the speed* the old one had reached. The cubic
 * below is the unique one through (0, from) and (1, to) with those two end
 * slopes, so a reversal bends around instead of stopping dead, and nothing ever
 * jumps. Left alone from rest it is an ease-in-out.
 */
export class Zoom {
  private from: number;
  private goal: number;
  private current: number;
  /** Levels per millisecond, carried into the next curve on a retarget. */
  private speed = 0;
  private slope = 0;
  private startedAt = 0;
  private duration = ZOOM_MS;
  private raf = 0;
  private scrubbing = false;
  private listeners = new Set<() => void>();

  constructor(initial: number = DEFAULT_ZOOM) {
    const at = clamp(initial, 0, MAX_ZOOM);
    this.from = this.goal = this.current = at;
  }

  /** Where the zoom is right now; integers are the settled levels. */
  get value(): number {
    return this.current;
  }

  /** The level being animated towards, or the drag position while scrubbing. */
  get target(): number {
    return this.goal;
  }

  /** Whether the grid geometry is still changing under its own steam. */
  get moving(): boolean {
    return this.raf !== 0 || this.scrubbing;
  }

  subscribe(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  /** Animates to a level, picking up whatever speed the current motion has. */
  to(level: number, duration = ZOOM_MS): void {
    const goal = clamp(level, 0, MAX_ZOOM);
    const now = performance.now();
    if (this.raf) this.sample(now);

    this.from = this.current;
    this.slope = this.speed;
    this.goal = goal;
    this.startedAt = now;
    this.scrubbing = false;

    const distance = Math.abs(goal - this.from);
    this.duration = Math.min(ZOOM_MS, Math.max(MIN_MS, ZOOM_MS * distance));

    if (this.from === goal && this.slope === 0) {
      // Already there and standing still — nothing to animate, but say so
      // anyway so the slider surfaces to show the level is at its limit.
      this.emit();
      return;
    }
    if (!this.raf) this.raf = requestAnimationFrame(this.tick);
    this.remember(goal);
    this.emit();
  }

  /** Steps relative to where the zoom is *heading*, so held keys compound. */
  step(delta: number): void {
    this.to(Math.round(this.goal) + delta);
  }

  /** Follows a drag one-to-one: no curve, no duration, just the handle. */
  scrub(value: number): void {
    if (this.raf) {
      cancelAnimationFrame(this.raf);
      this.raf = 0;
    }
    const at = clamp(value, 0, MAX_ZOOM);
    this.from = this.goal = this.current = at;
    this.speed = this.slope = 0;
    this.scrubbing = true;
    this.emit();
  }

  /** Eases from a released drag to the nearest level. */
  settle(): void {
    this.to(Math.round(this.current));
  }

  dispose(): void {
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
    this.listeners.clear();
  }

  private tick = () => {
    const now = performance.now();
    const done = this.sample(now);
    this.raf = done ? 0 : requestAnimationFrame(this.tick);
    this.emit();
  };

  /** Advances `current` and `speed` to time `now`; true once the curve is spent. */
  private sample(now: number): boolean {
    const s = (now - this.startedAt) / this.duration;
    if (!(s < 1)) {
      this.current = this.goal;
      this.speed = 0;
      return true;
    }

    const s2 = s * s;
    const s3 = s2 * s;
    const m0 = this.slope * this.duration;

    this.current =
      (2 * s3 - 3 * s2 + 1) * this.from +
      (s3 - 2 * s2 + s) * m0 +
      (-2 * s3 + 3 * s2) * this.goal;
    this.speed =
      ((6 * s2 - 6 * s) * this.from +
        (3 * s2 - 4 * s + 1) * m0 +
        (-6 * s2 + 6 * s) * this.goal) /
      this.duration;
    return false;
  }

  private remember(level: number): void {
    if (typeof window === "undefined" || !Number.isInteger(level)) return;
    try {
      window.localStorage.setItem(STORAGE_KEY, String(level));
    } catch {
      // Private-mode storage refusals are not worth breaking a zoom over.
    }
  }

  private emit(): void {
    for (const fn of this.listeners) fn();
  }
}
