package searchquery

import (
	"testing"
	"time"
)

// The vocabulary of the real archive, cut down to what these tests need. Real
// names and real places on purpose: the grammar only recognises what the
// library contains, so a fixture of invented ones would be testing a different
// parser.
func testVocab() Vocabulary {
	return Vocabulary{
		People: []string{"Dominic", "Phoenix", "Jonathan", "Chris Morrison", "Bo Lago"},
		Cities: []Place{
			{City: "Breckenridge", Admin1: "Colorado", Country: "United States"},
			{City: "New York City", Admin1: "New York", Country: "United States"},
			{City: "Moraga", Admin1: "California", Country: "United States"},
			{City: "San Francisco", Admin1: "California", Country: "United States"},
			{City: "South Lake Tahoe", Admin1: "California", Country: "United States"},
		},
		Admin1s:   []Place{{Admin1: "California"}, {Admin1: "New York"}, {Admin1: "Colorado"}},
		Countries: []Place{{Country: "United States"}, {Country: "Mexico"}},
		Tags:      map[string]string{"dog": "dog", "puppy": "dog", "snow": "snow"},
	}
}

// now is a Thursday in the middle of a summer that has not finished, which is
// the single most load-bearing fact in the date tests: it is what makes "last
// summer" mean the year before.
var now = time.Date(2026, time.August, 20, 15, 4, 0, 0, time.UTC)

func date(y int, m time.Month, d int) time.Time { return day(y, m, d) }

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  Query
	}{
		{
			// ML_IMAGES.md §2's worked example, and the reason the whole
			// feature is split in two.
			name:  "the worked example",
			query: "phoenix at the beach last summer",
			want: Query{
				People: []string{"Phoenix"},
				After:  ptr(date(2025, time.June, 1)),
				Before: ptr(date(2025, time.September, 30)),
				Visual: "at the beach",
			},
		},
		{
			name:  "a fully structured query has no visual half",
			query: "videos from 2019",
			want: Query{
				Kind:   "video",
				After:  ptr(date(2019, time.January, 1)),
				Before: ptr(date(2019, time.December, 31)),
			},
		},
		{
			// The rule matchFacets exists for: this is a question about a
			// beach, not an instruction to hide every video in the archive.
			name:  "photos is framing, not a filter",
			query: "show me photos of the beach",
			want:  Query{Visual: "the beach"},
		},
		{
			name:  "and photos is a filter when it says so",
			query: "only photos of the beach",
			want:  Query{Kind: "image", Visual: "the beach"},
		},
		{
			name:  "a stranded me goes without stranding the preposition",
			query: "photos of me at the beach",
			want:  Query{Visual: "at the beach"},
		},
		{
			name:  "a holiday is a week, and a city is a city",
			query: "christmas 2019 in breckenridge",
			want: Query{
				Place:  &Place{City: "Breckenridge", Admin1: "Colorado", Country: "United States"},
				After:  ptr(date(2019, time.December, 20)),
				Before: ptr(date(2019, time.December, 26)),
				Visual: "",
			},
		},
		{
			name:  "longest match keeps a city out of its own state",
			query: "new york city",
			want:  Query{Place: &Place{City: "New York City", Admin1: "New York", Country: "United States"}},
		},
		{
			name:  "and the state when the city was not named",
			query: "new york",
			want:  Query{Place: &Place{Admin1: "New York"}},
		},
		{
			name:  "a two-word name",
			query: "chris morrison skiing",
			want:  Query{People: []string{"Chris Morrison"}, Visual: "skiing"},
		},
		{
			name:  "two people is an and",
			query: "phoenix and dominic",
			want:  Query{People: []string{"Phoenix", "Dominic"}, Visual: ""},
		},
		{
			// Not a filter. The word falls through to the visual phrase, where
			// the encoder can find a dog nobody wrote "dog" about and the
			// tsvector ranks the ones somebody did. See matchVocabulary.
			name:  "a tag in prose stays in the prose",
			query: "puppy in the snow",
			want:  Query{Visual: "puppy in the snow"},
		},
		{
			// And the explicit form, which is a filter, and resolves the merge.
			name:  "an explicit tag is a filter",
			query: "tag:puppy in the snow",
			want:  Query{Tags: []string{"dog"}, Visual: "in the snow"},
		},
		{
			name:  "explicit fields, generally",
			query: "person:chris_morrison place:breckenridge after:2019-01-01 kind:video",
			want: Query{
				People: []string{"Chris Morrison"},
				Place:  &Place{City: "Breckenridge", Admin1: "Colorado", Country: "United States"},
				After:  ptr(date(2019, time.January, 1)),
				Kind:   "video",
			},
		},
		{
			// A value the archive does not hold is left where it was typed
			// rather than turned into a filter that matches nothing.
			name:  "an unknown tag is not a filter",
			query: "tag:banana",
			want:  Query{Visual: "tag:banana"},
		},
		{
			name:  "a category and a person combine",
			query: "screenshots",
			want:  Query{Category: "screenshots"},
		},
		{
			name:  "favourites, spelled either way",
			query: "favourite videos",
			want:  Query{Favorites: true, Kind: "video"},
		},
		{
			name:  "a possessive still finds the name",
			query: "phoenix's birthday",
			want:  Query{People: []string{"Phoenix"}, Visual: "birthday"},
		},
		{
			name:  "an empty box narrows nothing",
			query: "   ",
			want:  Query{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.query, testVocab(), now)
			assertQuery(t, got, tt.want)
		})
	}
}

