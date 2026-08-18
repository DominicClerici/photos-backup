package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Albums over HTTP: making one, filling it, and taking things back out — on
// both sides of the lock, because the two do the same thing to two completely
// different stores and only the route says which.

// sendJSON is postJSON with a method, for the one route here that is a DELETE
// carrying a selection. A selection is a body, and the verb that says "take
// these out" is the verb that cannot hold what to take out.
func (h *harness) sendJSON(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	h.authorize(req)
	return h.do(t, req)
}

// makeAlbum posts the create endpoint and returns the album it made.
func (h *harness) makeAlbum(t *testing.T, path, body string) struct {
	ID, Title, Description string
	Count, Added           int
} {
	t.Helper()
	resp := h.sendJSON(t, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusCreated {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("create returned %d: %s", resp.StatusCode, out)
	}
	var made struct {
		ID, Title, Description string
		Count, Added           int
	}
	if err := json.NewDecoder(resp.Body).Decode(&made); err != nil {
		t.Fatalf("decode the new album: %v", err)
	}
	return made
}

func TestCreatingAnAlbumAndFillingIt(t *testing.T) {
	h := newHarness(t)
	one := decodeUpload(t, h.upload(t, loadFixture(t), nil)).ID
	two := decodeUpload(t, h.upload(t, loadNamedFixture(t, "photo.jpg"), map[string]string{
		"X-Photo-Filename": "photo.jpg",
		"X-Photo-Local-Id": "photo.jpg",
	})).ID

	// Made from a selection, which is one request rather than two: an album
	// that exists and is empty because the second half failed is a mess
	// somebody then has to notice.
	made := h.makeAlbum(t, "/v1/collections/albums",
		fmt.Sprintf(`{"title":"Iceland","description":"Three weeks","ids":[%q]}`, one))
	if made.Added != 1 {
		t.Fatalf("added = %d, want the photo the request named", made.Added)
	}
	if made.Description != "Three weeks" {
		t.Errorf("description = %q, want what was typed", made.Description)
	}

	resp := h.postJSON(t, "/v1/collections/albums/"+made.ID+"/items", fmt.Sprintf(`{"ids":[%q]}`, two))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("add returned %d: %s", resp.StatusCode, out)
	}
	var added struct{ Added, Removed int }
	decode(t, resp, &added)
	if added.Added != 1 {
		t.Errorf("added = %d, want 1", added.Added)
	}

	if counts := h.albumCounts(t, "/v1/collections"); counts["Iceland"] != 2 {
		t.Errorf("the album holds %d, want both photos", counts["Iceland"])
	}

	// And the membership is readable per photo, which is what ticks the rows of
	// the "Add to album" menu.
	var held struct {
		Albums []string `json:"albums"`
	}
	decode(t, h.get(t, "/v1/assets/"+one+"/albums"), &held)
	if len(held.Albums) != 1 || held.Albums[0] != made.ID {
		t.Errorf("albums of the first photo = %v, want %s", held.Albums, made.ID)
	}

	resp = h.sendJSON(t, http.MethodDelete, "/v1/collections/albums/"+made.ID+"/items",
		fmt.Sprintf(`{"ids":[%q]}`, one))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("remove returned %d: %s", resp.StatusCode, out)
	}
	var removed struct{ Added, Removed int }
	decode(t, resp, &removed)
	if removed.Removed != 1 {
		t.Errorf("removed = %d, want 1", removed.Removed)
	}

	// Removed from the album, still in the library. That is the whole
	// difference between this and a delete.
	if counts := h.albumCounts(t, "/v1/collections"); counts["Iceland"] != 1 {
		t.Errorf("the album holds %d after a removal, want 1", counts["Iceland"])
	}
	if total := h.timelineTotal(t, "/v1/timeline/days"); total != 2 {
		t.Errorf("the library holds %d, want both photos untouched", total)
	}
}

