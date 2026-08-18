"use client";

import Link from "next/link";
import {
  Archive,
  Aperture,
  ChevronRight,
  Clapperboard,
  Heart,
  Layers,
  RectangleHorizontal,
  Smartphone,
  Sun,
  Timer,
  Video,
  type LucideIcon,
} from "lucide-react";

import type { Category } from "@/lib/api";
import { BASE_THUMB_SIZE } from "@/lib/layout";
import { Cover } from "./Cover";

/**
 * What each category key is called and drawn as.
 *
 * The server decides which categories exist and sends only their keys, because
 * what a category *is* is a predicate over the library and belongs beside the
 * query. What it is called is a UI decision and belongs here — and a key with
 * no entry falls back to something legible rather than rendering nothing, so
 * adding a category to the server does not require a matching release here.
 */
const LOOK: Record<string, { label: string; Icon: LucideIcon }> = {
  videos: { label: "Videos", Icon: Video },
  favorites: { label: "Favorites", Icon: Heart },
  live: { label: "Live Photos", Icon: Aperture },
  screenshots: { label: "Screenshots", Icon: Smartphone },
  panoramas: { label: "Panoramas", Icon: RectangleHorizontal },
  timelapse: { label: "Time-lapse", Icon: Timer },
  cinematic: { label: "Cinematic", Icon: Clapperboard },
  hdr: { label: "HDR", Icon: Sun },
  // Drawn under Other rather than in this list — see OtherList — but its page
  // is still /collections/categories/archived, so the label still lives here.
  archived: { label: "Archive", Icon: Archive },
};

function look(key: string) {
  return LOOK[key] ?? { label: key, Icon: Layers };
}

/**
 * The named slices of a scope, as rows rather than as a grid.
 *
 * @param basePath where a row leads. The library's categories live under
 * /collections; a bucket's live under /archive or /hidden, over exactly the same
 * keys — a hidden screenshot is a screenshot — which is the whole reason this
 * takes a prefix rather than growing a second component.
 */
export function CategoryList({
  categories,
  basePath = "/collections/categories",
}: {
  categories: Category[];
  basePath?: string;
}) {
  return (
    <ul className="overflow-hidden rounded-xl border bg-card">
      {categories.map((category, i) => {
        const { label, Icon } = look(category.key);
        return (
          <li key={category.key}>
            <Link
              href={`${basePath}/${category.key}`}
              className={
                "flex h-16 items-center gap-3.5 px-3.5 transition-colors hover:bg-foreground/[0.04] focus-visible:outline-2 -outline-offset-2 focus-visible:outline-ring" +
                (i > 0 ? " border-t" : "")
              }
            >
              <div className="relative size-11 shrink-0">
                <Cover
                  id={category.cover_id}
                  size={BASE_THUMB_SIZE}
                  className="size-11 rounded-lg"
                />
                {/* The icon is what makes the row scannable; the photo behind it
                    is what makes the list look like the library rather than a
                    settings screen. Dimming it is what keeps both readable. */}
                <span className="absolute inset-0 flex items-center justify-center rounded-lg bg-background/55">
                  <Icon className="size-[18px] text-foreground" aria-hidden="true" />
                </span>
              </div>

              <span className="flex-1 truncate text-sm font-medium">{label}</span>
              <span className="text-[13px] text-faint">{category.count.toLocaleString()}</span>
              <ChevronRight className="size-4 shrink-0 text-faint" aria-hidden="true" />
            </Link>
          </li>
        );
      })}
    </ul>
  );
}

/** The label a category's own page puts in its heading. */
export const categoryLabel = (key: string) => look(key).label;