func TestParseDates(t *testing.T) {
	tests := []struct {
		name          string
		query         string
		after, before *time.Time
	}{
		{"a bare year", "2019", ptr(date(2019, time.January, 1)), ptr(date(2019, time.December, 31))},
		{"an iso day", "2019-06-01", ptr(date(2019, time.June, 1)), ptr(date(2019, time.June, 1))},
		{"an iso month", "2019-06", ptr(date(2019, time.June, 1)), ptr(date(2019, time.June, 30))},
		{"a decade", "the 2010s", ptr(date(2010, time.January, 1)), ptr(date(2019, time.December, 31))},
		{"two spellings of a decade", "90s", ptr(date(1990, time.January, 1)), ptr(date(1999, time.December, 31))},

		{"today", "today", ptr(date(2026, time.August, 20)), ptr(date(2026, time.August, 20))},
		{"yesterday", "yesterday", ptr(date(2026, time.August, 19)), ptr(date(2026, time.August, 19))},
		{"last night is two days", "last night", ptr(date(2026, time.August, 19)), ptr(date(2026, time.August, 20))},

		// The 20th of August 2026 is a Thursday, so this week began on the
		// 17th.
		{"this week", "this week", ptr(date(2026, time.August, 17)), ptr(date(2026, time.August, 23))},
		{"last week is the calendar one", "last week", ptr(date(2026, time.August, 10)), ptr(date(2026, time.August, 16))},
		{"past week is a rolling one", "past week", ptr(date(2026, time.August, 13)), ptr(date(2026, time.August, 20))},
		{"last weekend", "last weekend", ptr(date(2026, time.August, 15)), ptr(date(2026, time.August, 16))},

		{"last month", "last month", ptr(date(2026, time.July, 1)), ptr(date(2026, time.July, 31))},
		{"last year", "last year", ptr(date(2025, time.January, 1)), ptr(date(2025, time.December, 31))},
		{"last three months rolls", "last 3 months", ptr(date(2026, time.May, 20)), ptr(date(2026, time.August, 20))},
		{"spelled counts too", "past two weeks", ptr(date(2026, time.August, 6)), ptr(date(2026, time.August, 20))},

		// The whole reason resolveYear distinguishes "over" from "begun".
		{"last summer is the one that finished", "last summer", ptr(date(2025, time.June, 1)), ptr(date(2025, time.September, 30))},
		{"this summer is the one happening", "this summer", ptr(date(2026, time.June, 1)), ptr(date(2026, time.September, 30))},
		{"a bare summer is the one begun", "summer", ptr(date(2026, time.June, 1)), ptr(date(2026, time.September, 30))},
		{"winter is named by the year it ends in", "winter 2020", ptr(date(2019, time.December, 1)), ptr(date(2020, time.March, 31))},

		{"a bare month that has been", "june", ptr(date(2026, time.June, 1)), ptr(date(2026, time.June, 30))},
		{"a bare month that has not", "december", ptr(date(2025, time.December, 1)), ptr(date(2025, time.December, 31))},
		{"a month and a year", "june 2019", ptr(date(2019, time.June, 1)), ptr(date(2019, time.June, 30))},
		{"a month, a day and a year", "june 5th 2019", ptr(date(2019, time.June, 5)), ptr(date(2019, time.June, 5))},
		{"of, between a month and its year", "summer of 2019", ptr(date(2019, time.June, 1)), ptr(date(2019, time.September, 30))},

		{"before is exclusive", "before 2020", nil, ptr(date(2019, time.December, 31))},
		{"until is not", "until 2020", nil, ptr(date(2020, time.December, 31))},
		{"since is inclusive", "since 2020", ptr(date(2020, time.January, 1)), nil},
		{"after is exclusive", "after 2020", ptr(date(2021, time.January, 1)), nil},
		{"between", "between 2019 and 2021", ptr(date(2019, time.January, 1)), ptr(date(2021, time.December, 31))},
		{"two bare years widen", "2019 2021", ptr(date(2019, time.January, 1)), ptr(date(2021, time.December, 31))},

		{"thanksgiving is the long weekend", "thanksgiving 2019", ptr(date(2019, time.November, 27)), ptr(date(2019, time.December, 1))},
		{"new year is named by the year it arrives in", "new years 2020", ptr(date(2019, time.December, 30)), ptr(date(2020, time.January, 2))},
		{"the fourth of july", "fourth of july 2021", ptr(date(2021, time.July, 2)), ptr(date(2021, time.July, 5))},

		// The words that are also ordinary English, and are not dates without
		// something saying so.
		{"a bare may is not a month", "may", nil, nil},
		{"a may with a year is", "may 2019", ptr(date(2019, time.May, 1)), ptr(date(2019, time.May, 31))},
		{"a bare fall is not a season", "fall", nil, nil},
		{"a fall with a year is", "fall 2019", ptr(date(2019, time.September, 1)), ptr(date(2019, time.December, 15))},
		{"a fall with a qualifier is", "last fall", ptr(date(2025, time.September, 1)), ptr(date(2025, time.December, 15))},
		{"a bare spring is not a season", "spring", nil, nil},
		{"a number that is not a year", "42", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.query, testVocab(), now)
			assertDate(t, "after", got.After, tt.after)
			assertDate(t, "before", got.Before, tt.before)
		})
	}
}

