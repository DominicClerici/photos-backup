"use client";

import { useCallback, useMemo, useState } from "react";
import Link from "next/link";
import {
  ArrowLeft,
  BadgeCheck,
  Check,
  Loader2,
  Merge,
  Search,
  Sparkles,
  Trash2,
  Undo2,
  X,
} from "lucide-react";

import { thumbUrl, type TagCounts, type TagProposal, type TagWord } from "@/lib/api";
import { cn } from "@/lib/utils";
import {
  useMergedTags,
  useTagCounts,
  useTagPasses,
  useTagProposals,
  useTagWords,
} from "@/hooks/useTags";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { HoverCard, HoverCardContent, HoverCardTrigger } from "@/components/ui/hover-card";
import { Input } from "@/components/ui/input";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import { Slider } from "@/components/ui/slider";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";

/**
 * The tag cleanup — ML_IMAGES.md §9.
 *
 * Four lists, and they are two pairs rather than four things. The first pair is
 * the triage: every word the captioner has ever written, split into the ones
 * worth keeping and the ones that are interface text off a screenshot or a
 * vague word about mood. The second pair is the merge: what the surviving words
 * cluster into, and a log of what has already been folded.
 *
 * They are in that order because the first changes the answer to the second. A
 * free-form vocabulary is about a third rubbish, and clustering three thousand
 * words when two thousand are the question is two thousand words of work and a
 * worse result — the junk sits between the real synonyms and takes up their
 * neighbour slots.
 *
 * Nothing on this page destroys anything. `junk` and `canonical_id` are one
 * column each, read at every point of search, so every button here takes effect
 * everywhere at once and every one of them has an opposite.
 */
type Tab = "keep" | "junk" | "suggestions" | "merged";

const TABS: { value: Tab; label: string }[] = [
  { value: "keep", label: "Words" },
  { value: "junk", label: "Junk" },
  { value: "suggestions", label: "Suggestions" },
  { value: "merged", label: "Merged" },
];

/**
 * Where the similarity slider can go, and why it is not 0 to 1.
 *
 * SigLIP-2's text tower puts the median pair of unrelated tags at 0.73 cosine,
 * so the whole useful range is up at the top: 0.80 proposes "man ← woman", and
 * 0.93 proposes "mountains ← mountain, mountain range". Anything below 0.85 is
 * not a looser setting, it is a broken one, so the control does not offer it.
 */
const MIN_SIMILARITY = 0.85;
const MAX_SIMILARITY = 0.99;
const DEFAULT_SIMILARITY = 0.93;

