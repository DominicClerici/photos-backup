"use client";

import { useEffect, useRef, useState } from "react";
import { Check } from "lucide-react";

import { useLiveFade } from "@/hooks/useLiveFade";
import { liveThumbUrl, thumbUrl, type TimelineItem } from "@/lib/api";
import { formatDuration } from "@/lib/format";
import { BASE_THUMB_SIZE, thumbSizeFallbacks, type ThumbSize } from "@/lib/layout";
import { cn } from "@/lib/utils";

/**
 * How long a pointer has to rest on a tile before its Live Photo starts.
 *
 * Sweeping the mouse across the grid crosses a dozen tiles in that time, and
 * without the delay every one of them would start pulling down a video for an
 * animation nobody was going to see.
 */
const HOVER_DELAY_MS = 120;

/**
 * `MediaError.MEDIA_ERR_SRC_NOT_SUPPORTED`, which is what a 404 looks like from
 * inside a <video>: the response arrived, and it was not something to play.
 */
const SRC_NOT_SUPPORTED = 4;

/**
 * The picture inside a tile's square, which is not quite the square itself.
 *
 * A selected tile draws back from its cell and rounds off, the way a card
 * lifted off a table stops being flush with the ones beside it. The square is
 * still exactly where the grid put it — the shrink happens inside, on an
 * element the zoom loop does not touch, so the two transforms never fight over
 * the same node.
 */
const CHROME =
  "absolute inset-0 overflow-hidden bg-tile transition-[transform,border-radius] duration-150 ease-out motion-reduce:transition-none";
/** Proportional, so the corner looks the same at 64px and at 512px. */
const PICKED = "scale-[0.86] rounded-[7%]";

interface Props {
  item: TimelineItem;
  /**
   * The box the tile is laid out at. During a zoom this is the largest size the
   * tile will reach, and `transform` scales it down from there — resizing the
   * box every frame would mean re-rasterising every thumbnail every frame,
   * while scaling one that is already big enough only costs a composite.
   */
  size: number;
  /** Which stored rendition to draw, chosen from the zoom level by the grid. */
  thumbSize: ThumbSize;
  transform: string;
  /** Where the render loop finds this tile in the day model, without a lookup. */
  day: number;
  offset: number;
  /** Whether the grid is in selection mode; every tile shows its mark if so. */
  selecting: boolean;
  selected: boolean;
  onOpen: (id: string) => void;
}

