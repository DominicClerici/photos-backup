"use client";

import Link from "next/link";
import { ArrowLeft, Trash2 } from "lucide-react";

import type { TimelineFilter } from "@/lib/api";
import { useTimeline } from "@/hooks/useTimeline";
import { useView } from "@/hooks/useView";
import { useTrashActions } from "@/hooks/useTrash";
import { useViewer } from "@/hooks/useViewer";
import { Timeline } from "./Timeline";
import { Viewer } from "./Viewer";

/**
 * A module constant rather than a literal in the body, so its identity is
 * stable across renders — the actions memoise on it, and a fresh object every
 * render would rebuild them every render.
 */
const TRASH: TimelineFilter = { kind: "trash" };

/**
 * Recently Deleted, browsed exactly as the library is.
 *
 * The same grid, the same viewer, the same zoom, the same paging, over the same
 * timeline read with one predicate flipped — see db.TimelineFilter.scope. What
 * differs is only what a selection can do: here the destructive action is final
 * and there is a restore beside it.
 *
 * Which is the whole argument for making the trash a scope rather than a place.
 * A separate "deleted items" screen would be a second grid to keep in step with
 * the first, and it would be the worse of the two, because nobody looks at it
 * often enough to notice it rotting.
 */
export function TrashView() {
  const { view } = useView();
  const timeline = useTimeline(TRASH, view);
  const { index, open, close, navigate } = useViewer(timeline);
  const actions = useTrashActions(TRASH, timeline.retry, undefined, view);

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-2 sm:px-4">
        <Link
          href="/collections"
          aria-label="Back to collections"
          className="flex size-9 shrink-0 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-foreground/[0.05] hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
        >
          <ArrowLeft className="size-[18px]" aria-hidden="true" />
        </Link>

        <h1 className="truncate text-[15px] font-semibold tracking-[0.01em]">
          Recently Deleted
        </h1>
        <span className="shrink-0 text-[13px] text-faint">
          {timeline.total.toLocaleString()} items
        </span>

        {/* The retention is the whole promise this page makes, so it is on the
            page rather than in a tooltip. Below 640px only the count survives. */}
        <span className="ml-auto hidden shrink-0 items-center gap-1.5 text-[13px] text-faint sm:flex">
          <Trash2 className="size-3.5" aria-hidden="true" />
          Deleted for good after 365 days
        </span>
      </header>

      <Timeline timeline={timeline} actions={actions} onOpen={open} />

      {index >= 0 ? (
        <Viewer
          at={timeline.at}
          total={timeline.total}
          index={index}
          onClose={close}
          onNavigate={navigate}
        />
      ) : null}
    </div>
  );
}