export function TagCleanup() {
  const [tab, setTab] = useState<Tab>("keep");
  const { counts, setCounts, error: countsError } = useTagCounts();
  const apply = useCallback((next: TagCounts) => setCounts(next), [setCounts]);
  const passes = useTagPasses(counts, apply);

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

        <h1 className="truncate text-[15px] font-semibold tracking-[0.01em]">Tags</h1>
        {counts ? (
          <span className="shrink-0 text-[13px] text-faint max-sm:sr-only">
            {counts.vocabulary.toLocaleString()} words the captioner has written
          </span>
        ) : null}

        <div className="ml-auto flex shrink-0 items-center gap-2">
          <ToggleGroup
            value={[tab]}
            onValueChange={(next) => {
              const [chosen] = next as Tab[];
              if (chosen) setTab(chosen);
            }}
            aria-label="What to review"
          >
            {TABS.map(({ value, label }) => (
              <ToggleGroupItem key={value} value={value} className="text-[13px]">
                {label}
                {counts ? (
                  <span className="ml-1 tabular-nums text-faint">{figureFor(value, counts)}</span>
                ) : null}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
        </div>
      </header>

      {/* The passes are the only thing on this page that takes longer than a
          click, and they are a loop of short calls rather than one long one —
          so there is a real number to show, and showing it is what makes a
          two-minute wait legible instead of a frozen button. */}
      {passes.running ? (
        <div className="flex flex-none items-center gap-3 border-b bg-card px-4 py-2">
          <Loader2 className="size-3.5 shrink-0 animate-spin text-muted-foreground" aria-hidden="true" />
          <span className="shrink-0 text-[13px] text-muted-foreground">
            {passes.running === "triage" ? "Reading the vocabulary" : "Comparing the words"}
          </span>
          <Progress
            value={passes.progress.total > 0 ? (passes.progress.done / passes.progress.total) * 100 : 0}
            className="max-w-xs"
          />
          <span className="shrink-0 text-[12px] tabular-nums text-faint">
            {passes.progress.done.toLocaleString()} / {passes.progress.total.toLocaleString()}
          </span>
        </div>
      ) : null}

      <div className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-4 pb-28">
        <div className="mx-auto max-w-5xl pt-4">
          {countsError ? (
            <Notice>
              <span className="text-destructive">{countsError}</span>
            </Notice>
          ) : !counts ? (
            <Skeleton className="h-40 rounded-xl" />
          ) : tab === "suggestions" ? (
            <Suggestions counts={counts} apply={apply} passes={passes} />
          ) : tab === "merged" ? (
            <Merged apply={apply} />
          ) : (
            <Words junk={tab === "junk"} counts={counts} apply={apply} passes={passes} />
          )}
        </div>
      </div>
    </div>
  );
}

/** The number beside a tab name: how much is on that list. */
function figureFor(tab: Tab, counts: TagCounts): string {
  const n =
    tab === "keep"
      ? counts.kept
      : tab === "junk"
        ? counts.junk
        : tab === "suggestions"
          ? (counts.suggestions ?? 0)
          : counts.groups;
  return n.toLocaleString();
}

type Passes = ReturnType<typeof useTagPasses>;

/**
 * One of the two triage lists.
 *
 * A wall of chips rather than rows, because what is being read is words: three
 * thousand of them at a glance, in use order, and the one thing to do to any of
 * them is move it to the other list. Rows would fit forty on a screen and would
 * suggest each one deserves a decision.
 */
function Words({
  junk,
  counts,
  apply,
  passes,
}: {
  junk: boolean;
  counts: TagCounts;
  apply: (counts: TagCounts) => void;
  passes: Passes;
}) {
  const [search, setSearch] = useState("");
  const { words, total, error, loadingMore, more, judge, retry } = useTagWords(junk, search, apply);

  return (
    <div className="flex flex-col gap-4">
      <Card className="gap-3 px-4">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <h2 className="text-sm font-medium">
            {junk ? "Words to throw away" : "Words worth keeping"}
          </h2>
          <p className="min-w-0 flex-1 text-[13px] text-muted-foreground">
            {junk
              ? "These have been struck out: no photograph is findable by them any more. Click one to put it back."
              : "These are what the archive can be searched by. Click one to strike it out."}
          </p>

          <div className="flex shrink-0 items-center gap-2">
            {counts.untriaged > 0 ? (
              <Button size="sm" onClick={passes.analyse} disabled={passes.running !== null}>
                <Sparkles className="size-3.5" aria-hidden="true" />
                Analyse {counts.untriaged.toLocaleString()}
              </Button>
            ) : null}
            {counts.unreviewed > 0 ? (
              <Button
                variant={counts.untriaged > 0 ? "outline" : "default"}
                size="sm"
                onClick={passes.approve}
                disabled={passes.running !== null}
              >
                <BadgeCheck className="size-3.5" aria-hidden="true" />
                Approve {counts.unreviewed.toLocaleString()}
              </Button>
            ) : null}
          </div>
        </div>

        {/* The one sentence that says what approving is for. Without it the
            button reads as a formality, and it is not: it is the difference
            between a verdict a model made and one this archive's owner stands
            behind, and no later pass revisits the second kind. */}
        {counts.unreviewed > 0 ? (
          <p className="text-[13px] text-faint">
            {counts.unreviewed.toLocaleString()} of these are the captioner&apos;s opinion and
            nobody else&apos;s — drawn with a dashed outline. Approving makes them yours, rebuilds
            the search index, and stops later passes from revisiting them.
          </p>
        ) : null}

        <div className="relative max-w-xs">
          <Search
            className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-faint"
            aria-hidden="true"
          />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Find a word"
            className="h-8 pl-8 text-[13px]"
            aria-label="Find a word"
          />
        </div>
      </Card>

      {error ? (
        <Notice>
          <span className="text-destructive">{error}</span>
          <Button variant="outline" size="sm" onClick={retry}>
            Try again
          </Button>
        </Notice>
      ) : null}

      {!words && !error ? (
        <Notice>
          <Loader2 className="size-4 animate-spin" aria-hidden="true" />
          Loading
        </Notice>
      ) : null}

      {words?.length === 0 ? (
        <Notice>
          {search
            ? `No word matching “${search}”.`
            : junk
              ? "Nothing has been struck out. Analyse the vocabulary to have the captioner sort it."
              : "No words yet. The captioner writes these; run the describe pass first."}
        </Notice>
      ) : null}

      {words?.length ? (
        <>
          <div className="flex flex-wrap gap-1.5">
            {words.map((word) => (
              <WordChip key={word.id} word={word} junk={junk} onJudge={() => judge([word.id])} />
            ))}
          </div>
          {words.length < total ? (
            <div>
              <Button variant="outline" size="sm" onClick={more} disabled={loadingMore}>
                {loadingMore ? (
                  <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                ) : null}
                Show {(total - words.length).toLocaleString()} more
              </Button>
            </div>
          ) : null}
        </>
      ) : null}
    </div>
  );
}

/**
 * One word, as a chip that is also a button.
 *
 * The dashed outline is ML_IMAGES.md §11's seam drawn where somebody can see
 * it: a solid chip is a verdict a person gave, a dashed one is a model's guess
 * nobody has confirmed. Without the distinction the two lists read as facts
 * about the archive from the first moment a pass finishes, which is exactly the
 * confident-and-invisible failure that paragraph is about.
 */
function WordChip({
  word,
  junk,
  onJudge,
}: {
  word: TagWord;
  junk: boolean;
  onJudge: () => void;
}) {
  const confirmed = Boolean(word.judged_at);

  return (
    <HoverCard>
      <HoverCardTrigger
        render={
          <button
            type="button"
            onClick={onJudge}
            title={junk ? "Put this word back" : "Strike this word out"}
            className={cn(
              "group flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-[13px] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
              confirmed ? "border-solid" : "border-dashed",
              junk
                ? "border-destructive/30 text-muted-foreground line-through decoration-destructive/40 hover:border-primary/40 hover:text-foreground hover:no-underline"
                : "hover:border-destructive/40 hover:text-destructive",
            )}
          >
            <span className="max-w-[18ch] truncate">{word.name}</span>
            <span className="tabular-nums text-faint">{word.uses.toLocaleString()}</span>
            {junk ? (
              <Undo2 className="size-3 opacity-0 transition-opacity group-hover:opacity-100" aria-hidden="true" />
            ) : (
              <Trash2 className="size-3 opacity-0 transition-opacity group-hover:opacity-100" aria-hidden="true" />
            )}
          </button>
        }
      />
      <HoverCardContent className="w-64" side="bottom">
        <WordEvidence word={word} />
      </HoverCardContent>
    </HoverCard>
  );
}

/**
 * What a word is attached to, which is the only thing that actually answers
 * "should this be here".
 *
 * The photographs are the argument. "casual" reads as a plausible tag until you
 * see the four unrelated pictures it is on, and "screenshot" reads as junk until
 * you see that it is on a hundred and thirty of them.
 */
function WordEvidence({ word }: { word: TagWord }) {
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-baseline gap-2">
        <p className="min-w-0 flex-1 truncate text-[13px] font-medium">{word.name}</p>
        <span className="shrink-0 text-[12px] tabular-nums text-faint">
          {word.uses.toLocaleString()} {word.uses === 1 ? "photo" : "photos"}
        </span>
      </div>

      {word.samples?.length ? (
        <div className="flex gap-1">
          {word.samples.map((id) => (
            <img
              key={id}
              src={thumbUrl(id, 256)}
              alt=""
              loading="lazy"
              decoding="async"
              draggable={false}
              className="block size-16 rounded bg-tile object-cover"
            />
          ))}
        </div>
      ) : (
        <p className="text-[12px] text-faint">No photograph carries this word any more.</p>
      )}

      <p className="text-[12px] text-faint">
        {word.judged_at
          ? "You decided this."
          : word.triaged_at
            ? `The captioner called it ${word.junk ? "junk" : "worth keeping"}${
                word.score !== undefined ? ` (${Math.round(word.score * 100)}% sure)` : ""
              }.`
            : "Nothing has judged this word yet."}
      </p>
    </div>
  );
}

/** The clustering, and the control that decides how close is close enough. */
function Suggestions({
  counts,
  apply,
  passes,
}: {
  counts: TagCounts;
  apply: (counts: TagCounts) => void;
  passes: Passes;
}) {
  const [similarity, setSimilarity] = useState(DEFAULT_SIMILARITY);
  const { groups, unembedded, error, merge, dismiss, retry } = useTagProposals(similarity, apply);

  return (
    <div className="flex flex-col gap-4">
      <Card className="gap-3 px-4">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <h2 className="text-sm font-medium">Words that may be one word</h2>
          <p className="min-w-0 flex-1 text-[13px] text-muted-foreground">
            Grouped by how near they sit to each other in the encoder&apos;s own space. The
            most-used word leads; everything else in the group becomes searchable as it.
          </p>
          {unembedded > 0 ? (
            <Button size="sm" onClick={passes.compare} disabled={passes.running !== null}>
              <Sparkles className="size-3.5" aria-hidden="true" />
              Compare {unembedded.toLocaleString()}
            </Button>
          ) : null}
        </div>

        <div className="flex items-center gap-3">
          <span className="shrink-0 text-[13px] text-muted-foreground">How alike</span>
          <Slider
            value={[similarity]}
            min={MIN_SIMILARITY}
            max={MAX_SIMILARITY}
            step={0.01}
            onValueChange={(next) => setSimilarity(Array.isArray(next) ? next[0] : next)}
            className="max-w-xs"
            aria-label="How alike two words must be"
          />
          <span className="w-10 shrink-0 text-[13px] tabular-nums text-faint">
            {similarity.toFixed(2)}
          </span>
          {/* The slider is a control rather than a constant because the right
              value moves with the vocabulary, and it is cheap because the
              vectors are stored: dragging it is one query, not a re-embedding. */}
          <span className="text-[12px] text-faint max-sm:sr-only">
            lower finds more and is wrong more often
          </span>
        </div>
      </Card>

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
          Clustering
        </Notice>
      ) : null}

      {groups?.length === 0 ? (
        <Notice>
          {unembedded > 0
            ? "None of the words have been compared with each other yet — press Compare."
            : counts.kept === 0
              ? "There are no words to compare. The captioner writes these."
              : "No two words are this alike. Drag the slider down to look further afield."}
        </Notice>
      ) : null}

      <div className="flex flex-col gap-3">
        {groups?.map((group) => (
          <ProposalCard
            key={group.canonical.id}
            group={group}
            onMerge={(canonical, members, rejected) =>
              merge(group, canonical, members, rejected)
            }
            onDismiss={() => dismiss(group)}
          />
        ))}
      </div>
    </div>
  );
}