func TestCreatingAnAlbumRefusesANameThatIsTaken(t *testing.T) {
	h := newHarness(t)
	h.makeAlbum(t, "/v1/collections/albums", `{"title":"Iceland"}`)

	resp := h.sendJSON(t, http.MethodPost, "/v1/collections/albums", `{"title":"Iceland"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a duplicate name returned %d, want 409", resp.StatusCode)
	}
}

func TestAnAlbumNeedsAName(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{`{"title":""}`, `{"title":"   "}`} {
		resp := h.sendJSON(t, http.MethodPost, "/v1/collections/albums", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", body, resp.StatusCode)
		}
	}
}

// The library's membership endpoint cannot reach an album in the vault. Its
// membership lives inside sealed documents, so writing an album_assets row for
// it would either do nothing or name a hidden photograph in the clear.
func TestTheLibraryCannotWriteIntoAVaultedAlbum(t *testing.T) {
	h := newHarness(t)
	id := decodeUpload(t, h.upload(t, loadFixture(t), nil)).ID
	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	made := h.makeAlbum(t, "/v1/vault/hidden/albums", `{"title":"Private"}`)

	resp := h.postJSON(t, "/v1/collections/albums/"+made.ID+"/items", fmt.Sprintf(`{"ids":[%q]}`, id))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("writing into a hidden album through the library returned %d, want 409", resp.StatusCode)
	}
}

func TestAlbumsInsideABucket(t *testing.T) {
	h := newHarness(t)
	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	id := h.hidden(t, "hidden", loadFixture(t))

	made := h.makeAlbum(t, "/v1/vault/hidden/albums",
		fmt.Sprintf(`{"title":"Private","ids":[%q]}`, id))
	if made.Added != 1 {
		t.Fatalf("added = %d, want the hidden photo the request named", made.Added)
	}

	if counts := h.albumCounts(t, "/v1/vault/hidden/collections"); counts["Private"] != 1 {
		t.Errorf("the hidden album holds %d, want 1", counts["Private"])
	}
	// It is not on the collections page. A hidden album's title on the library's
	// front page would be the one thing this feature exists to prevent.
	if counts := h.albumCounts(t, "/v1/collections"); len(counts) != 0 {
		t.Errorf("the library lists %v, want nothing", counts)
	}

	var held struct {
		Albums []string `json:"albums"`
	}
	decode(t, h.get(t, "/v1/assets/"+id+"/albums"), &held)
	if len(held.Albums) != 1 || held.Albums[0] != made.ID {
		t.Errorf("albums of the hidden photo = %v, want %s", held.Albums, made.ID)
	}

	resp := h.sendJSON(t, http.MethodDelete, "/v1/vault/hidden/albums/"+made.ID+"/items",
		fmt.Sprintf(`{"ids":[%q]}`, id))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("remove returned %d: %s", resp.StatusCode, out)
	}
	if counts := h.albumCounts(t, "/v1/vault/hidden/collections"); counts["Private"] != 0 {
		t.Errorf("the hidden album holds %d after a removal, want 0", counts["Private"])
	}
	// And the photograph is still hidden: what was removed is the grouping.
	if total := h.timelineTotal(t, "/v1/vault/hidden/timeline/days"); total != 1 {
		t.Errorf("the bucket holds %d, want the photo untouched", total)
	}
}

// The one filing operation a locked vault cannot do, and not by policy: the
// sealed document has to be opened before a line can be added to it.
func TestFilingInsideTheVaultNeedsThePassword(t *testing.T) {
	h := newHarness(t)
	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	id := h.hidden(t, "archive", loadFixture(t))
	made := h.makeAlbum(t, "/v1/vault/archive/albums", `{"title":"Kept"}`)

	if resp := h.postJSON(t, "/v1/vault/lock", `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock returned %d", resp.StatusCode)
	}

	resp := h.postJSON(t, "/v1/vault/archive/albums/"+made.ID+"/items", fmt.Sprintf(`{"ids":[%q]}`, id))
	if resp.StatusCode != http.StatusLocked {
		t.Fatalf("filing into a locked vault returned %d, want 423", resp.StatusCode)
	}
	// Reading the membership of a hidden photograph is a read of the same
	// document, so it is shut for the same reason.
	if resp := h.get(t, "/v1/assets/"+id+"/albums"); resp.StatusCode != http.StatusLocked {
		t.Fatalf("reading a hidden photo's albums while locked returned %d, want 423", resp.StatusCode)
	}
}

// A bucket is a boundary in both directions: an archived album is not a hidden
// one, and the route says which vault is being written to.
func TestABucketsAlbumsAreItsOwn(t *testing.T) {
	h := newHarness(t)
	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	id := h.hidden(t, "hidden", loadFixture(t))
	made := h.makeAlbum(t, "/v1/vault/archive/albums", `{"title":"Archived"}`)

	resp := h.postJSON(t, "/v1/vault/hidden/albums/"+made.ID+"/items", fmt.Sprintf(`{"ids":[%q]}`, id))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("filing into the other bucket's album returned %d, want 409", resp.StatusCode)
	}
}
