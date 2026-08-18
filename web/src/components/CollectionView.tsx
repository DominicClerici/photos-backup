"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { ArrowLeft } from "lucide-react";

import { fetchAlbum, type Album, type CollectionFilter } from "@/lib/api";
import { useTimeline } from "@/hooks/useTimeline";
import { useTrashActions } from "@/hooks/useTrash";
import { useViewer } from "@/hooks/useViewer";
import { categoryLabel } from "./CategoryList";
import { Timeline } from "./Timeline";
import { Viewer } from "./Viewer";

/**
 * One collection, browsed exactly as the library is.
 *
 * The grid, the viewer, the zoom and the paging are the gallery's own, given a
 * filtered timeline: a collection is a narrower query, not a different kind of
 * thing, and anything built separately here would be a second grid to keep in
 * step with the first.
 */
export function CollectionView({ filter }: { filter: CollectionFilter }) {
  const timeline = useTimeline(filter);
  const { index, open, close, navigate } = useViewer(timeline);
  const heading = useHeading(filter);
  // The filter goes with the actions because a position is a position *in this
  // collection*: index 2 of an album is not index 2 of the library.
  //
  // The name goes with them too, and only because of the toast: "removed from
  // Iceland 2025" needs a word the filter does not carry.
  const actions = useTrashActions(filter, timeline.retry, heading.album);

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

        <h1 className="truncate text-[15px] font-semibold tracking-[0.01em]">{heading.title}</h1>
        <span className="shrink-0 text-[13px] text-faint">
          {timeline.total.toLocaleString()} items
        </span>
        {/* Beside the count rather than under the title: the header is one row
            everywhere else in the app, and a description most albums do not
            have is not a reason to make it two. */}
        {heading.description ? (
          <p className="hidden min-w-0 truncate text-[13px] text-faint sm:block">
            · {heading.description}
          </p>
        ) : null}
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

/**
 * What to call this collection, and what else its heading has to say.
 *
 * Two of the three kinds already carry their name in the URL, and only an album
 * — addressed by uuid — has to be asked about. That lookup is its own endpoint
 * rather than a scan of the collections index, so opening one album does not
 * cost a count of every other one.
 */
function useHeading(filter: CollectionFilter): {
  title: string;
  description?: string;
  /** The album's own name, when this is an album. What a toast calls it. */
  album?: string;
} {
  const [album, setAlbum] = useState<Album | null>(null);

  useEffect(() => {
    setAlbum(null);
    if (filter.kind !== "albums") return;
    const abort = new AbortController();
    fetchAlbum(filter.value, abort.signal)
      .then(setAlbum)
      // A heading that says "Album" is a worse page than one that says
      // "Iceland 2025", and a better one than no photos at all.
      .catch(() => {});
    return () => abort.abort();
  }, [filter.kind, filter.value]);

  switch (filter.kind) {
    case "albums":
      return {
        title: album?.title ?? "Album",
        description: album?.description,
        album: album?.title,
      };
    case "people":
      return { title: filter.value };
    case "categories":
      return { title: categoryLabel(filter.value) };
  }
}
