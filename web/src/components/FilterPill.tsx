"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ArrowDownWideNarrow,
  ArrowUpNarrowWide,
  CalendarArrowDown,
  CalendarArrowUp,
  CalendarSearch,
  Check,
  ChevronLeft,
  FolderMinus,
  Image,
  Images,
  ListFilter,
  Star,
  Video,
} from "lucide-react";

import { useView, type GridView } from "@/hooks/useView";
import type { View } from "@/lib/api";
import type { Day } from "@/lib/layout";
import {
  describeView,
  facetsOn,
  isDefault,
  isFiltered,
  pickSort,
  toggleFacet,
  FACET_LABEL,
  SORT_LABEL,
  type Facet,
  type Facets,
} from "@/lib/view";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "@/components/ui/accordion";
import { Button } from "@/components/ui/button";
import { Calendar } from "@/components/ui/calendar";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Separator } from "@/components/ui/separator";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { cn } from "@/lib/utils";

/**
 * The orders, in the row they are drawn in.
 *
 * The last two are marked rather than explained: ordering by length is a
 * question about videos, and the row says so before it is clicked instead of
 * the filter appearing to move by itself afterwards. See pickSort.
 */
const SORTS: { key: View["sort"]; Icon: typeof CalendarArrowDown; note?: string }[] = [
  { key: "newest", Icon: CalendarArrowDown },
  { key: "oldest", Icon: CalendarArrowUp },
  { key: "longest", Icon: ArrowDownWideNarrow, note: "Videos" },
  { key: "shortest", Icon: ArrowUpNarrowWide, note: "Videos" },
];

const FACET_ICON: Record<Facet, typeof Images> = {
  all: Images,
  photos: Image,
  videos: Video,
  favorites: Star,
  unalbumed: FolderMinus,
};

/**
 * The sort-and-filter control, standing to the left of the selection pill.
 *
 * Same shape and same materials as its neighbour, and mounted by the same bar,
 * for the same reason: these two are the things you do *to* a grid, and the
 * grid is a full-height scroller with no room left in it. It draws nothing at
 * all unless a grid is on screen, which keeps it off the collections and
 * status pages.
 *
 * The panel opens upward and holds three things in the order somebody reaches
 * for them: the order, then what to leave out, then where to go. The filters
 * are folded away because most of the time the answer is "all of them" — and
 * unfolded already when it is not, so a grid that is hiding photographs never
 * hides the reason.
 */
