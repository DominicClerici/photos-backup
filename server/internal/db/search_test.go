package db

import (
	"context"
	"testing"
	"time"
)

func TestPutDescriptionWritesCaptionTagsAndSearchRow(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, store, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))

	if err := store.PutDescription(ctx, id, CaptionModel,
		"A golden retriever running along a beach at sunset",
		[]Tag{{Name: "Dog "}, {Name: "beach", Confidence: 0.9}, {Name: "dog"}},
	); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}

	// "Dog ", "dog" and "Dog" are one word. Three rows would be three entries
	// in the merge UI on the first day of a vocabulary.
	if got := tagsOf(t, store, id); len(got) != 2 {
		t.Errorf("tags = %v, want the two distinct ones", got)
	}
	// And the search row is written in the same transaction, because a
	// photograph findable by "dog" that cannot say why is a half-published
	// result with a job marked done behind it.
	if !matches(t, store, id, "golden retriever") {
		t.Error("the caption did not reach the tsvector")
	}
}

// The whole of the free-form-then-merge plan, in one test: the model wrote
// "puppy", somebody merged it into "dog", and both words now find the
// photograph without a single row being rewritten.
func TestAMergedTagIsFoundByEitherName(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, store, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))

	if err := store.PutDescription(ctx, id, CaptionModel, "a small dog", []Tag{{Name: "puppy"}}); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
	mergeTag(t, store, "puppy", "dog")
	if _, err := store.RefreshSearch(ctx, id); err != nil {
		t.Fatalf("RefreshSearch: %v", err)
	}

	// The raw claim is untouched — which is what makes the merge reversible.
	if got := tagsOf(t, store, id); len(got) != 1 || got[0] != "puppy" {
		t.Errorf("asset_tags = %v; the model's own word must survive a merge", got)
	}

	vocab, err := store.SearchVocabulary(ctx)
	if err != nil {
		t.Fatalf("SearchVocabulary: %v", err)
	}
	if vocab.Tags["puppy"] != "dog" {
		t.Errorf("vocabulary resolves puppy to %q, want dog", vocab.Tags["puppy"])
	}

	for _, name := range []string{"puppy", "dog"} {
		results, _, err := store.Search(ctx, SearchRequest{
			Filter: TimelineFilter{Tags: []string{"dog"}},
			Text:   name,
		})
		if err != nil {
			t.Fatalf("search %q: %v", name, err)
		}
		if len(results) != 1 || results[0].ID != id {
			t.Errorf("searching %q found %d results, want the one photograph", name, len(results))
		}
	}
}

