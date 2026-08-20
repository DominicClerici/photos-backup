"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import {
  ArrowLeft,
  BadgeCheck,
  Check,
  Heart,
  Loader2,
  Play,
  RefreshCw,
  RotateCcw,
  TriangleAlert,
  Undo2,
  X,
} from "lucide-react";

import {
  joinPreviewUrl,
  originalUrl,
  playbackUrl,
  thumbUrl,
  type MergeGroup,
  type MergeKind,
  type MergeMember,
} from "@/lib/api";
import { formatBytes, formatDuration, formatSince } from "@/lib/format";
import { useMerges } from "@/hooks/useMerges";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
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
  // Off by default, and the whole point of approving: the joined recordings tab
  // is what is left to look at rather than everything that ever happened.
  const [showApproved, setShowApproved] = useState(false);
  const { groups, error, scanning, merge, dismiss, unmerge, approve, force, rescan, retry } =
    useMerges(tab, showApproved);

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-2 sm:px-4">
        <Link
          href="/status"
          aria-label="Back to status"
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

          {/* Beside the switch rather than inside it, because it is not a third
              thing to look at: it is how much of the second one to show. Only
              on that tab, since nothing else here can be approved. */}
          {tab === "video-segments" ? (
            <Label className="gap-2 text-[13px] font-normal text-muted-foreground">
              <Checkbox
                checked={showApproved}
                onCheckedChange={(next) => setShowApproved(next === true)}
              />
              <span className="max-sm:sr-only">Show approved</span>
            </Label>
          ) : null}

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
                : showApproved
                  ? "Nothing has been joined yet. Recordings that Snapchat exported in ten-second pieces are put back together automatically."
                  : "Nothing left to look at. Recordings you have approved are hidden — tick “Show approved” to see them again."}
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
              ) : group.state === "merged" ? (
                <JoinedCard
                  key={group.id}
                  group={group}
                  onUndo={() => unmerge(group.id)}
                  onApprove={(approved) => approve(group.id, approved)}
                />
              ) : (
                /* Still pending, and on this list at all only because its join
                   gave up — see useMerges, which asks for those separately. */
                <FailedCard
                  key={group.id}
                  group={group}
                  onForce={() => force(group.id)}
                  onDismiss={() => dismiss(group.id)}
                />
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
function JoinedCard({
  group,
  onUndo,
  onApprove,
}: {
  group: MergeGroup;
  onUndo: () => void;
  onApprove: (approved: boolean) => void;
}) {
  const [busy, setBusy] = useState(false);
  const seconds = group.members.reduce((sum, m) => sum + (m.duration ?? 0), 0);
  const approved = Boolean(group.approved_at);

  return (
    <section
      className={cn("overflow-hidden rounded-xl border bg-card", approved && "opacity-70")}
    >
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
        {/* Two badges that mean opposite things, and both are worth a word. The
            first says somebody signed this off; the second says this archive
            built a file it had doubts about, which outlives any approval. */}
        {approved ? (
          <Badge variant="outline" className="border-primary/40 text-primary">
            <BadgeCheck className="size-3" aria-hidden="true" />
            Approved
          </Badge>
        ) : null}
        {group.forced ? (
          <Badge variant="outline" className="border-warning/40 text-warning">
            Saved anyway
          </Badge>
        ) : null}

        <div className="ml-auto flex shrink-0 items-center gap-2">
          {group.keeper_asset_id ? (
            <Watch
              label="Preview merged"
              title={`${group.members.length} clips, joined`}
              description="The recording as it is now in the library, played from the joined file itself."
              src={playbackUrl(group.keeper_asset_id)}
              // The joined original is an H.264 MP4 this server wrote, so it
              // plays in a browser as it stands. That makes it the right
              // fallback for the minutes before the playback rendition of it
              // has been built.
              fallback={originalUrl(group.keeper_asset_id)}
            />
          ) : null}

          <Button
            variant={approved ? "ghost" : "outline"}
            size="sm"
            disabled={busy}
            onClick={() => onApprove(!approved)}
          >
            {approved ? (
              <RotateCcw className="size-3.5" aria-hidden="true" />
            ) : (
              <Check className="size-3.5" aria-hidden="true" />
            )}
            {approved ? "Unapprove" : "Approve"}
          </Button>

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

/**
 * A recording the worker tried to put back together and would not archive.
 *
 * The pieces are still in the library and nothing has been trashed: this is a
 * question rather than a log entry, which is why it looks less like the card
 * above it than its position on the page suggests. The question is always the
 * same one, and it is not one the server can answer — the join came out a
 * different length from the sum of its parts, which is what a dropped part
 * looks like and also what a container that overstates its own length looks
 * like. Watching it is the only way to tell, so the preview is not a
 * convenience here; it is the evidence, and the two buttons beside it are the
 * two answers.
 */
function FailedCard({
  group,
  onForce,
  onDismiss,
}: {
  group: MergeGroup;
  onForce: () => void;
  onDismiss: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const seconds = group.members.reduce((sum, m) => sum + (m.duration ?? 0), 0);

  return (
    <section className="overflow-hidden rounded-xl border bg-card ring-1 ring-destructive/25">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-4 py-3">
        <Badge variant="outline" className="border-destructive/40 text-destructive">
          <TriangleAlert className="size-3" aria-hidden="true" />
          Failed
        </Badge>
        <h2 className="text-sm font-medium">{group.members.length} clips not joined</h2>
        {seconds > 0 ? (
          <span className="text-[13px] text-faint">{formatDuration(seconds)} of video</span>
        ) : null}
        {group.failure ? (
          <span className="text-[13px] text-faint">
            <time dateTime={group.failure.failed_at} title={group.failure.failed_at}>
              {formatSince(group.failure.failed_at)}
            </time>
            {" · "}
            {group.failure.attempts}{" "}
            {group.failure.attempts === 1 ? "attempt" : "attempts"}
          </span>
        ) : null}

        <div className="ml-auto flex shrink-0 items-center gap-2">
          {group.preview ? (
            <Watch
              label="Preview merged"
              title={`${group.members.length} clips, as they would be joined`}
              description="The file the join produced and refused to archive. Nothing is in the library yet — watch it through, and if none of the clips is missing, save it anyway."
              src={joinPreviewUrl(group.id)}
            />
          ) : null}

          <Button
            variant="ghost"
            size="sm"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              onDismiss();
            }}
          >
            <X className="size-3.5" aria-hidden="true" />
            Don&apos;t join
          </Button>

          <Button
            size="sm"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              onForce();
            }}
          >
            <Check className="size-3.5" aria-hidden="true" />
            Save anyway
          </Button>
        </div>
      </header>

      <div className="flex flex-col gap-3 p-4">
        {/* Verbatim, in the same shape the status page shows it. It is the one
            thing on this card that says which of the two failures this is. */}
        <pre className="min-w-0 overflow-auto rounded-md bg-tile/60 px-2.5 py-1.5 font-mono text-[11.5px] leading-relaxed whitespace-pre-wrap text-muted-foreground">
          {group.failure?.error || "The job recorded no error text."}
        </pre>

        <ol className="flex min-w-0 gap-1.5 overflow-x-auto">
          {group.members.map((member) => (
            <li key={member.id} className="shrink-0">
              <div className="relative size-14 overflow-hidden rounded bg-tile">
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

/**
 * A button that plays one video, and the dialog it opens.
 *
 * The <video> is mounted only while the dialog is open. A page of forty joined
 * recordings would otherwise hand the browser forty video elements to
 * preload — and on the failed rows, forty requests for files the server has to
 * read off the archive drive.
 */
function Watch({
  label,
  title,
  description,
  src,
  fallback,
}: {
  label: string;
  title: string;
  description: string;
  src: string;
  fallback?: string;
}) {
  const [open, setOpen] = useState(false);

  return (
    <>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        <Play className="size-3.5 fill-current" aria-hidden="true" />
        <span className="max-sm:sr-only">{label}</span>
      </Button>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>
          {open ? <Player src={src} fallback={fallback} /> : null}
        </DialogContent>
      </Dialog>
    </>
  );
}

function Player({ src, fallback }: { src: string; fallback?: string }) {
  const [current, setCurrent] = useState(src);
  const [broken, setBroken] = useState(false);

  if (broken) {
    return (
      <p className="rounded-lg bg-tile/60 px-3 py-8 text-center text-[13px] text-muted-foreground">
        This video could not be played. The file may still be being built, or it may not be
        there at all.
      </p>
    );
  }

  return (
    // eslint-disable-next-line jsx-a11y/media-has-caption -- a Snapchat memory
    // from 2018 has no captions to offer, and inventing an empty track would
    // say it does.
    <video
      key={current}
      src={current}
      controls
      autoPlay
      playsInline
      className="max-h-[70dvh] w-full rounded-lg bg-black"
      onError={() => {
        if (fallback && current !== fallback) {
          setCurrent(fallback);
          return;
        }
        setBroken(true);
      }}
    />
  );
}

function Notice({ children }: { children: React.ReactNode }) {
  return (
    <p className="flex items-center justify-center gap-3 py-16 text-center text-sm text-muted-foreground">
      {children}
    </p>
  );
}