// A range that came out backwards is dropped whole rather than half-applied,
// because half of a wrong range silently excludes the archive while showing a
// chip that reads reasonably.
func TestBackwardsRangeIsDropped(t *testing.T) {
	got := Parse("between 2021 and 2019", testVocab(), now)
	if got.After != nil || got.Before != nil {
		t.Fatalf("expected no range, got %v..%v", got.After, got.Before)
	}
}

func TestMergeKeepsTheGrammarAndFillsGaps(t *testing.T) {
	vocab := testVocab()

	t.Run("the model may not move what the grammar read", func(t *testing.T) {
		grammar := Parse("phoenix last summer", vocab, now)
		merged := Merge(grammar, &ModelQuery{
			People: []string{"Dominic"},
			After:  "2024-01-01",
			Before: "2024-12-31",
		}, vocab, now)

		if len(merged.People) != 1 || merged.People[0] != "Phoenix" {
			t.Errorf("people = %v, want [Phoenix]", merged.People)
		}
		if !merged.After.Equal(date(2025, time.June, 1)) {
			t.Errorf("after = %v, want the grammar's 2025-06-01", merged.After)
		}
		if merged.Source != SourceGrammar {
			t.Errorf("source = %q, want %q", merged.Source, SourceGrammar)
		}
	})

	t.Run("but may fill a gap in words the archive holds", func(t *testing.T) {
		grammar := Parse("the snowy one from the ski trip with chris", vocab, now)
		if len(grammar.People) != 0 {
			t.Fatalf("the grammar was not supposed to find a person: %v", grammar.People)
		}
		merged := Merge(grammar, &ModelQuery{People: []string{"Chris Morrison"}}, vocab, now)
		if len(merged.People) != 1 || merged.People[0] != "Chris Morrison" {
			t.Errorf("people = %v, want [Chris Morrison]", merged.People)
		}
		if merged.Source != SourceModel {
			t.Errorf("source = %q, want %q", merged.Source, SourceModel)
		}
	})

	// The failure that made mentions() exist. A 0.6B model handed the archive's
	// people as a spelling hint returns the whole list on every query — and
	// every name on it passes a vocabulary check, because they are all real
	// people here. Six ANDed people is a filter that matches nothing, which is
	// §11's silent exclusion arriving through the front door.
	t.Run("a parroted hint list is not a filter", func(t *testing.T) {
		grammar := Parse("that receipt from the ski trip", vocab, now)
		merged := Merge(grammar, &ModelQuery{
			People: []string{"Dominic", "Phoenix", "Jonathan", "Chris Morrison", "Bo Lago"},
		}, vocab, now)
		if len(merged.People) != 0 {
			t.Errorf("people = %v; nobody was mentioned", merged.People)
		}
	})

	// And the same model, on a query that does mention somebody, contributing
	// the one thing it is actually for: the archive's spelling of a loose name.
	t.Run("a name the query mentions survives the same gate", func(t *testing.T) {
		grammar := Parse("skiing with chris", vocab, now)
		merged := Merge(grammar, &ModelQuery{
			People: []string{"Dominic", "Chris Morrison", "Bo Lago"},
		}, vocab, now)
		if len(merged.People) != 1 || merged.People[0] != "Chris Morrison" {
			t.Errorf("people = %v, want just [Chris Morrison]", merged.People)
		}
	})

	// A model with no clock and a prompt full of date fields answers "today" to
	// questions with no date in them. A range is the one field where being
	// wrong removes photographs rather than adding them.
	t.Run("a date is refused when the query says nothing about time", func(t *testing.T) {
		merged := Merge(Parse("screenshots about taxes", vocab, now), &ModelQuery{
			After: "2026-08-20", Before: "2026-08-20",
		}, vocab, now)
		if merged.After != nil || merged.Before != nil {
			t.Errorf("range = %v..%v, want none", merged.After, merged.Before)
		}
	})

	// But taken when the query is about time and the grammar had no rule for
	// the phrasing, which is the case the model is worth asking about.
	t.Run("a date is taken when the query is about time", func(t *testing.T) {
		grammar := Parse("skiing the year we moved house", vocab, now)
		if grammar.After != nil {
			t.Fatalf("the grammar read a range out of this; the gate is not what is under test")
		}
		merged := Merge(grammar, &ModelQuery{After: "2019-12-01", Before: "2020-03-31"}, vocab, now)
		if merged.After == nil || !merged.After.Equal(date(2019, time.December, 1)) {
			t.Errorf("after = %v, want 2019-12-01", merged.After)
		}
	})

	t.Run("a place nobody has photographed is dropped", func(t *testing.T) {
		merged := Merge(Parse("at the beach", vocab, now), &ModelQuery{Place: "beach"}, vocab, now)
		if merged.Place != nil {
			t.Errorf("place = %+v, want none", *merged.Place)
		}
	})

	t.Run("a person the archive does not hold is dropped", func(t *testing.T) {
		grammar := Parse("skiing", vocab, now)
		merged := Merge(grammar, &ModelQuery{People: []string{"Gandalf"}}, vocab, now)
		if len(merged.People) != 0 {
			t.Errorf("people = %v, want none", merged.People)
		}
		if merged.Source != SourceGrammar {
			t.Errorf("source = %q; nothing survived, so nothing was contributed", merged.Source)
		}
	})

	t.Run("half a range is no range", func(t *testing.T) {
		merged := Merge(Parse("skiing", vocab, now), &ModelQuery{
			After: "2021-01-01", Before: "2019-01-01",
		}, vocab, now)
		if merged.After != nil || merged.Before != nil {
			t.Errorf("range = %v..%v, want none", merged.After, merged.Before)
		}
	})

	t.Run("a visual phrase nobody typed is refused", func(t *testing.T) {
		grammar := Parse("the snowy one with chris", vocab, now)
		merged := Merge(grammar, &ModelQuery{
			People: []string{"Chris Morrison"},
			Visual: "a dog wearing sunglasses",
		}, vocab, now)
		if merged.Visual == "a dog wearing sunglasses" {
			t.Error("the model invented a phrase and it was believed")
		}
	})

	t.Run("a visual phrase that was typed is taken", func(t *testing.T) {
		grammar := Parse("the snowy one with chris", vocab, now)
		merged := Merge(grammar, &ModelQuery{
			People: []string{"Chris Morrison"},
			Visual: "snowy",
		}, vocab, now)
		if merged.Visual != "snowy" {
			t.Errorf("visual = %q, want %q", merged.Visual, "snowy")
		}
	})
}

