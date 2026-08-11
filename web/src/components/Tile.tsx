"use client";

import { useEffect, useRef, useState } from "react";

import { liveThumbUrl, thumbUrl, type TimelineItem } from "@/lib/api";
import { formatDuration } from "@/lib/format";
import { cn } from "@/lib/utils";

/**
 * How long a pointer has to rest on a tile before its Live Photo starts.
 *
 * Sweeping the mouse across the grid crosses a dozen tiles in that time, and
 * without the delay every one of them would start pulling down a video for an
 * animation nobody was going to see.
 */
const HOVER_DELAY_MS = 120;

interface Props {
  item: TimelineItem;
  /**
   * The box the tile is laid out at. During a zoom this is the largest size the
   * tile will reach, and `transform` scales it down from there — resizing the
   * box every frame would mean re-rasterising every thumbnail every frame,
   * while scaling one that is already big enough only costs a composite.
   */
  size: number;
  transform: string;
  /** Where the render loop finds this tile in the day model, without a lookup. */
  day: number;
  offset: number;
  onOpen: (id: string) => void;
}

export function Tile({ item, size, transform, day, offset, onOpen }: Props) {
  const [broken, setBroken] = useState(false);
  const [seenState, setSeenState] = useState(item.state);

  // A tile that 404'd while its job was still running must try again once the
  // poller says the derivative landed. The component is keyed by id and so
  // survives that transition; without this it would stay broken forever.
  if (seenState !== item.state) {
    setSeenState(item.state);
    setBroken(false);
  }

  // "pending" means the thumbnail provably does not exist yet, so asking for it
  // would 404 by design. Every other state is worth an attempt: a metadata job
  // can fail *after* writing the thumbnail, and that photo should still appear.
  const attempt = item.state !== "pending" && !broken;
  const label = item.kind === "video" ? "Video" : "Photo";

  return (
    <button
      type="button"
      className="group absolute top-0 left-0 block origin-top-left overflow-hidden rounded-md bg-tile focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
      style={{ width: size, height: size, transform }}
      data-day={day}
      data-tile={offset}
      onClick={() => onOpen(item.id)}
      aria-label={`${label} taken ${new Date(item.taken_at).toLocaleString()}`}
    >
      {attempt ? (
        <img
          className="block size-full object-cover transition-[filter] group-hover:brightness-[1.08]"
          src={thumbUrl(item.id)}
          alt=""
          loading="lazy"
          decoding="async"
          draggable={false}
          onError={() => setBroken(true)}
        />
      ) : (
        <span
          className={cn(
            "flex size-full items-center justify-center",
            broken
              ? "text-faint"
              : "animate-shimmer bg-[linear-gradient(100deg,var(--tile)_30%,var(--tile-sheen)_50%,var(--tile)_70%)] bg-[length:200%_100%] motion-reduce:animate-none",
          )}
        >
          {broken ? <BrokenGlyph /> : null}
        </span>
      )}

      {attempt && item.live === "ready" ? <Motion id={item.id} /> : null}

      {item.live && item.live !== "failed" ? (
        <span
          className="absolute top-1.5 left-1.5 text-white/90 [filter:drop-shadow(0_1px_2px_rgb(0_0_0/0.6))]"
          aria-hidden="true"
        >
          <LiveGlyph />
        </span>
      ) : null}

      {item.kind === "video" ? (
        <span className="absolute right-1.5 bottom-[5px] flex items-center gap-[3px] text-[11px] tabular-nums text-white [text-shadow:0_1px_3px_rgb(0_0_0/0.7)]">
          <PlayGlyph />
          {item.duration ? <span>{formatDuration(item.duration)}</span> : null}
        </span>
      ) : null}
    </button>
  );
}

/**
 * The three seconds a Live Photo carries, played over its own still.
 *
 * The still stays mounted underneath rather than being swapped out. The video
 * fades in only once it is actually playing, so a slow first frame shows the
 * photo rather than a black square, and the fade back out at the end lands on
 * the picture that was there all along.
 *
 * Hover, not touch: pointerenter fires for a tap too, but pointerleave often
 * does not, which leaves a video playing under a finger that has moved on.
 */
function Motion({ id }: { id: string }) {
  const timer = useRef(0);
  const [armed, setArmed] = useState(false);
  const [visible, setVisible] = useState(false);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  const enter = () => {
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setArmed(true), HOVER_DELAY_MS);
  };

  const leave = () => {
    window.clearTimeout(timer.current);
    setVisible(false);
    setArmed(false);
  };

  return (
    // Mounted on hover and unmounted on leave, which is what makes coming back
    // to a tile play the clip again rather than resume it three frames from the
    // end.
    <span className="absolute inset-0" onMouseEnter={enter} onMouseLeave={leave}>
      {armed ? (
        <video
          className="block size-full object-cover transition-opacity duration-150"
          style={{ opacity: visible ? 1 : 0 }}
          src={liveThumbUrl(id)}
          // Muted is not a preference here: a hover is not a user gesture, and
          // every browser refuses to autoplay sound without one.
          muted
          autoPlay
          playsInline
          preload="auto"
          onPlaying={() => setVisible(true)}
          // Once through, then back to the photo — the same as pressing a Live
          // Photo on the phone and letting go.
          onEnded={() => setVisible(false)}
          onError={() => setVisible(false)}
        />
      ) : null}
    </span>
  );
}

/** Apple's concentric rings, the mark everyone already reads as "Live". */
function LiveGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">
      <circle cx="12" cy="12" r="3.1" fill="currentColor" />
      <circle cx="12" cy="12" r="6.2" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <circle
        cx="12"
        cy="12"
        r="9.3"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeDasharray="2.4 2.6"
      />
    </svg>
  );
}

function PlayGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
      <path d="M8 5v14l11-7z" fill="currentColor" />
    </svg>
  );
}

function BrokenGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true">
      <path
        d="M12 8v5m0 3.5v.5M10.3 3.9 2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
      />
    </svg>
  );
}
