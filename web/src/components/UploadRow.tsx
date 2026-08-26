"use client";

import { memo } from "react";
import {
  Ban,
  Check,
  CircleCheck,
  Clock,
  Copy,
  Eye,
  Film,
  Image as ImageIcon,
  Loader2,
  TriangleAlert,
  X,
} from "lucide-react";

import { formatBytes } from "@/lib/format";
import { labelFor } from "@/lib/upload";
import { cn } from "@/lib/utils";
import type { ItemStatus, UploadItem } from "@/hooks/useUpload";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";

/**
 * How each state looks, in one table.
 *
 * `tone` is the whole of the visual difference between a row that failed and a
 * row that is merely a duplicate, and the two are deliberately not the same
 * colour. A rejection is a mistake — the wrong file, a broken copy — and it is
 * drawn in the colour the rest of the app uses for things that are broken. A
 * duplicate is the archive working: it already has the photograph, nothing is
 * wrong, and nothing needs fixing. Drawing it in red would train somebody to
 * ignore red.
 */
const LOOK: Record<ItemStatus, { label: string; tone: Tone; Icon: typeof Check; spin?: boolean }> =
  {
    checking: { label: "Reading", tone: "quiet", Icon: Loader2, spin: true },
    ready: { label: "Ready", tone: "quiet", Icon: Check },
    rejected: { label: "Rejected", tone: "bad", Icon: Ban },
    duplicate: { label: "Duplicate", tone: "warn", Icon: Copy },
    queued: { label: "Waiting", tone: "quiet", Icon: Clock },
    sending: { label: "Uploading", tone: "live", Icon: Loader2, spin: true },
    stored: { label: "Uploaded", tone: "live", Icon: CircleCheck },
    failed: { label: "Failed", tone: "bad", Icon: TriangleAlert },
  };

type Tone = "quiet" | "live" | "warn" | "bad";

const TEXT: Record<Tone, string> = {
  quiet: "text-muted-foreground",
  live: "text-primary",
  warn: "text-warning",
  bad: "text-destructive",
};

/** The stripe down the left edge, which is what makes a long list scannable. */
const EDGE: Record<Tone, string> = {
  quiet: "before:bg-transparent",
  live: "before:bg-primary",
  warn: "before:bg-warning",
  bad: "before:bg-destructive",
};

/**
 * One file, and everything the page knows about it.
 *
 * There is no thumbnail here on purpose. A batch is routinely two hundred
 * files, and a real preview of each means the browser decoding two hundred
 * full-resolution photographs to draw them at forty pixels — several gigabytes
 * of bitmap for a row of icons. Half of them could not be decoded at all: a
 * browser has no HEIC or DNG decoder, and those are most of what a phone
 * produces. The eye opens the one file somebody actually wants to look at, and
 * says plainly when it cannot.
 */
export const UploadRow = memo(function UploadRow({
  item,
  onPreview,
  onRemove,
}: {
  item: UploadItem;
  onPreview: (item: UploadItem) => void;
  onRemove: (key: string) => void;
}) {
  const look = LOOK[item.status];
  const busy = item.status === "checking" || item.status === "sending";
  const share = item.file.size > 0 ? (item.progress / item.file.size) * 100 : 0;

  return (
    <li
      className={cn(
        "group relative flex items-center gap-3 overflow-hidden rounded-xl border bg-card/50 py-2 pr-1.5 pl-3 transition-colors",
        "before:absolute before:inset-y-0 before:left-0 before:w-[3px] before:transition-colors",
        EDGE[look.tone],
        item.status === "stored" && "bg-primary/[0.04]",
      )}
    >
      <span
        className={cn(
          "flex size-9 shrink-0 items-center justify-center rounded-lg bg-tile",
          TEXT[look.tone],
        )}
      >
        {item.kind === "video" ? (
          <Film className="size-4.5" aria-hidden="true" />
        ) : (
          <ImageIcon className="size-4.5" aria-hidden="true" />
        )}
      </span>

      <div className="min-w-0 flex-1">
        <div className="flex items-baseline gap-2">
          <p className="truncate text-[13px] font-medium" title={item.file.name}>
            {item.file.name}
          </p>
          <span
            className={cn(
              "ml-auto flex shrink-0 items-center gap-1 text-[12px] font-medium",
              TEXT[look.tone],
            )}
          >
            <look.Icon
              className={cn("size-3.5", look.spin && "animate-spin")}
              aria-hidden="true"
            />
            {item.status === "sending" ? `${Math.round(share)}%` : look.label}
          </span>
        </div>

        <p
          className={cn(
            "mt-0.5 truncate text-[12px]",
            item.reason ? TEXT[look.tone] : "text-faint",
          )}
          title={item.reason}
        >
          {item.reason ?? facts(item)}
        </p>

        {busy ? (
          <Progress
            value={share}
            aria-label={`${look.label} ${item.file.name}`}
            className="mt-1.5"
          />
        ) : null}
      </div>

      <div className="flex shrink-0 items-center gap-0.5">
        <Button
          variant="ghost"
          size="icon-sm"
          onClick={() => onPreview(item)}
          aria-label={`Preview ${item.file.name}`}
        >
          <Eye aria-hidden="true" />
        </Button>
        <Button
          variant="ghost"
          size="icon-sm"
          onClick={() => onRemove(item.key)}
          aria-label={`Remove ${item.file.name} from this batch`}
          className="text-muted-foreground hover:text-destructive"
        >
          <X aria-hidden="true" />
        </Button>
      </div>
    </li>
  );
});

/** The quiet line, for a row with nothing to complain about. */
function facts(item: UploadItem): string {
  const parts = [labelFor(item.file.name), formatBytes(item.file.size)];
  if (item.file.lastModified) {
    parts.push(
      new Date(item.file.lastModified).toLocaleDateString(undefined, {
        day: "numeric",
        month: "short",
        year: "numeric",
      }),
    );
  }
  return parts.filter(Boolean).join(" · ");
}