// The degraded path from ML_IMAGES.md §7, which is the same statement with the
// vector left out rather than a second query engine.
func TestSearchWithoutAVectorStillRanksByText(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	beach := seedAsset(t, store, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	kitchen := seedAsset(t, store, 2, time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC))
	describe(t, store, beach, "a dog on a beach", "dog", "beach")
	describe(t, store, kitchen, "a kitchen counter", "kitchen")

	results, total, err := store.Search(ctx, SearchRequest{Text: "beach"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if total != 1 || len(results) != 1 || results[0].ID != beach {
		t.Fatalf("found %d results, want the beach", len(results))
	}
	if results[0].Score <= 0 {
		t.Error("a text-only match scored zero; the fusion dropped the only ranking there was")
	}
	if results[0].Similarity != nil {
		t.Error("a search with no vector reported a similarity to nothing")
	}
	if results[0].Caption != "a dog on a beach" {
		t.Errorf("caption = %q; a tile has to be able to say why it matched", results[0].Caption)
	}
}

// A query with no fuzzy half at all is a timeline, and answering it must not
// require photo-ml, a caption, or a tsvector — "videos from 2019" is a question
// this archive could always have answered.
func TestAStructuralQueryFallsBackToTheTimeline(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	old := seedAsset(t, store, 1, time.Date(2019, 3, 1, 12, 0, 0, 0, time.UTC))
	recent := seedAsset(t, store, 2, time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))

	after := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2019, 12, 31, 0, 0, 0, 0, time.UTC)
	results, total, err := store.Search(ctx, SearchRequest{
		Filter: TimelineFilter{After: &after, Before: &before},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if total != 1 || len(results) != 1 || results[0].ID != old {
		t.Fatalf("found %d results, want only the 2019 one (%s, not %s)", len(results), old, recent)
	}
}

// The end of a range is inclusive of the whole day it names, because that is
// what the chip says it is.
func TestTheLastDayOfARangeIsInIt(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	lateOnTheLastDay := seedAsset(t, store, 1, time.Date(2019, 6, 30, 23, 50, 0, 0, time.UTC))
	after := time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2019, 6, 30, 0, 0, 0, 0, time.UTC)

	page, err := store.Timeline(ctx, TimelineFilter{After: &after, Before: &before}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != lateOnTheLastDay {
		t.Fatalf("a photograph taken ten minutes before midnight on the last day fell outside the range")
	}
}

func TestPlaceAndPeopleNarrowTheSameTimeline(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	here := seedAsset(t, store, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	elsewhere := seedAsset(t, store, 2, time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC))
	if err := store.ApplyPlace(ctx, here, chicago()); err != nil {
		t.Fatalf("ApplyPlace: %v", err)
	}
	if err := store.ApplyPlace(ctx, elsewhere, Place{City: "Moraga", Admin1: "California", Country: "United States", Source: "geonames"}); err != nil {
		t.Fatalf("ApplyPlace: %v", err)
	}

	// A city filter compares the city column. The state it sits in is a
	// different question with a different answer, which is why Place carries a
	// level rather than a name.
	page, err := store.Timeline(ctx, TimelineFilter{Place: &Place{City: "Chicago"}}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != here {
		t.Fatalf("place=Chicago returned %d items, want the one", len(page.Items))
	}

	page, err = store.Timeline(ctx, TimelineFilter{Place: &Place{Admin1: "California"}}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != elsewhere {
		t.Fatalf("place=California returned %d items, want the one", len(page.Items))
	}
}

// The vault's objection is precisely to a legible description of what it is
// holding, and a caption is the most legible one this server ever writes.
func TestNothingIsWrittenAboutAVaultedPhotograph(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, store, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))

	describe(t, store, id, "a dog on a beach", "dog")
	if !matches(t, store, id, "dog") {
		t.Fatal("the description did not land in the first place")
	}

	if _, err := store.pool.Exec(ctx, `update assets set vault = 'hidden' where id = $1::uuid`, id); err != nil {
		t.Fatalf("hide: %v", err)
	}
	if err := store.PutDescription(ctx, id, CaptionModel, "a dog on a beach", []Tag{{Name: "dog"}}); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
	if _, err := store.RefreshSearch(ctx, id); err != nil {
		t.Fatalf("RefreshSearch: %v", err)
	}
	if matches(t, store, id, "dog") {
		t.Error("a hidden photograph is still searchable by what it shows")
	}
}

func describe(t *testing.T, store *Store, id, caption string, tags ...string) {
	t.Helper()
	list := make([]Tag, len(tags))
	for i, name := range tags {
		list[i] = Tag{Name: name, Confidence: 1}
	}
	if err := store.PutDescription(context.Background(), id, CaptionModel, caption, list); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
}

func tagsOf(t *testing.T, store *Store, id string) []string {
	t.Helper()
	rows, err := store.pool.Query(context.Background(),
		`select t.name from asset_tags at join tags t on t.id = at.tag_id
		 where at.asset_id = $1::uuid order by t.name`, id)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	var names []string
	if err := scanInto(rows, &names); err != nil {
		t.Fatalf("read tags: %v", err)
	}
	return names
}

func mergeTag(t *testing.T, store *Store, from, into string) {
	t.Helper()
	_, err := store.pool.Exec(context.Background(),
		`insert into tags (name) values ($1) on conflict (name) do nothing`, into)
	if err != nil {
		t.Fatalf("create canonical tag: %v", err)
	}
	if _, err := store.pool.Exec(context.Background(), `
		update tags set canonical_id = (select id from tags where name = $2)
		where name = $1`, from, into); err != nil {
		t.Fatalf("merge tag: %v", err)
	}
}

func matches(t *testing.T, store *Store, id, phrase string) bool {
	t.Helper()
	var found bool
	err := store.pool.QueryRow(context.Background(), `
		select exists (
			select 1 from asset_search
			where asset_id = $1::uuid and tsv @@ websearch_to_tsquery('english', $2))`,
		id, phrase).Scan(&found)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return found
}
