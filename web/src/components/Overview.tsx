"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { ChevronRight, Copy, Film, Loader2 } from "lucide-react";

import { fetchMergeCounts, type MergeCounts } from "@/lib/api";
import { JobStatus } from "./JobStatus";

/**
 * The page that says what the archive has noticed about itself.
 *
 * One card per thing that is waiting, and a card is drawn even when its number
 * is zero — an archive with no duplicates left is a fact worth stating, and a
 * page whose contents appear and vanish is a page nobody learns the shape of.
 */
export function Overview() {
  const [counts, setCounts] = useState<MergeCounts | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const abort = new AbortController();
    fetchMergeCounts(abort.signal)
      .then(setCounts)
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        setError(err instanceof Error ? err.message : "could not reach the server");
      });
    return () => abort.abort();
  }, []);

  // How much of the library has been analysed. Until it is all of it, "no
  // duplicates" means "none among the ones looked at so far", and the card has
  // to say which — the two look identical and mean opposite things.
  const coverage = counts?.coverage;
  const analysing = coverage ? coverage.signed < coverage.assets : false;
  const percent =
    coverage && coverage.assets > 0 ? Math.floor((coverage.signed / coverage.assets) * 100) : 0;

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-4">
        <h1 className="text-[15px] font-semibold tracking-[0.01em]">Overview</h1>
        <JobStatus />
      </header>

      <div className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-4 pb-28">
        <div className="mx-auto max-w-3xl pt-4">
          {error ? (
            <p className="py-16 text-center text-sm text-destructive">{error}</p>
          ) : !counts ? (
            <p className="flex items-center justify-center gap-3 py-16 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" aria-hidden="true" />
              Loading
            </p>
          ) : (
            <div className="grid gap-3 sm:grid-cols-2">
              <Card
                href="/merge"
                Icon={Copy}
                title="Duplicates"
                value={counts.pending_duplicates}
                unit={counts.pending_duplicates === 1 ? "group" : "groups"}
                note={
                  analysing
                    ? `${percent}% of the library analysed so far`
                    : counts.pending_duplicates > 0
                      ? `${counts.duplicate_items.toLocaleString()} photos to look through`
                      : counts.merged_duplicates > 0
                        ? `${counts.merged_duplicates.toLocaleString()} merged so far`
                        : "Nothing looks duplicated"
                }
              />

              <Card
                href="/merge"
                Icon={Film}
                title="Joined recordings"
                value={counts.merged_segments}
                unit={counts.merged_segments === 1 ? "video" : "videos"}
                // The one card reporting something that happened without being
                // asked, so it says so rather than looking like a queue.
                note={
                  counts.pending_segments > 0
                    ? `${counts.pending_segments.toLocaleString()} still being joined`
                    : counts.merged_segments > 0
                      ? "Snapchat clips put back together"
                      : "No split recordings found"
                }
              />
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function Card({
  href,
  Icon,
  title,
  value,
  unit,
  note,
}: {
  href: string;
  Icon: typeof Copy;
  title: string;
  value: number;
  unit: string;
  note: string;
}) {
  return (
    <Link
      href={href}
      className="group flex flex-col gap-3 rounded-xl border bg-card p-4 transition-colors hover:bg-foreground/[0.04] focus-visible:outline-2 -outline-offset-2 focus-visible:outline-ring"
    >
      <div className="flex items-center gap-3">
        <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-tile">
          <Icon className="size-[18px]" aria-hidden="true" />
        </span>
        <span className="flex-1 truncate text-sm font-medium">{title}</span>
        <ChevronRight className="size-4 shrink-0 text-faint" aria-hidden="true" />
      </div>

      <div className="flex items-baseline gap-1.5">
        <span className="text-2xl font-semibold tabular-nums">{value.toLocaleString()}</span>
        <span className="text-[13px] text-faint">{unit}</span>
      </div>

      <p className="text-[13px] text-faint">{note}</p>
    </Link>
  );
}
