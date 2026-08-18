"use client";

import { useEffect, useState } from "react";
import { Grid2x2, Grid3x3 } from "lucide-react";

import { Slider } from "@/components/ui/slider";
import { MAX_ZOOM, ZOOM_LEVELS } from "@/lib/layout";
import type { Zoom } from "@/lib/zoom";
import { cn } from "@/lib/utils";

/** Quiet time after the last zoom, and after the pointer leaves, before hiding. */
const IDLE_MS = 1500;
const FADE_IN_MS = 100;
const FADE_OUT_MS = 500;

type Phase = "hidden" | "visible" | "fading";

/**
 * The zoom readout: surfaces on a zoom, follows it, then gets out of the way.
 *
 * The handle is bound to the zoom's continuous value rather than to the level
 * it is heading for, so it is in step with the grid by construction — it starts
 * moving on the same frame the tiles do and lands with them, including through
 * a reversal.
 */
export function ZoomSlider({ zoom }: { zoom: Zoom }) {
  const [z, setZ] = useState(zoom.value);
  const [phase, setPhase] = useState<Phase>("hidden");
  const [hovered, setHovered] = useState(false);

  useEffect(
    () =>
      zoom.subscribe(() => {
        setZ(zoom.value);
        setPhase("visible");
      }),
    [zoom],
  );

  // `z` is a dependency so that a running transition keeps pushing the idle
  // timer out: the countdown is to the last frame of zoom, not to the keypress
  // that started it.
  useEffect(() => {
    if (hovered || phase === "hidden") return;
    if (phase === "visible") {
      const t = window.setTimeout(() => setPhase("fading"), IDLE_MS);
      return () => window.clearTimeout(t);
    }
    const t = window.setTimeout(() => setPhase("hidden"), FADE_OUT_MS);
    return () => window.clearTimeout(t);
  }, [phase, hovered, z]);

  return (
    <div
      className={cn(
        // Above the tab bar, which holds the bottom of the screen permanently;
        // this one comes and goes, so it is the one that moves out of the way.
        "fixed bottom-24 left-1/2 z-20 -translate-x-1/2",
        phase === "hidden" && "pointer-events-none",
      )}
      style={{
        opacity: phase === "visible" ? 1 : 0,
        visibility: phase === "hidden" ? "hidden" : "visible",
        transitionProperty: "opacity",
        transitionDuration: `${phase === "visible" ? FADE_IN_MS : FADE_OUT_MS}ms`,
        transitionTimingFunction: "linear",
      }}
      onPointerEnter={() => {
        setHovered(true);
        setPhase("visible");
      }}
      onPointerLeave={() => setHovered(false)}
    >
      <div className="flex items-center gap-3.5 rounded-full border bg-card/70 px-4 py-2.5 shadow-lg backdrop-blur-md">
        <Grid3x3 className="size-3.5 shrink-0 text-faint" aria-hidden="true" />

        <div className="relative flex w-44 items-center">
          <Slider
            className="w-full [&_[data-slot=slider-thumb]]:z-10 [&_[data-slot=slider-track]]:bg-foreground/15"
            aria-label="Zoom"
            value={[z]}
            min={0}
            max={MAX_ZOOM}
            // Fine enough that a drag reads as continuous; the levels are
            // reimposed on release rather than during the drag.
            step={0.001}
            thumbAlignment="center"
            onKeyDownCapture={(e) => {
              const level = levelForKey(e.key, zoom.target);
              if (level === null) return;
              // Ahead of Base UI's own handler, which would otherwise nudge the
              // value by a thousandth of a level.
              e.preventDefault();
              e.stopPropagation();
              zoom.to(level);
            }}
            onValueChange={(value, details) => {
              const v = typeof value === "number" ? value : value[0];
              if (details.reason === "drag") zoom.scrub(v);
              // A press on the track is a jump, and a jump would teleport the
              // grid, so it eases to the nearest level like every other input.
              else if (details.reason === "track-press") zoom.to(Math.round(v));
            }}
            onValueCommitted={(_, details) => {
              if (details.reason === "drag") zoom.settle();
            }}
          />

          <div aria-hidden="true" className="pointer-events-none absolute inset-0">
            {ZOOM_LEVELS.map((size, i) => (
              // Marks the handle has passed sit on the filled part of the
              // track, where a light dot disappears — so they invert as it
              // goes by rather than dropping out of the scale.
              <span
                key={size}
                className={cn(
                  "absolute top-1/2 size-[3px] -translate-x-1/2 -translate-y-1/2 rounded-full",
                  i < z ? "bg-primary-foreground/45" : "bg-foreground/40",
                )}
                style={{ left: `${(i / MAX_ZOOM) * 100}%` }}
              />
            ))}
          </div>
        </div>

        <Grid2x2 className="size-4 shrink-0 text-faint" aria-hidden="true" />
      </div>
    </div>
  );
}

function levelForKey(key: string, target: number): number | null {
  switch (key) {
    case "ArrowLeft":
    case "ArrowDown":
      return Math.round(target) - 1;
    case "ArrowRight":
    case "ArrowUp":
      return Math.round(target) + 1;
    case "Home":
    case "PageDown":
      return 0;
    case "End":
    case "PageUp":
      return MAX_ZOOM;
    default:
      return null;
  }
}
