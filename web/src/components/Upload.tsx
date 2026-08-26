"use client";

import { useCallback, useState } from "react";
import Link from "next/link";
import {
  Ban,
  CircleCheck,
  Copy,
  Loader2,
  RotateCcw,
  TriangleAlert,
  Upload as UploadIcon,
} from "lucide-react";

import { formatBytes } from "@/lib/format";
import { cn } from "@/lib/utils";
import { useUpload, type UploadItem } from "@/hooks/useUpload";
import { toast } from "@/components/ui/toast";
import { Button } from "@/components/ui/button";
import { Progress, ProgressLabel, ProgressValue } from "@/components/ui/progress";
import { Separator } from "@/components/ui/separator";
import { UploadDropzone } from "./UploadDropzone";
import { UploadPreview } from "./UploadPreview";
import { UploadRow } from "./UploadRow";

/**
 * Putting photographs into the archive from a browser.
 *
 * The page is one column and reads top to bottom in the order the work happens:
 * where files come in, what is about to happen to them, and then every file
 * with its own answer. Nothing is hidden behind a tab or a disclosure, because
 * the whole question somebody has on this page is "did all of them go in", and
 * that has to be answerable by looking.
 *
 * The two buttons live in the header rather than under the list. A batch of two
 * hundred files is a long scroll, and Upload has to be reachable from the row
 * that made somebody scroll down to check it.
 */
export function Upload() {
  const { items, summary, add, remove, reset, start, cancel } = useUpload();
  const [previewing, setPreviewing] = useState<UploadItem | null>(null);

  const submit = useCallback(() => {
    void start().then(({ stored, failed, duplicate }) => {
      if (stored === 0 && failed === 0 && duplicate === 0) return;
      // One sentence for the whole run, said once. The rows are the detail and
      // they are still on screen; this is for somebody who walked away from a
      // batch of two hundred and wants to know whether to look at them.
      toast.add({
        type: failed > 0 ? "error" : "success",
        title: failed > 0 ? `${failed} of ${stored + failed + duplicate} did not upload` : ranTitle(stored, duplicate),
        description:
          failed > 0
            ? "The rows that failed say why. Upload again to retry just those."
            : "Thumbnails appear in the gallery as the server builds them.",
      });
    });
  }, [start]);

  const preview = useCallback((item: UploadItem) => setPreviewing(item), []);

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-4">
        <h1 className="text-[15px] font-semibold tracking-[0.01em]">Upload</h1>
        {summary.total > 0 ? (
          <span className="text-[12px] text-faint max-sm:sr-only">
            {summary.total === 1 ? "1 file" : `${summary.total.toLocaleString()} files`}
            {summary.sendableBytes > 0 ? ` · ${formatBytes(summary.sendableBytes)} to send` : null}
          </span>
        ) : null}

        <div className="ml-auto flex items-center gap-2">
          {summary.running ? (
            <Button variant="outline" size="lg" onClick={cancel}>
              Cancel
            </Button>
          ) : (
            <Button variant="ghost" size="lg" onClick={reset} disabled={summary.total === 0}>
              <RotateCcw aria-hidden="true" />
              Reset
            </Button>
          )}
          <Button
            size="lg"
            onClick={submit}
            disabled={summary.running || summary.sendable === 0 || summary.checking > 0}
          >
            {summary.running ? (
              <Loader2 className="animate-spin" aria-hidden="true" />
            ) : (
              <UploadIcon aria-hidden="true" />
            )}
            {uploadLabel(summary)}
          </Button>
        </div>
      </header>

      <div className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-4 pb-28">
        <div className="mx-auto flex max-w-3xl flex-col gap-4 pt-4">
          <UploadDropzone onFiles={add} compact={summary.total > 0} />

          {summary.run && summary.progress !== null ? (
            <Progress value={summary.progress * 100} className="rounded-xl border bg-card/50 p-3">
              <ProgressLabel className="text-[13px] font-medium">
                Uploading — {summary.run.done.toLocaleString()} of{" "}
                {summary.run.total.toLocaleString()} done
              </ProgressLabel>
              <ProgressValue className="text-[13px]" />
            </Progress>
          ) : null}

          {summary.total > 0 ? <Tallies summary={summary} /> : null}

          {items.length === 0 ? (
            <Empty />
          ) : (
            <ul className="flex flex-col gap-1.5">
              {items.map((item) => (
                <UploadRow key={item.key} item={item} onPreview={preview} onRemove={remove} />
              ))}
            </ul>
          )}
        </div>
      </div>

      <UploadPreview file={previewing?.file ?? null} onClose={() => setPreviewing(null)} />
    </div>
  );
}