/**
 * One proposed merge, and the two ways of disagreeing with it.
 *
 * There are two, and they are different: a member can be wrong while the rest of
 * the group is right — "mountain, mountains, and no, not mountaineering" — and
 * that is an untick, not a rejection of the group. Both are recorded, because a
 * disagreement nobody wrote down comes back on the next clustering run.
 *
 * The head is the most-used word and is preselected for the reason the duplicate
 * review preselects its keeper: an archive that says "mountain" a hundred times
 * and "mountain range" nine is telling you which word it speaks. It is a radio
 * rather than a fact, because that rule is wrong in the interesting cases.
 */
function ProposalCard({
  group,
  onMerge,
  onDismiss,
}: {
  group: TagProposal;
  onMerge: (canonical: number, members: number[], rejected: number[]) => void;
  onDismiss: () => void;
}) {
  const all = useMemo(() => [group.canonical, ...group.members], [group]);
  const [head, setHead] = useState(group.canonical.id);
  const [excluded, setExcluded] = useState<Set<number>>(new Set());
  const [busy, setBusy] = useState(false);

  const members = all.filter((w) => w.id !== head && !excluded.has(w.id));
  const rejected = all.filter((w) => w.id !== head && excluded.has(w.id));
  const headWord = all.find((w) => w.id === head) ?? group.canonical;

  return (
    <section className="overflow-hidden rounded-xl border bg-card">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-4 py-3">
        <h3 className="text-sm font-medium">
          {members.length + 1} words become{" "}
          <span className="text-primary">{headWord.name}</span>
        </h3>
        <span className="text-[13px] text-faint">
          {group.uses.toLocaleString()} {group.uses === 1 ? "photo" : "photos"}
        </span>

        <div className="ml-auto flex shrink-0 items-center gap-2">
          <Button variant="ghost" size="sm" onClick={onDismiss} disabled={busy}>
            <X className="size-3.5" aria-hidden="true" />
            Not the same
          </Button>
          <Button
            size="sm"
            disabled={busy || members.length === 0}
            onClick={() => {
              setBusy(true);
              onMerge(head, members.map((w) => w.id), rejected.map((w) => w.id));
            }}
          >
            <Merge className="size-3.5" aria-hidden="true" />
            Merge
          </Button>
        </div>
      </header>

      <div className="grid grid-cols-[repeat(auto-fill,minmax(150px,1fr))] gap-2 p-3">
        {all.map((word) => (
          <WordTile
            key={word.id}
            word={word}
            head={word.id === head}
            included={word.id === head || !excluded.has(word.id)}
            onToggle={() =>
              setExcluded((current) => {
                const next = new Set(current);
                if (next.has(word.id)) next.delete(word.id);
                else next.add(word.id);
                return next;
              })
            }
            onMakeHead={() => {
              setHead(word.id);
              setExcluded((current) => {
                const next = new Set(current);
                next.delete(word.id);
                return next;
              });
            }}
          />
        ))}
      </div>
    </section>
  );
}

