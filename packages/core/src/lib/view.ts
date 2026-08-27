// The rules a sort-and-filter control has to obey, kept apart from the control
// that draws them.
//
// Every function here is total and pure: a view in, a view out. Which is what
// makes the awkward parts statable rather than scattered through click
// handlers — "ordering by length is a question about videos", "turning the last
// filter off is the same as turning All on", "an album has no photographs that
// are in no album". Each of those is one line below and a test beside it.

// Types only. This module is pure and is tested by node's own runner, which
// resolves what it imports for real — and the wire client reaches for the
// browser on the way in.
import type { MediaKind, TimelineFilter, View } from "../wire/api.ts";

/** Newest first, nothing filtered out: what a grid opens as. */
export const DEFAULT_VIEW: View = { sort: "newest" };

/**
 * A stable string for a view, so React can depend on one without depending on
 * the object literal that carries it.
 *
 * Also what the timeline reloads on. Two views that name the same timeline must
 * produce the same key or the grid refetches itself for nothing.
 */
export function viewKey(view: View): string {
  return [
    view.sort,
    view.media ?? "",
    view.favorites ? "f" : "",
    view.unalbumed ? "u" : "",
  ].join("|");
}

/**
 * A stable string for the collection itself, as opposed to how it is being
 * looked at. Undefined is the library.
 *
 * Every field is in it, because two filters that differ anywhere are two
 * different timelines: album 3 and album 4 share nothing, and the Hidden bucket
 * browsed inside a person is not that person browsed in the library.
 */
export function filterKey(filter?: TimelineFilter): string {
  if (!filter) return "library";
  if (filter.kind === "trash") return "trash";
  if (filter.kind === "vault") {
    return filter.within
      ? `vault:${filter.bucket}:${filter.within.kind}:${filter.within.value}`
      : `vault:${filter.bucket}`;
  }
  return `${filter.kind}:${filter.value}`;
}

/**
 * The whole address of a grid: which collection, in which order, narrowed how.
 *
 * What an offline cache is keyed by, and the reason it is keyed by both halves.
 * A day table describes one collection read one way — index 2 of an album
 * sorted oldest-first is a different photograph from index 2 of the same album
 * sorted newest-first — so a cache that keyed on the collection alone would
 * hand a reorder somebody else's geometry. See WEB_TO_MOBILE § 3.6.
 */
export function collectionKey(filter: TimelineFilter | undefined, view: View): string {
  return `${filterKey(filter)}#${viewKey(view)}`;
}

/** Whether this is the timeline a grid opens as: newest first, nothing hidden. */
export function isDefault(view: View): boolean {
  return view.sort === "newest" && !isFiltered(view);
}

/** Whether anything is being filtered out, as opposed to merely reordered. */
export function isFiltered(view: View): boolean {
  return view.media !== undefined || !!view.favorites || !!view.unalbumed;
}

/**
 * The two orders that are about length rather than time.
 *
 * They are the reason several rules below exist at all: a photograph has no
 * duration, so these only mean something over videos.
 */
export function byDuration(view: View): boolean {
  return view.sort === "longest" || view.sort === "shortest";
}

/**
 * Which filters a grid can offer, given what that grid already is.
 *
 * A collection is itself a filter, and offering the same one again is at best
 * noise and at worst a combination that can only ever be empty. Inside the
 * Videos category every item is a video; inside an album nothing is in no
 * album. Hiding those is not a special case per page — it is one question asked
 * of the filter the page was built from.
 */
export interface Facets {
  media: boolean;
  favorites: boolean;
  unalbumed: boolean;
}

export function facetsFor(filter?: TimelineFilter): Facets {
  // Inside the vault the collection is nested one level down, because the
  // bucket is the scope and the album is what it has been narrowed to.
  const within = filter?.kind === "vault" ? filter.within : filter;
  const album = within?.kind === "albums";
  const category = within?.kind === "categories" ? within.value : undefined;

  return {
    media: category !== "videos",
    favorites: category !== "favorites",
    unalbumed: !album,
  };
}

/**
 * The named states of the filter row, in the order it draws them.
 *
 * "All" is not a filter but the absence of every other one, which is why it is
 * in the same list: the control it appears in is a set of toggles, and the way
 * to say "none of these" in a set of toggles is a button that turns the rest
 * off.
 */
export type Facet = "all" | "photos" | "videos" | "favorites" | "unalbumed";

