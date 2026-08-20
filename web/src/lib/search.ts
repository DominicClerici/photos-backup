// A search, as the thing a URL can hold and a chip row can edit.
//
// Everything here is total and pure — a parse in, parameters or labels out —
// for the reason lib/view is: the awkward parts are statable rather than
// scattered through click handlers. "Removing a date removes both ends", "a
// query can name two people and only one of them is being taken off", "an empty
// visual phrase has to be spelled, not omitted". Each of those is one function
// below with a test beside it.
//
// The shape it all rests on is ML_IMAGES.md §7's escape hatch. `?q=` alone asks
// the server to read the sentence; `parse=0` beside explicit parameters asks it
// to read nothing and take the filter as given. That second spelling is the
// only way to say "and *not* the date it found" — a parse can be added to but
// has no way to be subtracted from — so taking a chip off means materialising
// the whole reading into parameters and leaving one out.

// Types only, so this module stays pure and node's own test runner can resolve
// it without reaching for the browser. See lib/view.
import type { ParsedQuery, SearchPlace } from "./api";

/**
 * The parameters `/v1/search` reads, and the only ones a page URL passes on.
 *
 * An allowlist rather than a subtraction, because the page's own URL carries
 * things the server has no business seeing — `asset` is the open photograph,
 * which belongs to the viewer and not to the query. Adding a parameter here is
 * how a new filter reaches the server; forgetting to is a filter that silently
 * does nothing, which is why the two lists are one list.
 */
export const SEARCH_PARAMS = [
  "q",
  "parse",
  "person",
  "tag",
  "city",
  "admin1",
  "country",
  "after",
  "before",
  "kind",
  "category",
  "favorites",
  "visual",
] as const;

/**
 * The search request inside a page's URL.
 *
 * The page's query string *is* the API's, which is what makes a search
 * linkable, back-buttonable and re-runnable with no state held anywhere: the
 * chips edit the URL, and the URL is what gets sent.
 */
export function requestOf(page: URLSearchParams): URLSearchParams {
  const out = new URLSearchParams();
  for (const key of SEARCH_PARAMS) {
    for (const value of page.getAll(key)) out.append(key, value);
  }
  return out;
}

/** Whether a request asks for anything at all. The empty box searches nothing. */
export function asks(request: URLSearchParams): boolean {
  if (parsing(request)) return (request.get("q") ?? "").trim() !== "";
  return [...request.entries()].some(
    ([key, value]) => key !== "parse" && key !== "q" && value !== "",
  );
}

/** Whether the server is being asked to read the sentence, or take it as given. */
export function parsing(request: URLSearchParams): boolean {
  const raw = request.get("parse");
  return raw === null || (raw !== "0" && raw !== "false" && raw !== "no");
}

/** A plain search: one sentence, and the server reads it. */
export function askFor(text: string): URLSearchParams {
  return new URLSearchParams({ q: text.trim() });
}

/**
 * A parse, written back out as the parameters that reproduce it exactly.
 *
 * `parse=0` is the whole point: the reading in hand becomes the request, so
 * nothing is guessed a second time and the filter can be edited a field at a
 * time. `q` still travels, unread, because the box at the top of the page shows
 * what was typed and a URL that had dropped it would have nothing to show.
 *
 * `visual` is always written, empty included. A query that is entirely a name
 * has no phrase for the encoder, and the server falls back to `q` for an absent
 * `visual` — so "not this one" has to be spelled rather than omitted.
 */
export function explicitParams(query: ParsedQuery): URLSearchParams {
  const params = new URLSearchParams();
  if (query.text) params.set("q", query.text);
  params.set("parse", "0");
  for (const person of query.people ?? []) params.append("person", person);
  for (const tag of query.tags ?? []) params.append("tag", tag);
  if (query.place?.city) params.set("city", query.place.city);
  else if (query.place?.admin1) params.set("admin1", query.place.admin1);
  else if (query.place?.country) params.set("country", query.place.country);
  if (query.after) params.set("after", query.after);
  if (query.before) params.set("before", query.before);
  if (query.kind) params.set("kind", query.kind);
  if (query.category) params.set("category", query.category);
  if (query.favorites) params.set("favorites", "1");
  params.set("visual", query.visual ?? "");
  return params;
}

