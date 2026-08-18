"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { ArrowLeft, Lock } from "lucide-react";

import {
  fetchVaultCollections,
  type Album,
  type Bucket,
  type CollectionFilter,
  type TimelineFilter,
} from "@/lib/api";
import { askToUnlock, BUCKET_LABEL, useVault } from "@/hooks/useVault";
import { useTimeline } from "@/hooks/useTimeline";
import { useTrashActions } from "@/hooks/useTrash";
import { useViewer } from "@/hooks/useViewer";
import { categoryLabel } from "./CategoryList";
import { Timeline } from "./Timeline";
import { Viewer } from "./Viewer";

/**
 * A timeline inside a bucket: all of it, or one of its collections.
 *
 * The same grid, the same viewer, the same zoom and the same paging as the
 * library, over a timeline the server computes in memory from decrypted rows —
 * which the client cannot tell and does not need to. That is the point of the
 * vault's endpoints answering in the gallery's own shapes: there is one gallery
 * in this app, and encrypting half the archive did not earn a second one.
 *
 * What is different is the same one thing that is different everywhere in the
 * vault: with no password there is nothing here at all. Not a blurred grid, not
 * a count, not a placeholder tile — the day table this page's geometry is built
 * from is itself behind the lock.
 */
export function VaultTimelineView({
  bucket,
  within,
}: {
  bucket: Bucket;
  /** The collection inside the bucket, or undefined for all of it. */
  within?: CollectionFilter;
}) {
  const vault = useVault();
  const unlocked = vault.status?.unlocked === true;

  // Memoised on its parts rather than built inline: the actions and the
  // timeline both close over this object's identity, and a fresh one every
  // render would rebuild both every render.
  const filter = useMemo<TimelineFilter>(
    () => ({ kind: "vault", bucket, within }),
    [bucket, within?.kind, within?.value],
  );

  useEffect(() => {
    if (vault.ready && !unlocked) askToUnlock(vault.status);
  }, [vault.ready, vault.status, unlocked]);

  const heading = useVaultHeading(bucket, within, unlocked);

  return unlocked ? (
    <Unlocked bucket={bucket} heading={heading} filter={filter} />
  ) : (
    <div className="flex h-dvh flex-col">
      <Header bucket={bucket} heading={heading} />
      <div className="flex flex-1 flex-col items-center justify-center gap-4 px-6 text-center">
        <Lock className="size-6 text-faint" aria-hidden="true" />
        <p className="text-[13px] text-muted-foreground">
          {BUCKET_LABEL[bucket]} is locked.
        </p>
      </div>
    </div>
  );
}

/**
 * The page once the key is in memory.
 *
 * A separate component because useTimeline starts fetching the moment it is
 * mounted, and mounting it against a locked vault would be a day-table request
 * that is going to answer 423 — one wasted round trip per render, and an error
 * banner on a page whose actual problem is a dialog somebody has not typed into
 * yet.
 */
function Unlocked({
  bucket,
  heading,
  filter,
}: {
  bucket: Bucket;
  heading: Heading;
  filter: TimelineFilter;
}) {
  const timeline = useTimeline(filter);
  const { index, open, close, navigate } = useViewer(timeline);
  const actions = useTrashActions(filter, timeline.retry, heading.album);

  return (
    <div className="flex h-dvh flex-col">
      <Header bucket={bucket} heading={heading} total={timeline.total} />
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

function Header({
  bucket,
  heading,
  total,
}: {
  bucket: Bucket;
  heading: Heading;
  total?: number;
}) {
  return (
    <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-2 sm:px-4">
      <Link
        href={`/${bucket}`}
        aria-label={`Back to ${BUCKET_LABEL[bucket]}`}
        className="flex size-9 shrink-0 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-foreground/[0.05] hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
      >
        <ArrowLeft className="size-[18px]" aria-hidden="true" />
      </Link>

      <h1 className="truncate text-[15px] font-semibold tracking-[0.01em]">{heading.title}</h1>
      {total === undefined ? null : (
        <span className="shrink-0 text-[13px] text-faint">{total.toLocaleString()} items</span>
      )}
      {heading.description ? (
        <p className="hidden min-w-0 truncate text-[13px] text-faint sm:block">
          · {heading.description}
        </p>
      ) : null}

      {/* The one thing this header says that the library's does not. A page of
          decrypted thumbnails looks exactly like a page of ordinary ones, and
          it is worth being reminded which of the two is on screen. */}
      <span className="ml-auto hidden shrink-0 items-center gap-1.5 text-[13px] text-faint sm:flex">
        <Lock className="size-3.5" aria-hidden="true" />
        Encrypted · {BUCKET_LABEL[bucket]}
      </span>
    </header>
  );
}

/** A page's heading: what it is called, and what else it has to say. */
interface Heading {
  title: string;
  description?: string;
  /** The album's own name, when this is an album. What a toast calls it. */
  album?: string;
}

/**
 * What to call this page.
 *
 * Two of the three kinds carry their name in the URL. An album carries a uuid,
 * and unlike the library's it cannot be looked up by id either — the album
 * endpoint only knows about albums in the library, and a hidden one is
 * deliberately not there. So the title comes from the bucket's own collections
 * page, which is one request and the same one that drew the tile that was
 * clicked to get here.
 *
 * A heading that says "Album" is a worse page than one that says "Iceland
 * 2025", and a better one than no photographs at all.
 */
function useVaultHeading(
  bucket: Bucket,
  within: CollectionFilter | undefined,
  unlocked: boolean,
): Heading {
  const [album, setAlbum] = useState<Album | null>(null);

  const wanted = within?.kind === "albums" ? within.value : "";

  useEffect(() => {
    setAlbum(null);
    if (!wanted || !unlocked) return;
    const abort = new AbortController();
    fetchVaultCollections(bucket, abort.signal)
      .then((data) => setAlbum(data.albums.find((a) => a.id === wanted) ?? null))
      .catch(() => {});
    return () => abort.abort();
  }, [bucket, wanted, unlocked]);

  if (!within) return { title: `All of ${BUCKET_LABEL[bucket]}` };
  switch (within.kind) {
    case "albums":
      return {
        title: album?.title ?? "Album",
        description: album?.description,
        album: album?.title,
      };
    case "people":
      return { title: within.value };
    case "categories":
      return { title: categoryLabel(within.value) };
  }
}
