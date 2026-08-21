"use client";

import { useEffect, useState } from "react";
import { Copy, Film, Images, RefreshCw, Server, Tags } from "lucide-react";

import {
  fetchMergeCounts,
  fetchTagCounts,
  type MergeCounts,
  type Status as ServerStatus,
  type TagCounts,
} from "@/lib/api";
import { formatSince } from "@/lib/format";
import { cn } from "@/lib/utils";
import { useStatus } from "@/hooks/useStatus";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { HoverCard, HoverCardContent, HoverCardTrigger } from "@/components/ui/hover-card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusCard, StatusFigure } from "./StatusCard";
import { StatusIssues } from "./StatusIssues";
import { StorageCard } from "./StorageCard";

/**
 * What the server is doing, and what is wrong with it.
 *
 * The page is in two halves and they are ordered by how often the answer is
 * interesting. The row of cards is the glance — how much is here, how much room
 * is left, is anything stuck — and it is the same four shapes every time so
 * that a change in one of them is visible without reading any of them. Below it
 * is the part that is usually empty and occasionally the only reason anybody
 * opened the page.
 */
export function Status() {
  const { status, error, readAt, refresh } = useStatus();
  const merges = useMergeCounts();
  const tags = useTagCounts();

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-4">
        <h1 className="text-[15px] font-semibold tracking-[0.01em]">Status</h1>

        {error ? (
          <Badge variant="outline" className="border-destructive/40 text-destructive">
            {status ? "Lost contact with the server" : "Server unreachable"}
          </Badge>
        ) : null}

        <div className="ml-auto flex items-center gap-2">
          {readAt ? (
            <span className="text-[12px] text-faint max-sm:sr-only">
              Updated {formatSince(readAt.toISOString())}
            </span>
          ) : null}
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={() => void refresh()}
            aria-label="Refresh now"
          >
            <RefreshCw aria-hidden="true" />
          </Button>
        </div>
      </header>

      <div className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-4 pb-28">
        <div className="mx-auto flex max-w-5xl flex-col gap-6 pt-4">
          {!status ? (
            <Loading error={error} />
          ) : (
            <>
              <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                <LibraryCard status={status} />
                <StorageCard storage={status.storage} />
                <QueueCard status={status} />

                {/* The two review cards, drawn only when there is something to
                    review. Everything above is a fact about the archive and is
                    worth stating at any value, including zero; these two are
                    work waiting to be done, and a card that says there is none
                    is a row of pixels charging rent. */}
                {merges && merges.pending_duplicates > 0 ? (
                  <StatusCard href="/merge" Icon={Copy} title="Duplicates">
                    <StatusFigure
                      value={merges.pending_duplicates.toLocaleString()}
                      unit={merges.pending_duplicates === 1 ? "group" : "groups"}
                      note={`${merges.duplicate_items.toLocaleString()} photos to look through`}
                    />
                  </StatusCard>
                ) : null}

                {/* The vocabulary, and it is the one card here that is a
                    to-do list rather than a reading. Drawn whenever the
                    captioner has written anything, because there is always
                    something to do with three thousand free-form words — see
                    tagNote for what. */}
                {tags && tags.vocabulary > 0 ? (
                  <StatusCard href="/tags" Icon={Tags} title="Tags">
                    <StatusFigure
                      value={tags.vocabulary.toLocaleString()}
                      unit={tags.vocabulary === 1 ? "word" : "words"}
                      note={tagNote(tags)}
                    />
                  </StatusCard>
                ) : null}

                {merges &&
                merges.pending_segments + merges.merged_segments + merges.failed_segments >
                  0 ? (
                  <StatusCard href="/merge" Icon={Film} title="Joined recordings">
                    <StatusFigure
                      value={merges.merged_segments.toLocaleString()}
                      unit={merges.merged_segments === 1 ? "video" : "videos"}
                      note={joinedNote(merges)}
                    />
                  </StatusCard>
                ) : null}
              </div>

              <StatusIssues status={status} />
            </>
          )}
        </div>
      </div>
    </div>
  );
}

/**
 * The line under the joined recordings figure.
 *
 * Ordered by what is worth interrupting somebody for. A join that gave up waits
 * forever and is the only one of these that needs a person; one still being
 * joined comes right on its own within the minute; and with neither
 * outstanding there is nothing to say but what the number means.
 *
 * The figure above it counts only what has not been approved, so a card that
 * says 0 videos and nothing else has gone as far as it can and is about to
 * disappear.
 */