/** What a chip is about, which is what taking it off has to undo. */
export type ChipKind =
  | "person"
  | "place"
  | "tag"
  | "dates"
  | "kind"
  | "category"
  | "favorites"
  | "visual";

export interface Chip {
  /** Unique in the row: a query can name two people, and each × is its own. */
  id: string;
  kind: ChipKind;
  /** Which one, for the kinds there can be more than one of. */
  value?: string;
  label: string;
  /**
   * True for the phrase that went to the encoder, which is the one chip that is
   * not a filter. Drawn differently because removing it does something else:
   * every other chip narrows the answer, and this one is the question.
   */
  fuzzy?: boolean;
}

export const CATEGORY_LABEL: Record<string, string> = {
  screenshots: "Screenshots",
  panoramas: "Panoramas",
  live: "Live Photos",
  bursts: "Bursts",
  selfies: "Selfies",
  videos: "Videos",
  favorites: "Favorites",
  slomo: "Slo-mo",
  timelapse: "Time-lapse",
  portrait: "Portrait",
  raw: "RAW",
  edited: "Edited",
};

/**
 * The reading, as a row of removable labels.
 *
 * In the order the sentence is usually about: who, where, when, what kind, and
 * last the phrase nobody could answer exactly. A wrong parse is visible here or
 * it is not visible anywhere.
 */
export function chipsOf(query: ParsedQuery): Chip[] {
  const chips: Chip[] = [];

  for (const person of query.people ?? []) {
    chips.push({ id: `person:${person}`, kind: "person", value: person, label: person });
  }
  if (query.place) {
    const label = placeLabel(query.place);
    if (label) chips.push({ id: `place:${label}`, kind: "place", label });
  }
  if (query.after || query.before) {
    chips.push({
      id: "dates",
      kind: "dates",
      label: dateLabel(query.after, query.before),
    });
  }
  for (const tag of query.tags ?? []) {
    chips.push({ id: `tag:${tag}`, kind: "tag", value: tag, label: tag });
  }
  if (query.kind) {
    chips.push({
      id: `kind:${query.kind}`,
      kind: "kind",
      label: query.kind === "video" ? "Videos" : "Photos",
    });
  }
  if (query.category) {
    chips.push({
      id: `category:${query.category}`,
      kind: "category",
      label: CATEGORY_LABEL[query.category] ?? query.category,
    });
  }
  if (query.favorites) {
    chips.push({ id: "favorites", kind: "favorites", label: "Favorites" });
  }
  if (query.visual) {
    chips.push({ id: "visual", kind: "visual", label: query.visual, fuzzy: true });
  }

  return chips;
}

/** How a place says itself, at whichever level was matched. */
export function placeLabel(place: SearchPlace): string {
  return place.city || place.admin1 || place.country || "";
}

/**
 * The reading with one chip taken off, as a request.
 *
 * A date is one chip and comes off at both ends, because a range with one end
 * removed is a filter nobody asked for — "everything since June" is not what
 * "last summer, but not that" means.
 *
 * The phrase is the exception: it is emptied rather than dropped, since an
 * absent `visual` means "fall back to what was typed" and this has to mean the
 * opposite. See explicitParams.
 */
export function withoutChip(query: ParsedQuery, chip: Chip): URLSearchParams {
  const params = explicitParams(query);

  switch (chip.kind) {
    case "person":
    case "tag": {
      const key = chip.kind;
      const kept = params.getAll(key).filter((value) => value !== chip.value);
      params.delete(key);
      for (const value of kept) params.append(key, value);
      break;
    }
    case "place":
      params.delete("city");
      params.delete("admin1");
      params.delete("country");
      break;
    case "dates":
      params.delete("after");
      params.delete("before");
      break;
    case "kind":
      params.delete("kind");
      break;
    case "category":
      params.delete("category");
      break;
    case "favorites":
      params.delete("favorites");
      break;
    case "visual":
      params.set("visual", "");
      break;
  }

  return params;
}