/**
 * One word inside a proposal: the word, what it is attached to, and how near it
 * sits to the head.
 *
 * The photographs are inline rather than behind a hover here, because this is
 * the card where somebody is actually deciding. Three of them per word is what
 * separates "doggo means dog" from "doggo is what this model calls a wolf".
 */
function WordTile({
  word,
  head,
  included,
  onToggle,
  onMakeHead,
}: {
  word: TagWord;
  head: boolean;
  included: boolean;
  onToggle: () => void;
  onMakeHead: () => void;
}) {
  return (
    <div
      className={cn(
        "flex flex-col gap-1.5 rounded-lg border p-2 transition-colors",
        head ? "border-primary/50 bg-primary/[0.06]" : included ? "" : "opacity-45",
      )}
    >
      <div className="flex min-w-0 items-center gap-1.5">
        <button
          type="button"
          onClick={head ? undefined : onToggle}
          disabled={head}
          aria-pressed={included}
          title={head ? "Every other word becomes this one" : included ? "Leave this one out" : "Put this one back"}
          className="flex min-w-0 flex-1 items-baseline gap-1.5 text-left focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:cursor-default"
        >
          <span className={cn("min-w-0 truncate text-[13px]", included ? "font-medium" : "line-through")}>
            {word.name}
          </span>
          <span className="shrink-0 text-[11px] tabular-nums text-faint">
            {word.uses.toLocaleString()}
          </span>
        </button>
        {included && !head ? (
          <Check className="size-3.5 shrink-0 text-primary" aria-hidden="true" />
        ) : null}
      </div>

      {head ? (
        <Badge variant="outline" className="w-fit border-primary/40 text-primary">
          Keeps this word
        </Badge>
      ) : (
        <button
          type="button"
          onClick={onMakeHead}
          className="w-fit rounded text-[11px] text-muted-foreground underline decoration-dotted underline-offset-2 transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
        >
          Use this instead
        </button>
      )}

      {word.samples?.length ? (
        <div className="flex gap-1">
          {word.samples.slice(0, 3).map((id) => (
            <img
              key={id}
              src={thumbUrl(id, 256)}
              alt=""
              loading="lazy"
              decoding="async"
              draggable={false}
              className="block size-11 flex-1 rounded bg-tile object-cover"
            />
          ))}
        </div>
      ) : null}

      {/* Only on the members: the head has no distance to itself, and printing
          1.00 there would read as a measurement rather than as a tautology. */}
      {!head && word.similarity ? (
        <p className="text-[11px] tabular-nums text-faint">
          {Math.round(word.similarity * 100)}% alike
        </p>
      ) : null}
    </div>
  );
}