function joinedNote(merges: MergeCounts) {
  if (merges.failed_segments > 0) {
    return (
      <span className="text-destructive">
        {merges.failed_segments.toLocaleString()} failed to join
      </span>
    );
  }
  if (merges.pending_segments > 0) {
    return `${merges.pending_segments.toLocaleString()} still being joined`;
  }
  return "Snapchat clips put back together";
}

/**
 * The line under the vocabulary figure: the next thing to do to it.
 *
 * One stage at a time, in the order the cleanup actually runs, because a card
 * that listed all four numbers would be a status report on a job nobody has
 * started. The words are judged, the verdicts are read, what survives is
 * compared, and then the merges are accepted — and only the first of those that
 * is still outstanding is worth a sentence here.
 */
function tagNote(tags: TagCounts) {
  if (tags.untriaged > 0) {
    return `${tags.untriaged.toLocaleString()} not looked at yet`;
  }
  if (tags.unreviewed > 0) {
    return `${tags.unreviewed.toLocaleString()} verdicts to read`;
  }
  if (tags.unembedded > 0) {
    return `${tags.unembedded.toLocaleString()} still to compare`;
  }
  if (tags.suggestions && tags.suggestions > 0) {
    return `${tags.suggestions.toLocaleString()} ${tags.suggestions === 1 ? "merge" : "merges"} suggested`;
  }
  if (tags.junk > 0 || tags.folded > 0) {
    return `${tags.junk.toLocaleString()} struck out · ${tags.folded.toLocaleString()} merged`;
  }
  return "What the captioner has called things";
}

function LibraryCard({ status }: { status: ServerStatus }) {
  const { library } = status;
  const counted = library.photos + library.videos;
  const photoShare = counted > 0 ? (library.photos / counted) * 100 : 0;

  return (
    <StatusCard Icon={Images} title="Total items">
      <StatusFigure
        value={library.items.toLocaleString()}
        unit={library.items === 1 ? "item" : "items"}
        note={
          <>
            <span className="tabular-nums">{library.photos.toLocaleString()}</span> photos ·{" "}
            <span className="tabular-nums">{library.videos.toLocaleString()}</span> videos
            {library.trashed > 0 ? (
              <span className="text-faint">
                {" · "}
                <span className="tabular-nums">{library.trashed.toLocaleString()}</span> in the
                trash
              </span>
            ) : null}
          </>
        }
      />
      {/* The same two numbers as a length, which is the form the question
          "how much of this is video" is actually asked in. Decorative in the
          sense that it says nothing new, and worth the four pixels because a
          ratio read from two five-digit numbers is not read at all. */}
      {counted > 0 ? (
        <div
          className="mt-auto flex h-1.5 gap-0.5 overflow-hidden rounded-full"
          aria-hidden="true"
        >
          <span className="bg-chart-1" style={{ width: `${photoShare}%` }} />
          <span className="flex-1 bg-chart-3" />
        </div>
      ) : null}
    </StatusCard>
  );
}

/**
 * The dot, the queue depth, and what the queue is made of.
 *
 * Three states rather than two. "Unhealthy" covers both a server that has
 * stopped doing its job and one that has failed at forty of them, and those
 * want different reactions — the first is an outage, the second is a Saturday
 * afternoon. Amber is the difference.
 */
