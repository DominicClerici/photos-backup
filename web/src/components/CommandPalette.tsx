"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { Clock, CornerDownLeft, Loader2, Play, Search, X } from "lucide-react";

import { fetchSearch, thumbUrl, type SearchResult } from "@/lib/api";
import { askFor, forgetSearches, recentSearches, rememberSearch } from "@/lib/search";
import { formatDuration } from "@/lib/format";
import { closePalette, onPalette, openPalette } from "@/hooks/usePalette";
import {
  Command,
  CommandDialog,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command";
import { Kbd } from "@/components/ui/kbd";
import { cn } from "@/lib/utils";

/**
 * How long the box has to sit still before the archive is asked about it.
 *
 * A search here is not free the way a filter over a local list is: it is a
 * parse, a text encoding on the GPU, and a fused scan of two rankings. 300ms is
 * about the length of a pause between words, which is the point at which
 * somebody has stopped typing a thought rather than paused inside one.
 */
const DEBOUNCE_MS = 300;

/**
 * How many photographs the palette shows.
 *
 * Few enough that the answer is readable without scrolling and that the
 * question is still the thing on screen. The whole ranking is one keystroke
 * away — the first item on the list is the way to it.
 */
const PREVIEW = 6;

/**
 * The command palette: one box, over everything, for asking the archive
 * something.
 *
 * Mounted once by the root layout and opened from anywhere — ⌘K, ctrl-K, or the
 * Search tab in the bar — for the reason the vault's prompt is: the places that
 * open it cannot reach each other. See useVault and hooks/usePalette.
 *
 * Today it does one thing: it searches photographs, live, and hands the whole
 * ranking to /search. It is a *palette* rather than a search box because of
 * what comes next — a typed sentence is going to be able to name an action as
 * well as a subject, and the difference between the two is a group in this list
 * rather than a second surface. Which is also why the results are grouped when
 * there is only one group: adding "Actions" above "Photos" should be a block of
 * JSX, not a redesign.
 *
 * Nothing here filters locally. `shouldFilter` is off because the list is an
 * answer the server ranked, and cmdk's own fuzzy match over six captions would
 * be a second, worse opinion drawn on top of a fused ranking.
 */
export function CommandPalette() {
  const router = useRouter();

  const [open, setOpen] = useState(false);
  const [text, setText] = useState("");
  /** What was last actually asked, which trails the box by DEBOUNCE_MS. */
  const [asked, setAsked] = useState("");
  const [results, setResults] = useState<SearchResult[]>([]);
  const [total, setTotal] = useState(0);
  const [degraded, setDegraded] = useState("");
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);
  const [recent, setRecent] = useState<string[]>([]);

  useEffect(
    () =>
      onPalette((seed) => {
        if (seed === null) {
          setOpen(false);
          return;
        }
        setOpen(true);
        setText(seed);
        // Read on the way in rather than held: another tab, or the page behind
        // this one, may have asked something since this component mounted.
        setRecent(recentSearches());
      }),
    [],
  );

  // ⌘K and ctrl-K, from anywhere including inside another text box — which is
  // the convention and is why it is not conditional on what has focus. It
  // toggles rather than opens, so the same keystroke is both the way in and the
  // way back out.
  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key !== "k" && ev.key !== "K") return;
      if (!ev.metaKey && !ev.ctrlKey) return;
      ev.preventDefault();
      if (open) closePalette();
      else openPalette();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  // The pause. An emptied box needs no pause — there is nothing to wait for and
  // clearing the list instantly is what makes the backspace feel like an undo.
  const query = text.trim();
  useEffect(() => {
    if (!query) {
      setAsked("");
      return;
    }
    const id = setTimeout(() => setAsked(query), DEBOUNCE_MS);
    return () => clearTimeout(id);
  }, [query]);

  useEffect(() => {
    if (!asked || !open) {
      setResults([]);
      setTotal(0);
      setDegraded("");
      setFailed(null);
      setBusy(false);
      return;
    }

    const abort = new AbortController();
    setBusy(true);
    setFailed(null);

    fetchSearch(askFor(asked), PREVIEW, 0, abort.signal)
      .then((page) => {
        if (abort.signal.aborted) return;
        setResults(page.items ?? []);
        setTotal(page.total);
        setDegraded(page.degraded ?? "");
        setBusy(false);
      })
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        setResults([]);
        setTotal(0);
        setBusy(false);
        setFailed(err instanceof Error ? err.message : "the search could not be run");
      });

    return () => abort.abort();
  }, [asked, open]);

  /** Everything to the whole ranking: remember the question, then go and ask it. */
  const runSearch = useCallback(
    (sentence: string) => {
      const trimmed = sentence.trim();
      if (!trimmed) return;
      rememberSearch(trimmed);
      closePalette();
      router.push(`/search?${askFor(trimmed)}`);
    },
    [router],
  );

  /** One photograph, opened over the results of the question that found it. */
  const openResult = useCallback(
    (id: string) => {
      const params = askFor(query || asked);
      params.set("asset", id);
      rememberSearch(query || asked);
      closePalette();
      router.push(`/search?${params}`);
    },
    [router, query, asked],
  );

  // The list still shows the previous question's answer while the next one is
  // in flight, which is what keeps the palette from flashing empty between
  // keystrokes. `stale` is what greys it while that is true.
  const stale = busy && asked !== query;

  return (
    <CommandDialog
      open={open}
      onOpenChange={(next) => (next ? openPalette() : closePalette())}
      title="Search"
      description="Search your photos, or type a question about them."
      className="top-[18%] sm:max-w-2xl"
    >
      <Command shouldFilter={false} loop>
        <CommandInput
          autoFocus
          value={text}
          onValueChange={setText}
          placeholder="Search your photos…"
        />

        <CommandList className="max-h-[min(24rem,60dvh)]">
          {query ? (
            <>
              <CommandGroup heading="Search">
                <CommandItem value="run" onSelect={() => runSearch(query)}>
                  <Search />
                  <span className="truncate">
                    Search for <span className="font-medium text-foreground">“{query}”</span>
                  </span>
                  <span className="ml-auto flex items-center gap-2 text-xs text-muted-foreground">
                    {busy ? (
                      <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                    ) : total > 0 ? (
                      <span>{total.toLocaleString()} found</span>
                    ) : null}
                    <Kbd>
                      <CornerDownLeft />
                    </Kbd>
                  </span>
                </CommandItem>
              </CommandGroup>

              {results.length > 0 ? (
                <>
                  <CommandSeparator />
                  <CommandGroup heading="Photos">
                    <div className={cn("contents", stale && "opacity-60")}>
                      {results.map((item) => (
                        <ResultItem key={item.id} item={item} onOpen={openResult} />
                      ))}
                    </div>
                  </CommandGroup>
                </>
              ) : null}

              {/* Three different silences, and saying the wrong one sends
                  somebody looking for photographs that are exactly where they
                  left them. */}
              {results.length === 0 && !busy ? (
                <p className="px-3 py-6 text-center text-sm text-muted-foreground">
                  {failed ?? `Nothing in the archive matches “${query}”.`}
                </p>
              ) : null}
            </>
          ) : (
            <Recents
              queries={recent}
              onPick={(entry) => setText(entry)}
              onClear={() => {
                forgetSearches();
                setRecent([]);
              }}
            />
          )}
        </CommandList>

        {/* The one thing the box cannot say for itself: that this answer was
            ranked by words alone because the GPU service is not there. Without
            it, "no results" and "no results, and half the search is down" are
            the same screen. */}
        {degraded && query ? (
          <p className="border-t px-3 py-2 text-xs text-faint">{degraded}</p>
        ) : null}
      </Command>
    </CommandDialog>
  );
}