export function FilterPill() {
  const { view, setView, facets, grid } = useView();
  const [open, setOpen] = useState(false);
  // Controlled rather than defaulted, because what it defaults *to* changes:
  // the row opens already unfolded when something is being filtered out, and a
  // default that moves under an uncontrolled component is a component that has
  // stopped agreeing with itself.
  const [filters, setFilters] = useState<string[]>(isFiltered(view) ? ["filters"] : []);
  // Which half of the panel is showing. The calendar replaces the panel rather
  // than opening beside it: a popover hanging off a popover at the bottom edge
  // of the screen is two things to aim at and two things to dismiss, and this
  // way the one Escape everybody expects closes exactly what is in front of them.
  const [picking, setPicking] = useState(false);

  const label = describeView(view);
  const plain = isDefault(view);

  const close = useCallback(() => {
    setOpen(false);
    setPicking(false);
  }, []);

  // A grid that has gone away takes the panel with it, rather than leaving it
  // open over whatever replaced it.
  useEffect(() => {
    if (!grid) close();
  }, [grid, close]);

  if (!grid) return null;

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) setPicking(false);
        // Unfolded on the way in whenever something is being filtered out, so a
        // grid that is hiding photographs never hides the reason. Folding it
        // shut again is somebody's own business and is left alone.
        else if (isFiltered(view) && filters.length === 0) setFilters(["filters"]);
      }}
    >
      <PopoverTrigger
        aria-label="Sort and filter"
        className={cn(
          "pointer-events-auto flex h-13 items-center rounded-full border bg-card/80 px-[17px] shadow-lg backdrop-blur-xl transition-colors duration-200 focus-visible:ring-2 focus-visible:ring-ring/70 focus-visible:outline-none",
          plain ? "text-muted-foreground hover:text-foreground" : "text-foreground",
        )}
      >
        <span className="relative flex size-[18px] shrink-0 items-center justify-center">
          <ListFilter className="size-[18px]" aria-hidden="true" />
          {/* A dot rather than a count: what is on is spelled out beside it, and
              at the widths this pill has to survive the label is the first thing
              to go. */}
          <span
            className={cn(
              "absolute -top-0.5 -right-1 size-1.5 rounded-full bg-primary transition-opacity duration-200",
              plain && "opacity-0",
            )}
            aria-hidden="true"
          />
        </span>

        {/* A column that is 0fr wide with nothing to say and 1fr with something:
            the label keeps its own width throughout and the pill's width follows
            it, which is what makes the growth animate at all — `width: auto`
            does not. The same trick the selection pill uses, deliberately. */}
        <span
          className="grid transition-[grid-template-columns] duration-300 ease-[cubic-bezier(0.22,1,0.36,1)] motion-reduce:transition-none"
          style={{ gridTemplateColumns: plain ? "0fr" : "1fr" }}
        >
          <span className="overflow-hidden">
            {/* No truncation, and it matters: a 1fr track is sized by what is
                in it, and a child that has agreed to be cut short contributes
                nothing — the column collapses and the label never appears.
                describeView is what keeps it short instead. */}
            <span
              className={cn(
                // Gone below 640px, where the tab bar is icons only and the
                // room this would take is the room the tabs are standing in.
                "block pl-2.5 text-[13px] font-medium tracking-[0.01em] whitespace-nowrap transition-opacity duration-200 max-sm:hidden",
                plain ? "opacity-0" : "opacity-100 delay-100",
              )}
            >
              {label}
            </span>
          </span>
        </span>
      </PopoverTrigger>

      <PopoverContent
        side="top"
        align="start"
        sideOffset={10}
        className="w-[19rem] gap-0 border bg-card/95 p-0 shadow-lg backdrop-blur-xl"
      >
        {picking ? (
          <JumpPane grid={grid} onBack={() => setPicking(false)} onDone={close} />
        ) : (
          <MainPane
            view={view}
            facets={facets}
            setView={setView}
            filters={filters}
            setFilters={setFilters}
            onJump={() => {
              // Named as the control that sets it rather than as a rule applied
              // later: a date is a position in a timeline read in date order,
              // and there is no such position in one read by length.
              setView(pickSort(view, "newest", facets));
              setPicking(true);
            }}
          />
        )}
      </PopoverContent>
    </Popover>
  );
}