function QueueCard({ status }: { status: ServerStatus }) {
  const { queue, problems, failures } = status;
  const broken = problems.some((p) => p.severity === "error");
  const wounded = !broken && (failures.length > 0 || problems.length > 0);

  const queued = queue.pending + queue.running;
  const pending = waiting(queue.kinds);

  return (
    <StatusCard
      Icon={Server}
      title="Server"
      action={
        queue.failed > 0 ? (
          <Badge variant="destructive" className="tabular-nums">
            {queue.failed.toLocaleString()} failed
          </Badge>
        ) : null
      }
    >
      <div className="flex items-center gap-2">
        <span className="relative flex size-2.5 items-center justify-center">
          <span
            className={cn(
              "size-2 rounded-full",
              broken ? "bg-destructive" : wounded ? "bg-warning" : "bg-primary",
            )}
          />
          {/* The halo is only ever drawn around the healthy dot, so a page seen
              from the far side of the room reads "fine" without being read. */}
          {!broken && !wounded ? (
            <span className="absolute size-2.5 animate-ping rounded-full bg-primary/40" />
          ) : null}
        </span>
        <span className="text-[13px] font-medium">
          {broken ? "Not doing its job" : wounded ? "Running, with problems" : "Healthy"}
        </span>
      </div>

      {/* The breakdown is a hover only while there is something to break down.
          An empty queue answered with a card that says "nothing" is a control
          that punishes curiosity. */}
      {pending.length === 0 ? (
      <StatusFigure
        value={queued.toLocaleString()}
        unit={queued === 1 ? "item queued" : "items queued"}
        note={
          queued === 0
            ? "Nothing waiting to be processed"
            : `${queue.running.toLocaleString()} being worked on now`
        }
      />
      ) : (
        <HoverCard>
          <HoverCardTrigger
            render={
              <div className="w-fit cursor-default">
      <StatusFigure
        value={queued.toLocaleString()}
        unit={queued === 1 ? "item queued" : "items queued"}
        note={
          queued === 0
            ? "Nothing waiting to be processed"
            : `${queue.running.toLocaleString()} being worked on now`
        }
      />
              </div>
            }
          />
          <HoverCardContent className="w-64" side="bottom">
            <p className="mb-2 text-[13px] font-medium">What is in the queue</p>
            <dl className="flex flex-col gap-1 text-xs">
              {pending.map((kind) => (
                <div key={kind.kind} className="flex justify-between gap-4">
                  <dt className="text-muted-foreground">
                    {KIND_LABELS[kind.kind] ?? kind.kind}
                    {kind.running > 0 ? (
                      <span className="text-faint"> · {kind.running} running</span>
                    ) : null}
                  </dt>
                  <dd className="tabular-nums">{kind.total.toLocaleString()}</dd>
                </div>
              ))}
            </dl>
          </HoverCardContent>
        </HoverCard>
      )}

      <p className="mt-auto text-[12px] text-faint">
        {queue.failed > 0
          ? `${queue.failed.toLocaleString()} ${queue.failed === 1 ? "job" : "jobs"} gave up — listed below`
          : broken
            ? "Nothing is draining the queue"
            : "Nothing is stuck"}
      </p>
    </StatusCard>
  );
}

/**
 * The queue by kind, with the two states that mean "not done yet" added
 * together.
 *
 * A job that is running is a job that is in the queue: splitting them makes the
 * same kind of work appear twice on four lines, which reads as four things
 * waiting rather than one. How many are in flight is a detail of that line, not
 * a line of its own.
 */
function waiting(counts: ServerStatus["queue"]["kinds"]) {
  const byKind = new Map<string, { kind: string; total: number; running: number }>();
  for (const count of counts) {
    if (count.state !== "pending" && count.state !== "running") continue;
    const row = byKind.get(count.kind) ?? { kind: count.kind, total: 0, running: 0 };
    row.total += count.count;
    if (count.state === "running") row.running += count.count;
    byKind.set(count.kind, row);
  }
  return [...byKind.values()].sort((a, b) => b.total - a.total);
}

/**
 * What each job kind is for, in the words somebody looking at a queue would
 * use. The wire names are the worker's, and "metadata" is a poor description of
 * the thing the gallery is actually waiting for.
 */
const KIND_LABELS: Record<string, string> = {
  metadata: "Thumbnails and capture times",
  playback: "Video renditions",
  signature: "Duplicate fingerprints",
  merge: "Joining split recordings",
  mlprep: "Preparing images for search",
  vision: "Reading what photos look like",
  ocr: "Reading text in photos",
  describe: "Describing photos in words",
};

function Loading({ error }: { error: string | null }) {
  if (error) {
    return (
      <Card className="items-center gap-2 py-16 text-center">
        <p className="text-sm font-medium text-destructive">The server did not answer</p>
        <p className="max-w-sm text-[13px] text-muted-foreground">{error}</p>
      </Card>
    );
  }
  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {[0, 1, 2].map((i) => (
        <Skeleton key={i} className="h-[124px] rounded-xl" />
      ))}
    </div>
  );
}

/**
 * The duplicate and split-recording counts, which live behind their own
 * endpoint because the review page is built on it.
 *
 * Read once. Both numbers move only when a scan runs or somebody resolves a
 * group, neither of which happens while this page is open.
 */
function useTagCounts(): TagCounts | null {
  const [counts, setCounts] = useState<TagCounts | null>(null);

  useEffect(() => {
    const abort = new AbortController();
    fetchTagCounts(abort.signal)
      .then(setCounts)
      .catch(() => {
        // Same as the merge counts below: the card is conditional anyway, and
        // whatever is wrong with the server has already been said above.
      });
    return () => abort.abort();
  }, []);

  return counts;
}

function useMergeCounts(): MergeCounts | null {
  const [counts, setCounts] = useState<MergeCounts | null>(null);

  useEffect(() => {
    const abort = new AbortController();
    fetchMergeCounts(abort.signal)
      .then(setCounts)
      .catch(() => {
        // The two cards this feeds are conditional anyway, and the status
        // endpoint has already said whatever is wrong with the server.
      });
    return () => abort.abort();
  }, []);

  return counts;
}