/**
 * One ranked photograph, as a row.
 *
 * The thumbnail is the base rendition rather than the small one: it is drawn at
 * 40px, every asset is guaranteed to have it, and a 404 falling back through
 * three sizes is worth avoiding on a list that is rebuilt on every keystroke.
 *
 * What it says underneath is the evidence — ML_IMAGES.md §8's "each tile can
 * say why it matched". With a free-form vocabulary that is not a nicety: seeing
 * what the model called a photograph is what makes the tag cleanup possible at
 * all, and it is also the only way to tell a lucky match from a real one.
 */
function ResultItem({
  item,
  onOpen,
}: {
  item: SearchResult;
  onOpen: (id: string) => void;
}) {
  const when = new Date(item.taken_at).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });

  // Only a headline that actually marked something is evidence. ts_headline
  // returns the head of the recognised text when the query matched none of it —
  // which happens whenever the photograph got here on its vector alone — and
  // the first fourteen words of a screenshot are not why it is in these
  // results. The caption is.
  const matched = item.snippet?.includes("[") ? item.snippet : "";

  return (
    <CommandItem value={item.id} onSelect={() => onOpen(item.id)} className="gap-3">
      <span className="relative size-10 shrink-0 overflow-hidden rounded-md bg-tile">
        <img
          src={thumbUrl(item.id)}
          alt=""
          loading="lazy"
          decoding="async"
          draggable={false}
          className="size-full object-cover"
        />
        {item.kind === "video" ? (
          <span className="absolute inset-0 flex items-center justify-center bg-black/25">
            <Play className="size-3.5 fill-white text-white" aria-hidden="true" />
          </span>
        ) : null}
      </span>

      <span className="flex min-w-0 flex-col">
        <span className="truncate text-[13px] text-foreground">
          {matched ? (
            <Highlighted text={matched} />
          ) : (
            item.caption || `${item.kind === "video" ? "Video" : "Photo"} taken ${when}`
          )}
        </span>
        <span className="truncate text-xs text-faint">
          {[
            when,
            item.kind === "video" && item.duration ? formatDuration(item.duration) : null,
            item.tags?.length ? item.tags.slice(0, 3).join(", ") : null,
          ]
            .filter(Boolean)
            .join(" · ")}
        </span>
      </span>
    </CommandItem>
  );
}

