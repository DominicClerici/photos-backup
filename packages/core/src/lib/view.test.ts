// The rules a sort-and-filter control has to obey, checked without one.
//
// Every one of these is a sentence from view.ts read back as an assertion:
// ordering by length is a question about videos, All is the absence of the
// rest, and a collection does not offer the filter it already is.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  byDuration,
  DEFAULT_VIEW,
  describeView,
  facetsFor,
  facetsOn,
  isDefault,
  isFiltered,
  pickSort,
  toggleFacet,
  viewKey,
  withinFacets,
} from "./view.ts";
import type { TimelineFilter, View } from "../wire/api.ts";

const EVERYWHERE = { media: true, favorites: true, unalbumed: true };

test("the default view is newest and nothing hidden", () => {
  assert.equal(isDefault(DEFAULT_VIEW), true);
  assert.equal(isFiltered(DEFAULT_VIEW), false);
  assert.deepEqual(facetsOn(DEFAULT_VIEW), ["all"]);
  assert.equal(describeView(DEFAULT_VIEW), "");
});

test("two views naming the same timeline have the same key", () => {
  const spelled: View = { sort: "newest", media: undefined, favorites: false };
  assert.equal(viewKey(spelled), viewKey(DEFAULT_VIEW));
  assert.notEqual(viewKey({ sort: "oldest" }), viewKey(DEFAULT_VIEW));
});

test("ordering by length asks for videos", () => {
  const view = pickSort(DEFAULT_VIEW, "longest", EVERYWHERE);
  assert.equal(view.sort, "longest");
  assert.equal(view.media, "video");
  assert.equal(byDuration(view), true);
  assert.deepEqual(facetsOn(view), ["videos"]);
});

test("a grid that is already videos only needs no toggle moved", () => {
  const inside = { media: false, favorites: true, unalbumed: true };
  const view = pickSort(DEFAULT_VIEW, "shortest", inside);
  assert.equal(view.sort, "shortest");
  assert.equal(view.media, undefined);
});

test("taking the videos away takes the order about length with it", () => {
  const videos = pickSort(DEFAULT_VIEW, "longest", EVERYWHERE);

  // Every way of ceasing to be videos-only: the same toggle again, the other
  // medium, and All.
  for (const facet of ["videos", "photos", "all"] as const) {
    const after = toggleFacet(videos, facet);
    assert.equal(after.sort, "newest", `${facet} left the order by length behind`);
  }

  // And one that does not: favourites narrows the videos rather than replacing
  // them, so the order still means something.
  const starred = toggleFacet(videos, "favorites");
  assert.equal(starred.sort, "longest");
  assert.equal(starred.media, "video");
  assert.equal(starred.favorites, true);
});

test("photos and videos are one choice rather than two", () => {
  const photos = toggleFacet(DEFAULT_VIEW, "photos");
  assert.equal(photos.media, "image");

  const videos = toggleFacet(photos, "videos");
  assert.equal(videos.media, "video");

  // And pressing the lit one again is how a medium is let go of.
  assert.equal(toggleFacet(videos, "videos").media, undefined);
});

test("All is the absence of the rest, and turning the rest off is All", () => {
  const narrowed = toggleFacet(toggleFacet(toggleFacet(DEFAULT_VIEW, "videos"), "favorites"), "unalbumed");
  assert.deepEqual(facetsOn(narrowed), ["videos", "favorites", "unalbumed"]);

  const all = toggleFacet(narrowed, "all");
  assert.deepEqual(facetsOn(all), ["all"]);
  assert.equal(isDefault(all), true);

  // Turning the only filter off arrives at the same place by the other road.
  assert.deepEqual(facetsOn(toggleFacet(toggleFacet(DEFAULT_VIEW, "favorites"), "favorites")), ["all"]);
});

test("a collection does not offer the filter it already is", () => {
  const videos: TimelineFilter = { kind: "categories", value: "videos" };
  assert.deepEqual(facetsFor(videos), { media: false, favorites: true, unalbumed: true });

  const favorites: TimelineFilter = { kind: "categories", value: "favorites" };
  assert.deepEqual(facetsFor(favorites), { media: true, favorites: false, unalbumed: true });

  const album: TimelineFilter = { kind: "albums", value: "an-id" };
  assert.deepEqual(facetsFor(album), { media: true, favorites: true, unalbumed: false });

  // Inside the vault the collection is nested one level down, and the same
  // question has the same answer.
  const hiddenAlbum: TimelineFilter = {
    kind: "vault",
    bucket: "hidden",
    within: { kind: "albums", value: "an-id" },
  };
  assert.deepEqual(facetsFor(hiddenAlbum), { media: true, favorites: true, unalbumed: false });

  // Everything else — the library, the trash, a person, a bucket — offers all
  // three.
  assert.deepEqual(facetsFor(undefined), EVERYWHERE);
  assert.deepEqual(facetsFor({ kind: "trash" }), EVERYWHERE);
  assert.deepEqual(facetsFor({ kind: "people", value: "Dominic" }), EVERYWHERE);
});

test("a view is narrowed to what the grid it lands in can offer", () => {
  const starredVideos: View = { sort: "newest", media: "video", favorites: true, unalbumed: true };

  const inAlbum = withinFacets(starredVideos, { media: true, favorites: true, unalbumed: false });
  assert.equal(inAlbum.unalbumed, undefined);
  assert.equal(inAlbum.media, "video");

  // Losing the media toggle in a grid that is not videos-by-collection takes
  // the order by length with it, because nothing is holding it any more.
  const loose = withinFacets({ sort: "longest", media: "video" }, EVERYWHERE);
  assert.equal(loose.sort, "longest");

  const stripped = withinFacets({ sort: "longest" }, EVERYWHERE);
  assert.equal(stripped.sort, "newest");

  // Except in the Videos category, where the collection is the filter.
  const inVideos = withinFacets({ sort: "longest", media: "video" }, {
    media: false,
    favorites: true,
    unalbumed: true,
  });
  assert.equal(inVideos.sort, "longest");
  assert.equal(inVideos.media, undefined);
});

test("narrowing a view twice changes nothing the second time", () => {
  const view: View = { sort: "longest", media: "video", favorites: true, unalbumed: true };
  const once = withinFacets(view, { media: true, favorites: false, unalbumed: false });
  const twice = withinFacets(once, { media: true, favorites: false, unalbumed: false });
  assert.equal(viewKey(once), viewKey(twice));
});

test("the pill says the shortest true thing", () => {
  assert.equal(describeView({ sort: "oldest" }), "Oldest");
  assert.equal(describeView({ sort: "newest", media: "video" }), "Videos");
  assert.equal(
    describeView({ sort: "longest", media: "video", favorites: true }),
    "Longest · Videos · Favorites",
  );

  // Past what the pill has room for, the filters are counted rather than listed.
  assert.equal(
    describeView({ sort: "newest", media: "video", favorites: true, unalbumed: true }),
    "3 filters",
  );
  assert.equal(
    describeView({ sort: "longest", media: "video", favorites: true, unalbumed: true }),
    "Longest · 3 filters",
  );
});