function MainPane({
  view,
  facets,
  setView,
  filters,
  setFilters,
  onJump,
}: {
  view: View;
  facets: Facets;
  setView: (view: View) => void;
  filters: string[];
  setFilters: (open: string[]) => void;
  onJump: () => void;
}) {
  const on = facetsOn(view);

  // Which toggles this grid still has to offer. A collection is itself a
  // filter, and offering the same one twice is at best noise — see facetsFor.
  const offered: Facet[] = [
    "all",
    ...(facets.media ? (["photos", "videos"] as Facet[]) : []),
    ...(facets.favorites ? (["favorites"] as Facet[]) : []),
    ...(facets.unalbumed ? (["unalbumed"] as Facet[]) : []),
  ];

  return (
    <div className="flex flex-col">
      <div className="flex items-center justify-between px-3 pt-3 pb-1.5">
        <h2 className="text-[11px] font-semibold tracking-[0.06em] text-faint uppercase">
          Sort by
        </h2>
        {isDefault(view) ? null : (
          <button
            type="button"
            className="rounded-md px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/70 focus-visible:outline-none"
            onClick={() => setView({ sort: "newest" })}
          >
            Reset
          </button>
        )}
      </div>

      {/* One value, always set: an order is not something a grid can be without,
          so a click on the one already chosen changes nothing rather than
          leaving the timeline in no order at all. */}
      <ToggleGroup
        value={[view.sort]}
        onValueChange={(next) => {
          const picked = next[0] as View["sort"] | undefined;
          if (picked) setView(pickSort(view, picked, facets));
        }}
        orientation="vertical"
        // Not zero: at zero the group squares off its own items' corners, and
        // these are rows in a menu rather than segments of one control.
        spacing={1}
        className="w-full px-1.5 pb-1.5"
      >
        {SORTS.map(({ key, Icon, note }) => (
          <ToggleGroupItem
            key={key}
            value={key}
            className="h-9 w-full justify-start gap-2.5 rounded-md px-2 text-[13px] font-medium"
          >
            <Icon className="size-4 text-muted-foreground" aria-hidden="true" />
            {SORT_LABEL[key]}
            {note && view.sort !== key ? (
              <span className="ml-auto text-[11px] font-normal text-faint">{note}</span>
            ) : null}
            {view.sort === key ? (
              <Check className="ml-auto size-4 text-primary" aria-hidden="true" />
            ) : null}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>

      <Separator />

      {/* Open already when something is being filtered out, so a grid that is
          hiding photographs never hides the reason it is. */}
      <Accordion
        value={filters}
        onValueChange={(next) => setFilters(next as string[])}
        className="px-3"
      >
        <AccordionItem value="filters" className="border-b-0">
          <AccordionTrigger className="py-2.5 text-[13px] hover:no-underline">
            <span className="flex items-center gap-2">
              Filters
              <span className="text-[11px] font-normal text-faint">
                {on[0] === "all" ? FACET_LABEL.all : on.map((f) => FACET_LABEL[f]).join(" · ")}
              </span>
            </span>
          </AccordionTrigger>
          <AccordionContent className="pb-3">
            {/* Diffed rather than adopted wholesale: the rules about what turns
                what off live in lib/view, and handing this group's own idea of
                the next state straight through would be a second set of them. */}
            <ToggleGroup
              multiple
              value={on}
              onValueChange={(next) => {
                const picked =
                  (next as Facet[]).find((f) => !on.includes(f)) ??
                  on.find((f) => !(next as Facet[]).includes(f));
                if (picked) setView(toggleFacet(view, picked));
              }}
              variant="outline"
              size="sm"
              className="flex-wrap"
            >
              {offered.map((facet) => {
                const Icon = FACET_ICON[facet];
                return (
                  <ToggleGroupItem
                    key={facet}
                    value={facet}
                    className="gap-1.5 rounded-full px-3 aria-pressed:border-primary/60 aria-pressed:bg-primary/15 aria-pressed:text-foreground"
                  >
                    <Icon className="size-3.5" aria-hidden="true" />
                    {FACET_LABEL[facet]}
                  </ToggleGroupItem>
                );
              })}
            </ToggleGroup>
          </AccordionContent>
        </AccordionItem>
      </Accordion>

      <Separator />

      <div className="p-1.5">
        <Button
          type="button"
          variant="ghost"
          className="h-9 w-full justify-start gap-2.5 px-2 text-[13px] font-medium"
          onClick={onJump}
        >
          <CalendarSearch className="size-4 text-muted-foreground" aria-hidden="true" />
          Jump to date
          <ChevronLeft className="ml-auto size-4 rotate-180 text-faint" aria-hidden="true" />
        </Button>
      </div>
    </div>
  );
}

/**
 * The calendar, and the arithmetic that turns a date into a place in the grid.
 *
 * Everything here is answered from the day table the grid was laid out from —
 * no request, because the client already holds every heading the collection
 * will ever draw and where each one starts. Which is what makes jumping to a
 * date in 2014 the same instant operation as scrolling one screen.
 */
function JumpPane({
  grid,
  onBack,
  onDone,
}: {
  grid: GridView;
  onBack: () => void;
  onDone: () => void;
}) {
  const { days, loading, jump } = grid;
  const range = useMemo(() => rangeOf(days), [days]);

  const go = useCallback(
    (date: Date | undefined) => {
      if (!date || !range) return;
      const at = nearestDay(days, date);
      if (at >= 0) jump(days[at].start);
      onDone();
    },
    [days, range, jump, onDone],
  );

  return (
    <div className="flex flex-col">
      <div className="flex items-center gap-1 px-1.5 pt-1.5">
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="size-8"
          aria-label="Back to sort and filter"
          onClick={onBack}
        >
          <ChevronLeft className="size-4" aria-hidden="true" />
        </Button>
        <h2 className="text-[13px] font-medium">Jump to date</h2>
        {range ? (
          <span className="ml-auto pr-2 text-[11px] tabular-nums text-faint">
            {range.first.slice(0, 4)} — {range.last.slice(0, 4)}
          </span>
        ) : null}
      </div>

      <Separator className="mt-1.5" />

      {/* While a reordered day table is on its way, the one in hand describes a
          timeline nobody is looking at — and a date resolved against it would
          scroll to the right number in the wrong list. Waiting is a moment;
          landing somewhere arbitrary is a thing people learn not to trust the
          control for. */}
      {loading || !range ? (
        <p className="px-3 py-10 text-center text-[13px] text-muted-foreground">
          {loading ? "Reordering the timeline…" : "Nothing here to jump to."}
        </p>
      ) : (
        <Calendar
          mode="single"
          autoFocus
          className="mx-auto [--cell-size:--spacing(8)]"
          defaultMonth={dateOf(range.last)}
          startMonth={dateOf(range.first)}
          endMonth={dateOf(range.last)}
          captionLayout="dropdown"
          // Outside the range there is nothing to go to; inside it there may be
          // nothing on that particular day, which is not the same thing and is
          // answered by landing on the nearest day that does have something.
          disabled={{ before: dateOf(range.first), after: dateOf(range.last) }}
          modifiers={{ empty: (date: Date) => !range.keys.has(keyOf(date)) }}
          modifiersClassNames={{ empty: "text-faint" }}
          onSelect={go}
        />
      )}
    </div>
  );
}

/** The span a collection covers, and every day in it that holds something. */
interface Span {
  /** The oldest day, YYYY-MM-DD. */
  first: string;
  /** The newest. */
  last: string;
  keys: Set<string>;
}

/**
 * The dates a day table covers.
 *
 * Scanned rather than read off the ends, because the runs are in timeline order
 * and timeline order is not date order in either direction: a photograph taken
 * across a timezone boundary puts a date on both sides of another one, and an
 * order by length has no dates at all. Null when there is nothing to jump to.
 */
function rangeOf(days: Day[]): Span | null {
  const keys = new Set<string>();
  let first = "";
  let last = "";
  for (const day of days) {
    if (day.key === "") continue;
    keys.add(day.key);
    if (first === "" || day.key < first) first = day.key;
    if (day.key > last) last = day.key;
  }
  return first === "" ? null : { first, last, keys };
}

/**
 * The heading a date should land on: that day, or the nearest one that exists.
 *
 * A day inside the range with nothing in it is the ordinary case — most people
 * do not photograph every day — and refusing to move would be an answer nobody
 * asked for. Ties go to the newer day, which is the direction the grid is read
 * in. Linear over the day table, which is thousands of entries at most and is
 * walked once per click.
 */
function nearestDay(days: Day[], date: Date): number {
  const wanted = msOf(keyOf(date));
  let best = -1;
  let bestKey = "";
  let closest = Infinity;

  for (let i = 0; i < days.length; i++) {
    const key = days[i].key;
    if (key === "") continue;
    const away = Math.abs(msOf(key) - wanted);
    if (away === 0) return i;
    if (away < closest || (away === closest && key > bestKey)) {
      closest = away;
      bestKey = key;
      best = i;
    }
  }
  return best;
}

/** A local date as the YYYY-MM-DD the day table is keyed by. */
function keyOf(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** The other direction, as a local date the calendar can be handed. */
function dateOf(key: string): Date {
  const [y, m, d] = key.split("-").map(Number);
  return new Date(y, m - 1, d);
}

function msOf(key: string): number {
  const [y, m, d] = key.split("-").map(Number);
  return Date.UTC(y, m - 1, d);
}