/**
 * A date range, said the shortest way that is still true.
 *
 * Both ends are inclusive civil days, which is what lets a whole month be
 * spelled as a month rather than as the two days at its edges. The cases run
 * from the tidiest reading to the plainest, and the last one always works.
 */
export function dateLabel(after?: string, before?: string): string {
  const from = dayOf(after);
  const to = dayOf(before);

  if (!from && !to) return "";
  if (from && !to) return `Since ${plainDay(from)}`;
  if (to && !from) return `Until ${plainDay(to)}`;
  if (!from || !to) return "";

  if (after === before) return plainDay(from);

  const wholeStart = from.getUTCDate() === 1;
  const wholeEnd = to.getUTCDate() === lastDayOf(to);
  const sameYear = from.getUTCFullYear() === to.getUTCFullYear();

  if (wholeStart && wholeEnd && sameYear) {
    const first = from.getUTCMonth();
    const last = to.getUTCMonth();
    if (first === 0 && last === 11) return String(from.getUTCFullYear());
    if (first === last) return `${monthName(from, "long")} ${from.getUTCFullYear()}`;
    return `${monthName(from, "short")}–${monthName(to, "short")} ${from.getUTCFullYear()}`;
  }
  if (wholeStart && wholeEnd && from.getUTCMonth() === 0 && to.getUTCMonth() === 11) {
    return `${from.getUTCFullYear()}–${to.getUTCFullYear()}`;
  }

  return `${plainDay(from)} – ${plainDay(to)}`;
}

function dayOf(day?: string): Date | null {
  if (!day) return null;
  const at = new Date(`${day}T00:00:00Z`);
  return Number.isNaN(at.getTime()) ? null : at;
}

function plainDay(at: Date): string {
  return at.toLocaleDateString(undefined, {
    timeZone: "UTC",
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

function monthName(at: Date, month: "short" | "long"): string {
  return at.toLocaleDateString(undefined, { timeZone: "UTC", month });
}

function lastDayOf(at: Date): number {
  return new Date(Date.UTC(at.getUTCFullYear(), at.getUTCMonth() + 1, 0)).getUTCDate();
}

// ---------------------------------------------------------------------------
// What has been asked before.

const RECENTS_KEY = "photos.search.recent";
/** Enough to recognise last night's question, few enough to read at a glance. */
const RECENTS_MAX = 6;

/**
 * The last few sentences somebody typed, newest first.
 *
 * In localStorage rather than on the server, because it is a convenience of
 * this browser rather than a fact about the archive — and because a list of
 * what somebody went looking for is the kind of thing worth not writing down
 * anywhere it would outlive them clearing their history.
 */
export function recentSearches(): string[] {
  if (typeof window === "undefined") return [];
  try {
    const raw = window.localStorage?.getItem(RECENTS_KEY);
    const held: unknown = raw ? JSON.parse(raw) : [];
    if (!Array.isArray(held)) return [];
    return held.filter((entry): entry is string => typeof entry === "string" && entry !== "");
  } catch {
    // A refusal, or something else's key at this name. Neither is worth
    // breaking a search box over.
    return [];
  }
}

/** Files a query at the top of the list, moving it there if it is already in it. */
export function rememberSearch(text: string): void {
  const query = text.trim();
  if (!query || typeof window === "undefined") return;
  const next = [query, ...recentSearches().filter((held) => held !== query)].slice(0, RECENTS_MAX);
  try {
    window.localStorage.setItem(RECENTS_KEY, JSON.stringify(next));
  } catch {
    // See above.
  }
}

export function forgetSearches(): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.removeItem(RECENTS_KEY);
  } catch {
    // See above.
  }
}