func ptr[T any](v T) *T { return &v }

func assertDate(t *testing.T, name string, got, want *time.Time) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil:
		t.Errorf("%s = nil, want %s", name, want.Format("2006-01-02"))
	case want == nil:
		t.Errorf("%s = %s, want nil", name, got.Format("2006-01-02"))
	case !got.Equal(*want):
		t.Errorf("%s = %s, want %s", name, got.Format("2006-01-02"), want.Format("2006-01-02"))
	}
}

func assertQuery(t *testing.T, got, want Query) {
	t.Helper()
	assertStrings(t, "people", got.People, want.People)
	assertStrings(t, "tags", got.Tags, want.Tags)
	assertDate(t, "after", got.After, want.After)
	assertDate(t, "before", got.Before, want.Before)

	if got.Visual != want.Visual {
		t.Errorf("visual = %q, want %q", got.Visual, want.Visual)
	}
	if got.Kind != want.Kind {
		t.Errorf("kind = %q, want %q", got.Kind, want.Kind)
	}
	if got.Category != want.Category {
		t.Errorf("category = %q, want %q", got.Category, want.Category)
	}
	if got.Favorites != want.Favorites {
		t.Errorf("favorites = %v, want %v", got.Favorites, want.Favorites)
	}
	switch {
	case got.Place == nil && want.Place == nil:
	case got.Place == nil:
		t.Errorf("place = nil, want %+v", *want.Place)
	case want.Place == nil:
		t.Errorf("place = %+v, want nil", *got.Place)
	case *got.Place != *want.Place:
		t.Errorf("place = %+v, want %+v", *got.Place, *want.Place)
	}
}

func assertStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
	}
}
