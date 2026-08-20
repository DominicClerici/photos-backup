"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import {
  CalendarRange,
  Image as ImageIcon,
  Layers,
  Loader2,
  MapPin,
  RotateCcw,
  Search as SearchIcon,
  Sparkles,
  Star,
  Tag,
  User,
  Video,
  X,
} from "lucide-react";

import { useSearch, useSearchActions } from "@/hooks/useSearch";
import { useViewer } from "@/hooks/useViewer";
import { openPalette, usePaletteShortcut } from "@/hooks/usePalette";
import {
  askFor,
  asks,
  chipsOf,
  parsing,
  rememberSearch,
  requestOf,
  withoutChip,
  type Chip as QueryChip,
  type ChipKind,
} from "@/lib/search";
import { Button } from "@/components/ui/button";
import { Kbd } from "@/components/ui/kbd";
import { Timeline } from "./Timeline";
import { Viewer } from "./Viewer";
import { cn } from "@/lib/utils";

/**
 * The search page: a question at the top, how it was read underneath, and the
 * answer in a grid.
 *
 * The grid is the gallery's own with the day headings turned off — ML_IMAGES.md
 * §8 — because relevance is the answer to the question that was asked and
 * chronology is not. Everything else about it is unchanged: the same tiles, the
 * same zoom, the same right-click, the same viewer.
 *
 * The URL is the request. `?q=` alone asks the server to read the sentence;
 * taking a chip off rewrites it into the explicit spelling — `parse=0` beside
 * the fields that survived — which is the only way to say "and *not* the date
 * it found". So a search is linkable, a wrong reading is one click from being
 * corrected, and Back undoes a correction. See lib/search.
 */
export function SearchView() {
  const router = useRouter();
  const params = useSearchParams();

  // A fresh object every render, which is why the hook keys on its spelling
  // rather than its identity. See useSearch.
  const request = useMemo(() => requestOf(new URLSearchParams(params.toString())), [params]);
  const search = useSearch(request);
  const actions = useSearchActions(search);
  const { index, open, close, navigate } = useViewer(search);

  const { query, degraded, total, ready, loading } = search;
  const typed = request.get("q") ?? "";
  const edited = !parsing(request);
  const asked = asks(request);

  const chips = useMemo(() => (query ? chipsOf(query) : []), [query]);

  const run = useCallback(
    (sentence: string) => {
      const trimmed = sentence.trim();
      if (!trimmed) {
        router.push("/search");
        return;
      }
      rememberSearch(trimmed);
      router.push(`/search?${askFor(trimmed)}`);
    },
    [router],
  );

  // Taking a chip off is the reading itself, minus one field, asked again. It
  // pushes rather than replaces so that Back is an undo — a chip removed by
  // mistake is the case this whole mechanism exists for, and it should not cost
  // retyping the sentence.
  const drop = useCallback(
    (chip: QueryChip) => {
      if (!query) return;
      router.push(`/search?${withoutChip(query, chip)}`);
    },
    [router, query],
  );

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex flex-none flex-col gap-2 border-b bg-card px-3 py-2.5 sm:px-4">
        <div className="flex items-center gap-3">
          <QueryBox value={typed} onSubmit={run} />
          <span className="hidden shrink-0 text-[13px] text-faint sm:block">
            {!asked ? null : loading && !ready ? (
              <Loader2 className="size-4 animate-spin" aria-hidden="true" />
            ) : (
              `${total.toLocaleString()} ${total === 1 ? "result" : "results"}`
            )}
          </span>
        </div>

        {chips.length > 0 || edited ? (
          <div className="flex flex-wrap items-center gap-1.5">
            {chips.map((chip) => (
              <Chip key={chip.id} chip={chip} onRemove={() => drop(chip)} />
            ))}
            {/* Only once something has been taken off. Getting back to what the
                server made of the sentence is otherwise unreachable: the
                explicit spelling in the URL has replaced the reading it came
                from, and retyping is not the same thing as undoing. */}
            {edited ? (
              <button
                type="button"
                onClick={() => run(typed)}
                className="flex h-7 items-center gap-1.5 rounded-full px-2.5 text-[12px] text-muted-foreground transition-colors hover:bg-foreground/[0.06] hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
              >
                <RotateCcw className="size-3.5" aria-hidden="true" />
                Reset
              </button>
            ) : null}
          </div>
        ) : null}

        {/* What this search could not do. A note rather than an error: the
            answer below it is real, it was just ranked by words alone. */}
        {degraded ? (
          <p className="text-xs text-warning">{degraded}</p>
        ) : null}
      </header>

      {asked ? (
        <Timeline
          timeline={search}
          actions={actions}
          onOpen={open}
          ranked
          empty={
            <span>
              {chips.length > 0
                ? "Nothing matches all of that. Try taking a chip off."
                : `Nothing in the archive matches “${typed}”.`}
            </span>
          }
        />
      ) : (
        <Blank onOpen={() => openPalette(typed)} />
      )}

      {index >= 0 ? (
        <Viewer
          at={search.at}
          total={search.total}
          index={index}
          onClose={close}
          onNavigate={navigate}
        />
      ) : null}
    </div>
  );
}

