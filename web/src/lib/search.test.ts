// The rules a chip row has to obey, checked without one.
//
// Each of these is a sentence from search.ts read back as an assertion: a
// removed date takes both ends with it, a removed phrase is emptied rather than
// dropped, one of two people comes off without the other, and the page's own
// parameters never reach the server.

import { test } from "node:test";
import assert from "node:assert/strict";

import type { ParsedQuery } from "./api.ts";
import {
  asks,
  askFor,
  chipsOf,
  dateLabel,
  explicitParams,
  parsing,
  requestOf,
  withoutChip,
} from "./search.ts";

const PARSE: ParsedQuery = {
  text: "phoenix and dominic at the beach last summer",
  people: ["Phoenix", "Dominic"],
  place: { city: "Moraga", admin1: "California", country: "United States" },
  after: "2025-06-01",
  before: "2025-09-30",
  visual: "at the beach",
  source: "grammar",
};

function chip(query: ParsedQuery, id: string) {
  const found = chipsOf(query).find((c) => c.id === id);
  assert.ok(found, `no chip ${id}`);
  return found;
}

test("a request is only the parameters the server reads", () => {
  const page = new URLSearchParams("q=beach&asset=abc&parse=0&person=Phoenix&zoom=3");
  const request = requestOf(page);
  assert.equal(request.get("q"), "beach");
  assert.equal(request.get("person"), "Phoenix");
  assert.equal(request.get("asset"), null);
  assert.equal(request.get("zoom"), null);
});

test("repeated parameters survive the trip", () => {
  const request = requestOf(new URLSearchParams("person=Phoenix&person=Dominic&tag=dog"));
  assert.deepEqual(request.getAll("person"), ["Phoenix", "Dominic"]);
  assert.deepEqual(request.getAll("tag"), ["dog"]);
});

test("parse is on unless it is turned off", () => {
  assert.equal(parsing(new URLSearchParams("q=beach")), true);
  assert.equal(parsing(new URLSearchParams("q=beach&parse=1")), true);
  assert.equal(parsing(new URLSearchParams("q=beach&parse=0")), false);
});

test("an empty box asks nothing, in either spelling", () => {
  assert.equal(asks(askFor("  ")), false);
  assert.equal(asks(askFor("beach")), true);
  // Every chip taken off: the sentence is still in the URL for the box to show,
  // and there is nothing left to search for.
  assert.equal(asks(new URLSearchParams("q=phoenix&parse=0&visual=")), false);
  assert.equal(asks(new URLSearchParams("q=phoenix&parse=0&visual=&person=Phoenix")), true);
});

test("a parse becomes the parameters that reproduce it", () => {
  const params = explicitParams(PARSE);
  assert.equal(params.get("parse"), "0");
  assert.equal(params.get("q"), PARSE.text);
  assert.deepEqual(params.getAll("person"), ["Phoenix", "Dominic"]);
  // The city, and not the state or the country it also carries: exactly one
  // level was matched, and the other two are only there to say it aloud.
  assert.equal(params.get("city"), "Moraga");
  assert.equal(params.get("admin1"), null);
  assert.equal(params.get("after"), "2025-06-01");
  assert.equal(params.get("before"), "2025-09-30");
  assert.equal(params.get("visual"), "at the beach");
});

test("a phrase-less parse spells the empty phrase rather than omitting it", () => {
  const params = explicitParams({ text: "phoenix", people: ["Phoenix"], source: "grammar" });
  assert.equal(params.has("visual"), true);
  assert.equal(params.get("visual"), "");
});

test("one of two people comes off on its own", () => {
  const params = withoutChip(PARSE, chip(PARSE, "person:Phoenix"));
  assert.deepEqual(params.getAll("person"), ["Dominic"]);
  assert.equal(params.get("city"), "Moraga");
});

test("a date range comes off at both ends", () => {
  const params = withoutChip(PARSE, chip(PARSE, "dates"));
  assert.equal(params.has("after"), false);
  assert.equal(params.has("before"), false);
  assert.deepEqual(params.getAll("person"), ["Phoenix", "Dominic"]);
});

test("a place comes off at every level", () => {
  const params = withoutChip(PARSE, chip(PARSE, "place:Moraga"));
  assert.equal(params.has("city"), false);
  assert.equal(params.has("admin1"), false);
  assert.equal(params.has("country"), false);
});

test("the phrase is emptied, not dropped", () => {
  const params = withoutChip(PARSE, chip(PARSE, "visual"));
  assert.equal(params.has("visual"), true);
  assert.equal(params.get("visual"), "");
});

test("chips are drawn in the order a sentence is about", () => {
  const kinds = chipsOf({
    ...PARSE,
    tags: ["dog"],
    kind: "video",
    category: "screenshots",
    favorites: true,
  }).map((c) => c.kind);
  assert.deepEqual(kinds, [
    "person",
    "person",
    "place",
    "dates",
    "tag",
    "kind",
    "category",
    "favorites",
    "visual",
  ]);
});

test("only the phrase is fuzzy", () => {
  const fuzzy = chipsOf(PARSE).filter((c) => c.fuzzy);
  assert.equal(fuzzy.length, 1);
  assert.equal(fuzzy[0].kind, "visual");
});

test("a parse that narrowed nothing has no chips", () => {
  assert.deepEqual(chipsOf({ text: "beach", source: "grammar" }), []);
});

test("a whole year says the year", () => {
  assert.equal(dateLabel("2019-01-01", "2019-12-31"), "2019");
  assert.equal(dateLabel("2019-01-01", "2021-12-31"), "2019–2021");
});

test("a whole month says the month, and a run of them says both ends", () => {
  const month = dateLabel("2025-06-01", "2025-06-30");
  assert.match(month, /2025$/);
  assert.equal(month.includes("–"), false);

  const summer = dateLabel("2025-06-01", "2025-09-30");
  assert.match(summer, /–/);
  assert.match(summer, /2025$/);
});

test("an open-ended range says which end it is open at", () => {
  assert.match(dateLabel("2019-06-01", undefined), /^Since /);
  assert.match(dateLabel(undefined, "2019-06-01"), /^Until /);
  assert.equal(dateLabel(undefined, undefined), "");
});

test("a single day is one day rather than a range", () => {
  const day = dateLabel("2019-06-04", "2019-06-04");
  assert.equal(day.includes("–"), false);
  assert.match(day, /2019/);
});

test("a range that is not whole months falls back to two days", () => {
  assert.match(dateLabel("2021-03-12", "2021-04-04"), / – /);
});
