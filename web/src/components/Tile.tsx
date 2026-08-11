"use client";

import { useState } from "react";

import { thumbUrl, type TimelineItem } from "@/lib/api";
import { formatDuration } from "@/lib/format";
import { cn } from "@/lib/utils";

interface Props {
  item: TimelineItem;
  size: number;
  onOpen: (id: string) => void;
}

export function Tile({ item, size, onOpen }: Props) {
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
      className="group relative block flex-none overflow-hidden rounded-md bg-tile focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
      style={{ width: size, height: size }}
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

      {item.kind === "video" ? (
        <span className="absolute right-1.5 bottom-[5px] flex items-center gap-[3px] text-[11px] tabular-nums text-white [text-shadow:0_1px_3px_rgb(0_0_0/0.7)]">
          <PlayGlyph />
          {item.duration ? <span>{formatDuration(item.duration)}</span> : null}
        </span>
      ) : null}
    </button>
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