/** What a clean run is called, when some of it turned out to be already here. */
function ranTitle(stored: number, duplicate: number): string {
  const photos = stored === 1 ? "1 photo" : `${stored.toLocaleString()} photos`;
  if (stored === 0) {
    return duplicate === 1
      ? "That photo was already in the archive"
      : `All ${duplicate.toLocaleString()} were already in the archive`;
  }
  if (duplicate === 0) return `${photos} uploaded`;
  return `${photos} uploaded, ${duplicate.toLocaleString()} already here`;
}

/** What the primary button says, which is also what it is about to do. */
function uploadLabel(summary: ReturnType<typeof useUpload>["summary"]): string {
  if (summary.running) return "Uploading";
  if (summary.checking > 0) return "Reading files";
  if (summary.sendable === 0) return "Upload";
  // A run that left failures and nothing new is a retry, and saying so is the
  // difference between "press this again" and "press this again for the same
  // thing that just went wrong".
  if (summary.failed > 0 && summary.ready === 0) {
    return summary.failed === 1 ? "Retry 1 file" : `Retry ${summary.failed} files`;
  }
  return summary.sendable === 1
    ? "Upload 1 file"
    : `Upload ${summary.sendable.toLocaleString()} files`;
}

/**
 * The batch in five numbers.
 *
 * Only the ones that are not zero, so the strip is empty on a clean batch and
 * says exactly one thing when something is wrong with one file. A row of
 * counters that always shows "0 failed" makes the day it says "1 failed" look
 * the same at a glance.
 */
function Tallies({ summary }: { summary: ReturnType<typeof useUpload>["summary"] }) {
  const tallies = [
    { n: summary.checking, label: "reading", tone: "text-muted-foreground", Icon: Loader2 },
    { n: summary.stored, label: "uploaded", tone: "text-primary", Icon: CircleCheck },
    {
      n: summary.duplicate,
      label: summary.duplicate === 1 ? "duplicate" : "duplicates",
      tone: "text-warning",
      Icon: Copy,
    },
    { n: summary.rejected, label: "rejected", tone: "text-destructive", Icon: Ban },
    { n: summary.failed, label: "failed", tone: "text-destructive", Icon: TriangleAlert },
  ].filter((tally) => tally.n > 0);

  if (tallies.length === 0) return null;

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 px-0.5">
      {tallies.map(({ n, label, tone, Icon }, i) => (
        <span key={label} className="flex items-center gap-2">
          {i > 0 ? <Separator orientation="vertical" className="h-3.5" /> : null}
          <span className={cn("flex items-center gap-1.5 text-[12px] font-medium", tone)}>
            <Icon className="size-3.5" aria-hidden="true" />
            {n.toLocaleString()} {label}
          </span>
        </span>
      ))}
    </div>
  );
}

/**
 * What the page says before anything has been dropped on it.
 *
 * Not a second invitation — the box above it is already that — but the two
 * things somebody uploading from a browser needs to know and would otherwise
 * find out the hard way: the archive dedupes by content, and the phone is
 * still the right way to move a whole camera roll.
 */
function Empty() {
  return (
    <div className="flex flex-col gap-2 px-1 pt-2 text-[13px] text-muted-foreground">
      <p>
        Files are read here before anything is sent, so a photograph the archive already holds is
        marked as a duplicate rather than uploaded twice.
      </p>
      <p className="text-faint">
        This is for the handful of files that never went through a phone — a scan, a download, a
        camera card. A whole library belongs in{" "}
        <Link href="/status" className="text-primary underline-offset-4 hover:underline">
          the backup app
        </Link>
        , which resumes and runs unattended.
      </p>
    </div>
  );
}