/**
 * A line of recognised text with the matched words marked.
 *
 * The server hands these back with the match wrapped in square brackets — see
 * the ts_headline in db.searchSQL — because a headline that arrived as HTML
 * would be a string from the archive rendered as markup, and a photograph of
 * somebody's screen is exactly where a `<script>` would come from.
 */
function Highlighted({ text }: { text: string }) {
  const parts = useMemo(() => text.split(/(\[[^\]]*\])/g).filter(Boolean), [text]);
  return (
    <>
      {parts.map((part, i) =>
        part.startsWith("[") && part.endsWith("]") ? (
          <mark key={i} className="rounded-[3px] bg-primary/20 px-0.5 text-foreground">
            {part.slice(1, -1)}
          </mark>
        ) : (
          <span key={i}>{part}</span>
        ),
      )}
    </>
  );
}

/** What was asked before, which is most of what gets asked again. */
function Recents({
  queries,
  onPick,
  onClear,
}: {
  queries: string[];
  onPick: (query: string) => void;
  onClear: () => void;
}) {
  if (queries.length === 0) {
    return (
      <p className="px-3 py-6 text-center text-sm text-muted-foreground">
        Search your photos by what is in them — a place, a person, a year, or just what it
        looked like.
      </p>
    );
  }

  return (
    <CommandGroup heading="Recent">
      {queries.map((query) => (
        <CommandItem
          key={query}
          value={`recent:${query}`}
          onSelect={() => onPick(query)}
        >
          <Clock />
          <span className="truncate">{query}</span>
        </CommandItem>
      ))}
      <CommandItem value="forget" onSelect={onClear} className="text-muted-foreground">
        <X />
        <span>Clear recent searches</span>
      </CommandItem>
    </CommandGroup>
  );
}