/** What has been folded, and the undo beside every word of it. */
function Merged({ apply }: { apply: (counts: TagCounts) => void }) {
  const { groups, error, unmerge, retry } = useMergedTags(apply);

  return (
    <div className="flex flex-col gap-3">
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
          Nothing has been merged. Accepted suggestions appear here, and every one of them can be
          taken apart again.
        </Notice>
      ) : null}

      {groups?.map((group) => (
        <section key={group.canonical.id} className="overflow-hidden rounded-xl border bg-card">
          <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-4 py-2.5">
            <h3 className="text-sm font-medium">{group.canonical.name}</h3>
            <span className="text-[13px] text-faint">
              {group.uses.toLocaleString()} {group.uses === 1 ? "photo" : "photos"} findable under
              it
            </span>
            <Button
              variant="ghost"
              size="sm"
              className="ml-auto"
              onClick={() => unmerge(group.members.map((m) => m.id))}
            >
              <Undo2 className="size-3.5" aria-hidden="true" />
              Undo all
            </Button>
          </header>

          <div className="flex flex-wrap gap-1.5 p-3">
            {group.members.map((member) => (
              <button
                key={member.id}
                type="button"
                onClick={() => unmerge([member.id])}
                title={`Stop searching “${member.name}” as “${group.canonical.name}”`}
                className="group flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-[13px] transition-colors hover:border-destructive/40 hover:text-destructive focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
              >
                <span className="max-w-[18ch] truncate">{member.name}</span>
                <span className="tabular-nums text-faint">{member.uses.toLocaleString()}</span>
                <Undo2
                  className="size-3 opacity-0 transition-opacity group-hover:opacity-100"
                  aria-hidden="true"
                />
              </button>
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}

function Notice({ children }: { children: React.ReactNode }) {
  return (
    <p className="flex items-center justify-center gap-3 py-16 text-center text-sm text-muted-foreground">
      {children}
    </p>
  );
}