/**
 * The question, editable in place.
 *
 * Held locally and pushed on Enter rather than on every keystroke, because
 * committing here is a navigation: it replaces the ranking, drops the
 * selection, and lands in history. The palette is where a question is asked
 * *live*; this is where one already asked is amended.
 */
function QueryBox({ value, onSubmit }: { value: string; onSubmit: (text: string) => void }) {
  const [text, setText] = useState(value);
  const box = useRef<HTMLInputElement>(null);

  // The URL can change without this box being what changed it — the palette, a
  // link, the Back button — and when it does, what is in it is out of date.
  const shown = useRef(value);
  useEffect(() => {
    if (shown.current === value) return;
    shown.current = value;
    setText(value);
  }, [value]);

  return (
    <form
      className="flex h-9 min-w-0 flex-1 items-center gap-2 rounded-lg border border-input/40 bg-input/30 px-3 focus-within:border-ring/60"
      onSubmit={(ev) => {
        ev.preventDefault();
        shown.current = text.trim();
        onSubmit(text);
        box.current?.blur();
      }}
    >
      <SearchIcon className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
      <input
        ref={box}
        value={text}
        onChange={(ev) => setText(ev.target.value)}
        placeholder="Search your photos…"
        aria-label="Search your photos"
        className="min-w-0 flex-1 bg-transparent text-sm outline-none placeholder:text-faint"
      />
      {text ? (
        <button
          type="button"
          aria-label="Clear the search"
          onClick={() => {
            setText("");
            shown.current = "";
            onSubmit("");
            box.current?.focus();
          }}
          className="flex size-5 shrink-0 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-foreground/[0.08] hover:text-foreground"
        >
          <X className="size-3.5" aria-hidden="true" />
        </button>
      ) : null}
    </form>
  );
}

const CHIP_ICON: Record<ChipKind, typeof User> = {
  person: User,
  place: MapPin,
  dates: CalendarRange,
  tag: Tag,
  kind: ImageIcon,
  category: Layers,
  favorites: Star,
  visual: Sparkles,
};

/**
 * One thing the server understood, and the × that says it understood wrong.
 *
 * The phrase is drawn apart from the rest because it is not a filter: every
 * other chip narrows the answer and this one *is* the question — the words that
 * went to the encoder, which are not the words that were typed. Seeing that
 * "photos of my dog at the beach" became "dog at the beach" is how somebody
 * finds out why the ocean came back.
 */
function Chip({ chip, onRemove }: { chip: QueryChip; onRemove: () => void }) {
  const Icon = chip.kind === "kind" && chip.label === "Videos" ? Video : CHIP_ICON[chip.kind];

  return (
    <span
      className={cn(
        "flex h-7 max-w-full items-center gap-1.5 rounded-full border pr-1 pl-2.5 text-[12px]",
        chip.fuzzy
          ? "border-primary/30 bg-primary/10 text-foreground"
          : "border-border bg-background text-foreground",
      )}
    >
      <Icon
        className={cn("size-3.5 shrink-0", chip.fuzzy ? "text-primary" : "text-muted-foreground")}
        aria-hidden="true"
      />
      <span className="truncate">{chip.label}</span>
      <button
        type="button"
        onClick={onRemove}
        aria-label={`Search without ${chip.label}`}
        className="flex size-5 shrink-0 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-foreground/[0.1] hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring"
      >
        <X className="size-3" aria-hidden="true" />
      </button>
    </span>
  );
}

/** The page before anything has been asked of it. */
function Blank({ onOpen }: { onOpen: () => void }) {
  const shortcut = usePaletteShortcut();

  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-4 px-6 text-center">
      <SearchIcon className="size-8 text-faint" aria-hidden="true" />
      <div className="space-y-1">
        <p className="text-sm text-muted-foreground">
          Search by what is in a photograph, not what it was called.
        </p>
        <p className="text-xs text-faint">
          A place, a person, a year, or just what it looked like — “snow”, “Phoenix at the
          beach last summer”, “that blue error screenshot”.
        </p>
      </div>
      <Button type="button" variant="outline" size="sm" onClick={onOpen}>
        Open the search box
        {shortcut ? <Kbd className="ml-1">{shortcut}</Kbd> : null}
      </Button>
    </div>
  );
}
