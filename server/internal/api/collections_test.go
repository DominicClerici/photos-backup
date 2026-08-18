package api

import (
	"net/http"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

func TestCollectionsListsWhatAnImportCreated(t *testing.T) {
	h := newHarness(t)
	uploaded := decodeUpload(t, h.upload(t, loadFixture(t), nil))
	if resp := describeAsset(t, h, uploaded.ID, takeoutSidecar); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("import-metadata returned %d, want 204", resp.StatusCode)
	}

	var got db.Collections
	decodeJSON(t, h.get(t, "/v1/collections"), &got)

	if len(got.Albums) != 1 || got.Albums[0].Title != "Iceland 2025" {
		t.Fatalf("albums = %+v, want the one the sidecar named", got.Albums)
	}
	if got.Albums[0].CoverID != uploaded.ID {
		t.Errorf("album cover = %q, want the uploaded asset %q", got.Albums[0].CoverID, uploaded.ID)
	}
	if len(got.People) != 1 || got.People[0].Name != "Brody" {
		t.Fatalf("people = %+v, want Brody", got.People)
	}
	// takeoutSidecar sets favorited, and nothing else in this archive matches
	// any other category.
	if len(got.Categories) != 1 || got.Categories[0].Key != "favorites" {
		t.Errorf("categories = %+v, want favorites alone", got.Categories)
	}
}

func TestCollectionsAreEmptyArraysNotNull(t *testing.T) {
	h := newHarness(t)

	var got db.Collections
	decodeJSON(t, h.get(t, "/v1/collections"), &got)

	// A null here would make every section of the page special-case the archive
	// that has never been imported into — which is most of them.
	if got.Albums == nil || got.People == nil || got.Categories == nil {
		t.Errorf("a section came back null on an empty archive: %+v", got)
	}
}

func TestAlbumTimelineHoldsOnlyItsMembers(t *testing.T) {
	h := newHarness(t)
	inside := decodeUpload(t, h.upload(t, loadFixture(t), nil))
	h.upload(t, loadNamedFixture(t, "photo.jpg"), map[string]string{
		"X-Photo-Filename": "photo.jpg",
		"X-Photo-Local-Id": "photo.jpg",
	})
	if resp := describeAsset(t, h, inside.ID, takeoutSidecar); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("import-metadata returned %d, want 204", resp.StatusCode)
	}

	var collections db.Collections
	decodeJSON(t, h.get(t, "/v1/collections"), &collections)

	var page db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline?album="+collections.Albums[0].ID), &page)
	if len(page.Items) != 1 || page.Items[0].ID != inside.ID {
		t.Errorf("album timeline holds %d items, want only %q", len(page.Items), inside.ID)
	}

	var album db.Album
	decodeJSON(t, h.get(t, "/v1/collections/albums/"+collections.Albums[0].ID), &album)
	if album.Title != "Iceland 2025" || album.Count != 1 {
		t.Errorf("album lookup = %+v, want Iceland 2025 with 1 photo", album)
	}
}

func TestTimelineRefusesAFilterItCannotHonour(t *testing.T) {
	h := newHarness(t)

	for _, query := range []string{
		"?category=cats",               // no such category
		"?album=not-a-uuid",            // not an id at all
		"?person=A&category=favorites", // two collections at once
	} {
		if resp := h.get(t, "/v1/timeline"+query); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", query, resp.StatusCode)
		}
	}
}

func TestAlbumLookupIsNotFoundForAnUnknownID(t *testing.T) {
	h := newHarness(t)

	for _, id := range []string{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", "not-a-uuid"} {
		if resp := h.get(t, "/v1/collections/albums/"+id); resp.StatusCode != http.StatusNotFound {
			t.Errorf("album %q returned %d, want 404", id, resp.StatusCode)
		}
	}
}