export const FACET_LABEL: Record<Facet, string> = {
  all: "All photos",
  photos: "Photos",
  videos: "Videos",
  favorites: "Favorites",
  unalbumed: "Not in an album",
};

/** Which toggles are lit, which is a reading of the view rather than a state. */
export function facetsOn(view: View): Facet[] {
  const on: Facet[] = [];
  if (view.media === "image") on.push("photos");
  if (view.media === "video") on.push("videos");
  if (view.favorites) on.push("favorites");
  if (view.unalbumed) on.push("unalbumed");
  return on.length > 0 ? on : ["all"];
}

/**
 * One toggle pressed, and everything that follows from it.
 *
 * Photos and Videos are one choice rather than two, because a timeline showing
 * both is what turning either of them off means. All turns everything off. And
 * a view that has stopped being about videos cannot still be ordered by length
 * — see byDuration — so that order goes back to the one the grid opens in
 * rather than sorting photographs by a duration they do not have.
 */
export function toggleFacet(view: View, facet: Facet): View {
  const next: View = { ...view };

  switch (facet) {
    case "all":
      next.media = undefined;
      next.favorites = false;
      next.unalbumed = false;
      break;
    case "photos":
    case "videos": {
      const wanted: MediaKind = facet === "photos" ? "image" : "video";
      next.media = view.media === wanted ? undefined : wanted;
      break;
    }
    case "favorites":
      next.favorites = !view.favorites;
      break;
    case "unalbumed":
      next.unalbumed = !view.unalbumed;
      break;
  }

  if (byDuration(next) && next.media !== "video") next.sort = "newest";
  return clean(next);
}

/**
 * One order chosen.
 *
 * Ordering by length is a question about videos, so asking it says so — the
 * filter follows the sort rather than the two disagreeing and returning a grid
 * of photographs in an arbitrary order. Where the grid is already videos only,
 * because it is the Videos category, there is no toggle to move and none is
 * moved.
 */
export function pickSort(view: View, sort: View["sort"], facets: Facets): View {
  const next: View = { ...view, sort };
  if (byDuration(next) && facets.media) next.media = "video";
  return clean(next);
}

/**
 * A view narrowed to what this grid can actually offer.
 *
 * The filter row hides what a collection already implies, and a view carrying a
 * facet no longer on screen would be one nobody can turn off. Applied when a
 * grid publishes what it is rather than when the pill draws, so the timeline
 * and the control agree on what is being asked for.
 */
export function withinFacets(view: View, facets: Facets): View {
  const next: View = { ...view };
  if (!facets.media) next.media = undefined;
  if (!facets.favorites) next.favorites = false;
  if (!facets.unalbumed) next.unalbumed = false;
  // The one grid with no media toggle is the Videos category, which is videos
  // only whether or not anything says so — an order about length still means
  // something there. Anywhere else it needs the toggle to be holding it.
  if (byDuration(next) && facets.media && next.media !== "video") next.sort = "newest";
  return clean(next);
}

/** Drops the falsy fields, so two equal views have one spelling. See viewKey. */
function clean(view: View): View {
  const out: View = { sort: view.sort };
  if (view.media) out.media = view.media;
  if (view.favorites) out.favorites = true;
  if (view.unalbumed) out.unalbumed = true;
  return out;
}

export const SORT_LABEL: Record<View["sort"], string> = {
  newest: "Newest",
  oldest: "Oldest",
  longest: "Longest",
  shortest: "Shortest",
};

/**
 * What the pill says: the shortest true description of what is on screen, or
 * nothing at all when the grid is the one it opens as.
 *
 * The order is named only when it is not the default one, because a pill that
 * said "Newest" on every ordinary grid would be saying nothing while taking up
 * the room that says something.
 *
 * Past a certain length the filters are counted rather than listed. The pill
 * grows leftwards into a bar that also holds the selection control, so what is
 * being bought with those characters is room the rest of the screen has; two
 * filters fit and three do not, and "3 filters" is still a true and openable
 * answer where a line running off the edge is neither.
 */
const PILL_ROOM = 32;

export function describeView(view: View): string {
  const order = view.sort === "newest" ? [] : [SORT_LABEL[view.sort]];
  const facets = facetsOn(view).filter((facet) => facet !== "all");

  const spelled = [...order, ...facets.map((facet) => FACET_LABEL[facet])].join(" · ");
  if (spelled.length <= PILL_ROOM) return spelled;
  return [...order, `${facets.length} filters`].join(" · ");
}