export function Tile({
  item,
  size,
  thumbSize,
  transform,
  day,
  offset,
  selecting,
  selected,
  onOpen,
}: Props) {
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
  const { src, fallback } = useThumb(item.id, thumbSize, seenState, attempt);
  const label = item.kind === "video" ? "Video" : "Photo";

  return (
    <button
      type="button"
      className="absolute top-0 left-0 block origin-top-left focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
      style={{ width: size, height: size, transform }}
      data-day={day}
      data-tile={offset}
      // A click in selection mode has already been spent on the selection by
      // the time this fires — the grid picks tiles on pointerdown, so that a
      // press that turns into a drag is one gesture rather than a click and
      // then a drag.
      onClick={() => {
        if (!selecting) onOpen(item.id);
      }}
      aria-label={`${label} taken ${new Date(item.taken_at).toLocaleString()}`}
      aria-pressed={selecting ? selected : undefined}
    >
      <span className={cn(CHROME, selected && PICKED)}>
        {attempt ? (
          <img
            className="block size-full object-cover"
            src={src}
            alt=""
            loading="lazy"
            decoding="async"
            draggable={false}
            onError={() => {
              // A size that was never rendered is not a broken photo; only the
              // base rendition failing means there is nothing to draw.
              if (!fallback()) setBroken(true);
            }}
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

        {/* No motion while picking photographs: a sweep across the grid to
            select forty of them would otherwise start forty videos, and the clip
            would play underneath the mark saying the tile had been chosen. */}
        {attempt && item.live === "ready" && !selecting ? (
          <Motion id={item.id} size={thumbSize} />
        ) : null}

        {item.live && item.live !== "failed" ? (
          <span
            className={cn(
              "absolute top-1.5 left-1.5 text-white/90 transition-opacity duration-150 [filter:drop-shadow(0_1px_2px_rgb(0_0_0/0.6))]",
              // The mark stands where this does, and two glyphs in one corner
              // read as neither.
              selecting && "opacity-0",
            )}
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

        {selecting ? <Mark selected={selected} size={size} /> : null}
      </span>
    </button>
  );
}

/**
 * Whether a tile has been picked, drawn in the corner of the picture.
 *
 * Sized from the cell rather than fixed, because the cell runs from 64px to
 * 512px: a mark that reads well on the largest tiles would cover a third of the
 * smallest ones. It sits inside the chrome, so it draws back with the picture
 * rather than hanging off the corner of a square that has shrunk away from it.
 */
function Mark({ selected, size }: { selected: boolean; size: number }) {
  const d = Math.round(Math.min(20, Math.max(13, size * 0.17)));
  const inset = Math.round(d * 0.34);
  return (
    <span
      aria-hidden="true"
      className={cn(
        "absolute flex items-center justify-center rounded-full border-2 transition-colors duration-150 motion-reduce:transition-none",
        selected
          ? "border-primary bg-primary text-primary-foreground"
          : // Unpicked: an empty ring over whatever the photograph happens to
            // be, which is why it carries its own shadow. The tick is already
            // there and merely invisible, so picking the tile fills it in
            // rather than popping a new glyph into the corner.
            "border-white/85 bg-black/25 text-transparent [filter:drop-shadow(0_1px_2px_rgb(0_0_0/0.55))]",
      )}
      style={{ width: d, height: d, top: inset, left: inset }}
    >
      <Check strokeWidth={3.2} style={{ width: d * 0.6, height: d * 0.6 }} />
    </span>
  );
}

/**
 * A square the grid knows the place and size of but not yet the picture for.
 *
 * The grid can draw these before it has asked for a single photograph, because
 * the day table fixes every tile's position up front — which is the whole point
 * of them. Scrolling never runs out of grid to scroll through, the scrollbar
 * never shrinks under the pointer as pages land, and a photo that arrives
 * replaces the exact square that was standing in for it.
 *
 * Deliberately inert. It carries no shimmer: a screenful at the smallest zoom
 * is a couple of thousand of these, and animating all of them would cost more
 * than fetching the photographs they are waiting for. It is also flatter than
 * the shimmer a Tile shows for a thumbnail that is still being generated, which
 * keeps the two states distinguishable — this one is waiting on the network,
 * that one on the worker.
 *
 * It carries the same `data-day`/`data-tile` pair a Tile does, because the zoom
 * loop places every child of the board by those attributes and does not care
 * which kind it is. Selection reads it the same way: the index is the address,
 * so a placeholder for a selected photo is a selected placeholder, and scrolling
 * back to a stretch of the timeline that has been dropped from memory shows the
 * marks before it shows the photographs.
 */
export function Skeleton({
  size,
  transform,
  day,
  offset,
  selecting,
  selected,
}: {
  size: number;
  transform: string;
  day: number;
  offset: number;
  selecting: boolean;
  selected: boolean;
}) {
  return (
    <div
      className="absolute top-0 left-0 origin-top-left"
      style={{ width: size, height: size, transform }}
      data-day={day}
      data-tile={offset}
      aria-hidden="true"
    >
      <span className={cn(CHROME, selected && PICKED)}>
        {selecting ? <Mark selected={selected} size={size} /> : null}
      </span>
    </div>
  );
}

/**
 * The rendition a tile is currently drawing, and the only place a zoom changes
 * which one that is.
 *
 * A new size is loaded out of sight and swapped in once it has arrived, because
 * assigning a fresh `src` to a mounted <img> blanks it until the bytes land —
 * across a screenful of tiles at the end of every zoom, that is a flash of empty
 * grid for a picture that was already there. By the time the swap happens the
 * image is in the browser's cache, so it paints in the same frame.
 *
 * A size that 404s falls back to the base rendition, which every asset has: a
 * library ingested before these sizes existed keeps drawing, at the size it does
 * have, until `photobackup verify --fix` has rendered the rest.
 */
function useThumb(
  id: string,
  size: ThumbSize,
  state: TimelineItem["state"],
  drawing: boolean,
) {
  const [missing, setMissing] = useState<Set<ThumbSize>>(() => new Set());

  // Reset alongside `broken`: a job that has just finished may well have
  // written the size that was not there when this tile first asked for it.
  const [seenState, setSeenState] = useState(state);
  if (seenState !== state) {
    setSeenState(state);
    if (missing.size > 0) setMissing(new Set());
  }

  const target = thumbUrl(id, missing.has(size) ? BASE_THUMB_SIZE : size);
  const [shown, setShown] = useState(target);

  useEffect(() => {
    if (target === shown) return;
    if (!drawing) {
      // Nothing on screen to protect — a tile still waiting on its job, or one
      // that has already given up. Follow the zoom without spending a request
      // on a rendition that provably is not there.
      setShown(target);
      return;
    }
    const probe = new Image();
    probe.onload = () => setShown(target);
    // Leave `shown` alone: what is on screen is a picture of the right photo at
    // the wrong size, which beats a hole in the grid. Recording the gap is what
    // sends the next render to the base rendition instead.
    probe.onerror = () => setMissing((prev) => new Set(prev).add(size));
    probe.src = target;
    return () => {
      probe.onload = null;
      probe.onerror = null;
    };
  }, [drawing, target, shown, size]);

  /**
   * What to do when the <img> itself fails, which is the case the preload above
   * cannot cover: the very first rendition a tile asks for is mounted directly,
   * with nothing yet on screen to keep. Reports whether there is anything left
   * to try — false means the base rendition is the one that failed, and the tile
   * has genuinely lost its picture.
   */
  const fallback = (): boolean => {
    if (shown === thumbUrl(id, BASE_THUMB_SIZE)) return false;
    setMissing((prev) => new Set(prev).add(size));
    setShown(thumbUrl(id, BASE_THUMB_SIZE));
    return true;
  };

  return { src: shown, fallback };
}

/**
 * The three seconds a Live Photo carries, played over its own still.
 *
 * The still stays mounted underneath rather than being swapped out, so the
 * dissolve is between two pictures of the same moment and never lets the tile
 * show through. The video fades in only once it is actually playing, so a slow
 * first frame shows the photo rather than a black square, and it has faded back
 * out by the time the clip stops moving.
 *
 * Hover, not touch: pointerenter fires for a tap too, but pointerleave often
 * does not, which leaves a video playing under a finger that has moved on.
 */
function Motion({ id, size }: { id: string; size: ThumbSize }) {
  const timer = useRef(0);
  // Zero until a hover has rested long enough, then which hover it was: the
  // number is the video's key, so arming again always mounts a fresh element
  // rather than reusing one that is halfway through leaving.
  const [armed, setArmed] = useState(0);
  const [missing, setMissing] = useState<Set<ThumbSize>>(() => new Set());
  const fade = useLiveFade();

  useEffect(() => () => window.clearTimeout(timer.current), []);

  const enter = () => {
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setArmed((take) => take + 1), HOVER_DELAY_MS);
  };

  const leave = () => {
    window.clearTimeout(timer.current);
    const take = armed;
    // Only the hover this was: a pointer that comes back before the fade is out
    // arms a new one, and that clip must not be unmounted by an older leave.
    fade.end(() => setArmed((current) => (current === take ? 0 : current)));
  };

  const playable = thumbSizeFallbacks(size).find((option) => !missing.has(option));

  return (
    // Mounted on hover and unmounted on leave, which is what makes coming back
    // to a tile play the clip again rather than resume it three frames from the
    // end. The unmount waits out the fade: pulling the element mid-dissolve is
    // the cut all over again.
    <span className="absolute inset-0" onMouseEnter={enter} onMouseLeave={leave}>
      {armed && playable !== undefined ? (
        <video
          key={armed}
          // Dissolves in once the clip is really playing, and back out into the
          // still before its last frame.
          {...fade.props}
          className="block size-full object-cover"
          // Matched to the still it plays over, and falling back the same way it
          // does when that size was never rendered — a 404 here is a gap in what
          // is stored, and the wrong size playing beats nothing playing. The
          // still stays underneath throughout, so a size that is on its second
          // or third try simply fades in later.
          src={liveThumbUrl(id, playable)}
          // Muted is not a preference here: a hover is not a user gesture, and
          // every browser refuses to autoplay sound without one.
          muted
          autoPlay
          playsInline
          preload="auto"
          // Once through, then back to the photo — the same as pressing a Live
          // Photo on the phone and letting go. The fade has normally run itself
          // out by now; this covers the clip whose duration never arrived.
          onEnded={() => fade.end()}
          onError={(e) => {
            fade.end();
            // Only a rendition that is not there: a clip that fails partway
            // through has one, and asking a different size to start over under a
            // pointer that has already seen it play is not a recovery.
            if (e.currentTarget.error?.code === SRC_NOT_SUPPORTED) {
              setMissing((prev) => new Set(prev).add(playable));
            }
          }}
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
