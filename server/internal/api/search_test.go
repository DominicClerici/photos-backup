package api

import (
	"net/url"
	"testing"
	"time"
)

// The explicit spelling is what a chip row edits, and the whole of what makes a
// wrong parse fixable: a parse can be merged on top of but never subtracted
// from, so "and *not* the date it found" has to be expressible as parameters.
// These check that every field survives the round trip, and that the one field
// with a default has a way to say it does not want it.

func TestExplicitQueryReadsEveryField(t *testing.T) {
	q := explicitQuery(url.Values{
		"person":    {"Phoenix", "Dominic"},
		"tag":       {"dog"},
		"city":      {"Moraga"},
		"after":     {"2025-06-01"},
		"before":    {"2025-09-30"},
		"kind":      {"video"},
		"category":  {"screenshots"},
		"favorites": {"1"},
		"visual":    {"at the beach"},
	}, "phoenix at the beach last summer")

	if len(q.People) != 2 || q.People[0] != "Phoenix" || q.People[1] != "Dominic" {
		t.Fatalf("people = %v", q.People)
	}
	if len(q.Tags) != 1 || q.Tags[0] != "dog" {
		t.Fatalf("tags = %v", q.Tags)
	}
	if q.Place == nil || q.Place.City != "Moraga" {
		t.Fatalf("place = %+v", q.Place)
	}
	if q.After == nil || !q.After.Equal(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("after = %v", q.After)
	}
	if q.Before == nil || !q.Before.Equal(time.Date(2025, 9, 30, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("before = %v", q.Before)
	}
	if q.Kind != "video" || q.Category != "screenshots" || !q.Favorites {
		t.Fatalf("facets = %q %q %v", q.Kind, q.Category, q.Favorites)
	}
	if q.Visual != "at the beach" {
		t.Fatalf("visual = %q", q.Visual)
	}
}

// An absent `visual` falls back to what was typed, because a caller that sent
// only `q` meant the whole sentence. A *present* and empty one is the opposite
// claim and has to be believed: "phoenix", all of it a name, has no phrase for
// the encoder, and handing it "phoenix" would rank by a word the filter has
// already answered exactly.
func TestExplicitQueryVisualIsPresenceNotEmptiness(t *testing.T) {
	fallback := explicitQuery(url.Values{"person": {"Phoenix"}}, "phoenix")
	if fallback.Visual != "phoenix" {
		t.Fatalf("an absent visual should fall back to the query; got %q", fallback.Visual)
	}

	silent := explicitQuery(url.Values{"person": {"Phoenix"}, "visual": {""}}, "phoenix")
	if silent.Visual != "" {
		t.Fatalf("an empty visual should stay empty; got %q", silent.Visual)
	}
}

// Exactly one level of place, decided by which parameter was sent. Matching
// "California" against place_city would find nothing while looking like it
// worked, which is why these are three parameters rather than one.
func TestExplicitQueryPlaceKeepsItsLevel(t *testing.T) {
	for _, param := range []string{"city", "admin1", "country"} {
		q := explicitQuery(url.Values{param: {"somewhere"}}, "")
		if q.Place == nil {
			t.Fatalf("%s: no place", param)
		}
		got := map[string]string{
			"city": q.Place.City, "admin1": q.Place.Admin1, "country": q.Place.Country,
		}
		for level, value := range got {
			want := ""
			if level == param {
				want = "somewhere"
			}
			if value != want {
				t.Fatalf("%s: %s = %q, want %q", param, level, value, want)
			}
		}
	}
}

// A date the grammar could not have produced is left alone rather than guessed
// at, which keeps a hand-edited URL from silently narrowing to nothing.
func TestExplicitQueryIgnoresUnreadableDates(t *testing.T) {
	q := explicitQuery(url.Values{"after": {"last tuesday"}}, "")
	if q.After != nil {
		t.Fatalf("after = %v, want nil", q.After)
	}
}
