"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import {
  ArrowLeft,
  Check,
  Heart,
  Loader2,
  Play,
  RefreshCw,
  Undo2,
  X,
} from "lucide-react";

import { thumbUrl, type MergeGroup, type MergeKind, type MergeMember } from "@/lib/api";
import { formatBytes, formatDuration } from "@/lib/format";
import { useMerges } from "@/hooks/useMerges";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";

/**
 * The two halves of this page, and they are not two views of one thing.
 *
 * Duplicates is a queue of questions nobody has answered. Joined recordings is a
 * log of what the worker did without being asked. They share a page because
 * they came out of the same scan and because somebody arriving here wants both
 * answers; they are a switch rather than a filter because the actions differ
 * completely — one keeps a copy, the other undoes a decision already made.
 */
const TABS: { value: MergeKind; label: string }[] = [
  { value: "duplicate", label: "Duplicates" },
  { value: "video-segments", label: "Joined recordings" },
];

/** How many members of a group are drawn before the rest are folded away. */
const VISIBLE = 12;

export function MergeReview() {
  const [tab, setTab] = useState<MergeKind>("duplicate");
  // Pending duplicates are the review; joined recordings are already done, so
  // the only list worth showing of those is what was merged.
  const state = tab === "duplicate" ? "pending" : "merged";
  const { groups, error, scanning, merge, dismiss, unmerge, rescan, retry } = useMerges(tab, state);

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-2 sm:px-4">
        <Link
          href="/overview"
          aria-label="Back to overview"
          className="flex size-9 shrink-0 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-foreground/[0.05] hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
        >
          <ArrowLeft className="size-[18px]" aria-hidden="true" />
        </Link>

        <h1 className="truncate text-[15px] font-semibold tracking-[0.01em]">Merge</h1>
        {groups ? (
          <span className="shrink-0 text-[13px] text-faint">
            {groups.length.toLocaleString()} {groups.length === 1 ? "group" : "groups"}
          </span>
        ) : null}

        <div className="ml-auto flex shrink-0 items-center gap-2">
          <ToggleGroup
            value={[tab]}
            onValueChange={(next) => {
              const [chosen] = next as MergeKind[];
              if (chosen) setTab(chosen);
            }}
            aria-label="What to review"
          >
            {TABS.map(({ value, label }) => (
              <ToggleGroupItem key={value} value={value} className="text-[13px]">
                {label}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>

          <Button variant="outline" size="sm" onClick={rescan} disabled={scanning}>
            {scanning ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RefreshCw className="size-3.5" aria-hidden="true" />
            )}
            <span className="max-sm:sr-only">{scanning ? "Scanning" : "Scan again"}</span>
          </Button>
        </div>
      </header>

      <div className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-4 pb-28">
        <div className="mx-auto max-w-5xl">
          {error ? (
            <Notice>
              <span className="text-destructive">{error}</span>
              <Button variant="outline" size="sm" onClick={retry}>
                Try again
              </Button>
            </Notice>
          ) : null}

          {!groups && !error ? (
            <Notice>
              <Loader2 className="size-4 animate-spin" aria-hidden="true" />
              Loading
            </Notice>
          ) : null}

          {groups?.length === 0 ? (
            <Notice>
              {tab === "duplicate"
                ? "No duplicates waiting. Scan again after an import, or once the library has finished being analysed."
                : "Nothing has been joined yet. Recordings that Snapchat exported in ten-second pieces are put back together automatically."}
            </Notice>
          ) : null}

          <div className="flex flex-col gap-4 pt-4">
            {groups?.map((group) =>
              group.kind === "duplicate" ? (
                <DuplicateCard
                  key={group.id}
                  group={group}
                  onMerge={(keeper) => merge(group.id, keeper)}
                  onDismiss={() => dismiss(group.id)}
                />
              ) : (
                <JoinedCard key={group.id} group={group} onUndo={() => unmerge(group.id)} />
              ),
            )}
          </div>
        </div>
      </div>
    </div>
  );
}

/**
 * One pile of copies, and the choice of which to keep.
 *
 * The server sends the members best first — biggest picture, then largest file,
 * then oldest — so the preselection is the first one and needs no logic here.
 * That is the whole of the recommendation: everything else on the tile is there
 * so somebody can disagree with it on evidence.
 */
function DuplicateCard({
  group,
  onMerge,
  onDismiss,
}: {
  group: MergeGroup;
  onMerge: (keeper: string) => void;
  onDismiss: () => void;
}) {
  const [keeper, setKeeper] = useState(group.members[0]?.id ?? "");
  const [expanded, setExpanded] = useState(false);
  const [busy, setBusy] = useState(false);

  // What merging would free: everything except the copy being kept. Recomputed
  // as the choice changes, because keeping the 4MB copy instead of the 400kB one
  // is the difference somebody is weighing.
  const freed = useMemo(
    () =>
      group.members.reduce((sum, m) => (m.id === keeper ? sum : sum + m.byte_size), 0),
    [group.members, keeper],
  );

  const shown = expanded ? group.members : group.members.slice(0, VISIBLE);
  const hidden = group.members.length - shown.length;

  return (
    <section className="overflow-hidden rounded-xl border bg-card">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-4 py-3">
        <h2 className="text-sm font-medium">
          {group.members.length} copies
        </h2>
        <span className="text-[13px] text-faint">
          {formatBytes(freed)} freed by keeping one
        </span>
        <div className="ml-auto flex shrink-0 items-center gap-2">
          <Button variant="ghost" size="sm" onClick={onDismiss} disabled={busy}>
            <X className="size-3.5" aria-hidden="true" />
            Not duplicates
          </Button>
          <Button
            size="sm"
            disabled={!keeper || busy}
            onClick={() => {
              setBusy(true);
              onMerge(keeper);
            }}
          >
            <Check className="size-3.5" aria-hidden="true" />
            Keep this one
          </Button>
        </div>
      </header>

      <div className="grid grid-cols-[repeat(auto-fill,minmax(140px,1fr))] gap-3 p-4">
        {shown.map((member) => (
          <Candidate
            key={member.id}
            member={member}
            chosen={member.id === keeper}
            recommended={member.id === group.members[0]?.id}
            onChoose={() => setKeeper(member.id)}
          />
        ))}
      </div>

      {/* A burst really is a hundred near-identical photographs, so the fold is
          not a nicety: without it one group would be the whole page. */}
      {hidden > 0 ? (
        <div className="border-t px-4 py-2">
          <Button variant="ghost" size="sm" onClick={() => setExpanded(true)}>
            Show {hidden} more
          </Button>
        </div>
      ) : null}
    </section>
  );
}

/**
 * One candidate, as a radio button that happens to look like a photograph.
 *
 * A button rather than an <input type=radio> with a label: the whole tile is the
 * target, and `aria-pressed` on a button says "this is the one" as clearly as a
 * radio does without a fieldset around every group.
 */
function Candidate({
  member,
  chosen,
  recommended,
  onChoose,
}: {
  member: MergeMember;
  chosen: boolean;
  recommended: boolean;
  onChoose: () => void;
}) {
  const size = member.width && member.height ? `${member.width}×${member.height}` : null;

  return (
    <button
      type="button"
      aria-pressed={chosen}
      onClick={onChoose}
      className={cn(
        "group flex flex-col gap-1.5 rounded-lg p-1.5 text-left transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
        chosen ? "bg-primary/10" : "hover:bg-foreground/[0.04]",
      )}
    >
      <div
        className={cn(
          "relative aspect-square overflow-hidden rounded-md bg-tile ring-2 transition-colors",
          chosen ? "ring-primary" : "ring-transparent",
        )}
      >
        <img
          src={thumbUrl(member.id, 256)}
          alt=""
          loading="lazy"
          decoding="async"
          draggable={false}
          className="block size-full object-cover"
        />

        {chosen ? (
          <span className="absolute top-1.5 right-1.5 flex size-5 items-center justify-center rounded-full bg-primary text-primary-foreground">
            <Check className="size-3.5" aria-hidden="true" />
          </span>
        ) : null}

        {/* White with a shadow rather than a token, matching Tile: these sit
            over an arbitrary photograph, so there is no theme colour that is
            legible against all of them. */}
        {member.kind === "video" ? (
          <span className="absolute right-1.5 bottom-[5px] flex items-center gap-[3px] text-[11px] tabular-nums text-white [text-shadow:0_1px_3px_rgb(0_0_0/0.7)]">
            <Play className="size-2.5 fill-current" aria-hidden="true" />
            {member.duration ? formatDuration(member.duration) : null}
          </span>
        ) : null}

        {member.favorite ? (
          <Heart
            className="absolute bottom-1.5 left-1.5 size-3.5 fill-current text-white [filter:drop-shadow(0_1px_2px_rgb(0_0_0/0.6))]"
            aria-hidden="true"
          />
        ) : null}
      </div>

      <div className="min-w-0 px-0.5 text-[11px] leading-tight">
        <p className="truncate font-medium" title={member.filename}>
          {member.filename}
        </p>
        <p className="text-faint">
          {size ? `${size} · ` : ""}
          {formatBytes(member.byte_size)}
        </p>
        {member.import_source ? (
          <p className="truncate text-faint">{member.import_source}</p>
        ) : null}
        {/* What a discarded copy would take with it, if the merge did not carry
            it across. It does — and this is still the thing most likely to
            change somebody's mind about which copy to keep. */}
        {member.albums?.length ? (
          <p className="truncate text-faint" title={member.albums.join(", ")}>
            {member.albums.length === 1
              ? member.albums[0]
              : `${member.albums.length} albums`}
          </p>
        ) : null}
        {recommended ? <p className="text-primary">Suggested</p> : null}
      </div>
    </button>
  );
}

/**
 * One recording the worker put back together, and the way out of it.
 *
 * There is no choice to make here — it has already happened — so the pieces are
 * shown in order, small, as an account of what was joined rather than as
 * candidates. The only control is the undo, which is the whole reason this tab
 * exists: everything else in this feature asks first.
 */
function JoinedCard({ group, onUndo }: { group: MergeGroup; onUndo: () => void }) {
  const [busy, setBusy] = useState(false);
  const seconds = group.members.reduce((sum, m) => sum + (m.duration ?? 0), 0);

  return (
    <section className="overflow-hidden rounded-xl border bg-card">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-4 py-3">
        <h2 className="text-sm font-medium">
          {group.members.length} clips joined
        </h2>
        {seconds > 0 ? (
          <span className="text-[13px] text-faint">{formatDuration(seconds)} in one video</span>
        ) : null}
        <span className="text-[13px] text-faint">
          {new Date(group.detected_at).toLocaleDateString()}
        </span>

        <div className="ml-auto flex shrink-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              onUndo();
            }}
          >
            <Undo2 className="size-3.5" aria-hidden="true" />
            Split back up
          </Button>
        </div>
      </header>

      <div className="flex items-center gap-3 p-4">
        {group.keeper_asset_id ? (
          <div className="relative aspect-square w-24 shrink-0 overflow-hidden rounded-md bg-tile ring-2 ring-primary">
            <img
              src={thumbUrl(group.keeper_asset_id, 256)}
              alt=""
              loading="lazy"
              decoding="async"
              draggable={false}
              className="block size-full object-cover"
            />
            <span className="absolute bottom-1 left-1.5 text-[11px] font-medium text-white [text-shadow:0_1px_3px_rgb(0_0_0/0.7)]">
              Joined
            </span>
          </div>
        ) : null}

        {/* The pieces, in the order they were concatenated. Smaller than the
            result on purpose: they are what this used to be. */}
        <ol className="flex min-w-0 flex-1 gap-1.5 overflow-x-auto">
          {group.members.map((member) => (
            <li key={member.id} className="shrink-0">
              <div className="relative size-14 overflow-hidden rounded bg-tile opacity-60">
                <img
                  src={thumbUrl(member.id, 96)}
                  alt=""
                  loading="lazy"
                  decoding="async"
                  draggable={false}
                  className="block size-full object-cover"
                />
              </div>
            </li>
          ))}
        </ol>
      </div>
    </section>
  );
}

function Notice({ children }: { children: React.ReactNode }) {
  return (
    <p className="flex items-center justify-center gap-3 py-16 text-center text-sm text-muted-foreground">
      {children}
    </p>
  );
}
