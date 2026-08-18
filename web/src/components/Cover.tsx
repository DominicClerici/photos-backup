"use client";

import { useState } from "react";
import { ImageOff } from "lucide-react";

import { thumbUrl } from "@/lib/api";
import { BASE_THUMB_SIZE, type ThumbSize } from "@/lib/layout";
import { cn } from "@/lib/utils";

interface Props {
  /** The asset to draw, or nothing — an empty collection has no cover. */
  id?: string;
  /** Which stored rendition to ask for. Falls back to the base size on a 404. */
  size?: ThumbSize;
  className?: string;
}

/**
 * The single thumbnail that stands for a collection.
 *
 * Unlike a tile this has no state to poll and no motion to play: it is one
 * image, chosen by the server, and the worst it does is not exist yet. The
 * larger sizes are the ones a library ingested before they existed does not
 * have until a backfill runs, so a miss falls back to the base size — the only
 * one every asset is guaranteed to have — before giving up on a placeholder.
 */
export function Cover({ id, size = 512, className }: Props) {
  const [fallback, setFallback] = useState(false);
  const [broken, setBroken] = useState(false);

  const box = cn("relative overflow-hidden bg-tile", className);

  if (!id || broken) {
    return (
      <div className={cn(box, "flex items-center justify-center")}>
        <ImageOff className="size-5 text-faint" aria-hidden="true" />
      </div>
    );
  }

  return (
    <div className={box}>
      <img
        className="block size-full object-cover"
        src={thumbUrl(id, fallback ? BASE_THUMB_SIZE : size)}
        alt=""
        loading="lazy"
        decoding="async"
        draggable={false}
        onError={() => (fallback ? setBroken(true) : setFallback(true))}
      />
    </div>
  );
}
