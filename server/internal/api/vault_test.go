package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// The vault over HTTP: the same walk somebody takes through the gallery, in the
// order they take it. Choose a password, archive a photograph, watch it leave
// the library, find it locked, unlock it, and take it back out.
//
// It runs against the real routing table rather than calling the handlers,
// because half of what is being asserted here is *which* status comes back —
// 423 and 428 are what the gallery turns into a password prompt, and a handler
// answering the right thing on the wrong route would be invisible to a unit
// test and a dead end in a browser.

// hidden uploads one photograph and puts it in a bucket, returning its id.
func (h *harness) hidden(t *testing.T, bucket string, body []byte) string {
	t.Helper()

	id := decodeUpload(t, h.upload(t, body, nil)).ID
	resp := h.postJSON(t, "/v1/vault/"+bucket, fmt.Sprintf(`{"ids":[%q]}`, id))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("hide returned %d: %s", resp.StatusCode, out)
	}
	return id
}

func TestArchivingTakesAPhotoOutOfTheLibraryAndTheVaultNeedsAPassword(t *testing.T) {
	h := newHarness(t)

	// Nothing has ever been hidden, so there is no vault and hiding says so
	// rather than failing: 428 is what the gallery turns into "choose a
	// password", and it is the whole of how the vault comes into existence.
	id := decodeUpload(t, h.upload(t, loadFixture(t), nil)).ID
	resp := h.postJSON(t, "/v1/vault/archive", fmt.Sprintf(`{"ids":[%q]}`, id))
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("hiding into an archive with no vault returned %d, want 428", resp.StatusCode)
	}

	if resp = h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup returned %d: %s", resp.StatusCode, out)
	}
	if status := h.vaultStatus(t); !status.Exists || !status.Unlocked {
		t.Fatalf("after setup the vault is %+v, want it to exist and be open", status)
	}

	// Now the same request works, and the photograph leaves the library.
	resp = h.postJSON(t, "/v1/vault/archive", fmt.Sprintf(`{"ids":[%q]}`, id))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("hide returned %d: %s", resp.StatusCode, out)
	}
	var hid struct {
		Batch string `json:"batch"`
		Moved int    `json:"moved"`
	}
	decode(t, resp, &hid)
	if hid.Moved != 1 || hid.Batch == "" {
		t.Fatalf("hide reported %+v, want one item and a batch to undo it with", hid)
	}

	if n := h.timelineTotal(t, "/v1/timeline/days"); n != 0 {
		t.Errorf("the library still holds %d items", n)
	}
	// And it is not in the trash either: the vault is not reachable through it.
	if n := h.timelineTotal(t, "/v1/timeline/days?trash=1"); n != 0 {
		t.Errorf("the trash holds %d items after an archive", n)
	}
	if n := h.timelineTotal(t, "/v1/vault/archive/timeline/days"); n != 1 {
		t.Errorf("the archive holds %d items, want 1", n)
	}
}

func TestALockedVaultAnswersNothing(t *testing.T) {
	h := newHarness(t)

	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	id := h.hidden(t, "hidden", loadFixture(t))

	if resp := h.postJSON(t, "/v1/vault/lock", `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock returned %d", resp.StatusCode)
	}

	// Every read, including the thumbnail the grid would draw. 423 rather than
	// 404: there is something here and it is shut, which is a different sentence
	// and a different thing for the client to do about it.
	for _, path := range []string{
		"/v1/vault/hidden/collections",
		"/v1/vault/hidden/timeline",
		"/v1/vault/hidden/timeline/days",
		"/v1/assets/" + id + "/thumb",
		"/v1/assets/" + id + "/original",
		"/v1/assets/" + id,
	} {
		resp := h.get(t, path)
		if resp.StatusCode != http.StatusLocked {
			t.Errorf("GET %s returned %d, want 423", path, resp.StatusCode)
		}
	}

	// Restoring is a read too — it has to decrypt to write back — so it is
	// refused for the same reason.
	if resp := h.postJSON(t, "/v1/vault/restore", fmt.Sprintf(`{"bucket":"hidden","ids":[%q]}`, id)); resp.StatusCode != http.StatusLocked {
		t.Errorf("restore on a locked vault returned %d, want 423", resp.StatusCode)
	}

	// Hiding is not, and that is the asymmetry the whole design exists for.
	second := decodeUpload(t, h.upload(t, loadNamedFixture(t, "photo.jpg"), nil)).ID
	if resp := h.postJSON(t, "/v1/vault/hidden", fmt.Sprintf(`{"ids":[%q]}`, second)); resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Errorf("hiding on a locked vault returned %d: %s", resp.StatusCode, out)
	}

	// The wrong password does not open it, and the right one does.
	if resp := h.postJSON(t, "/v1/vault/unlock", `{"password":"not the password"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong password returned %d, want 403", resp.StatusCode)
	}
	if h.vaultStatus(t).Unlocked {
		t.Error("a rejected password left the vault open")
	}
	if resp := h.postJSON(t, "/v1/vault/unlock", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock returned %d", resp.StatusCode)
	}
	if n := h.timelineTotal(t, "/v1/vault/hidden/timeline/days"); n != 2 {
		t.Errorf("the bucket holds %d items, want the one hidden before the lock and the one hidden during it", n)
	}
}

func TestTheUndoInTheToastPutsAHiddenPhotoBack(t *testing.T) {
	h := newHarness(t)

	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	id := decodeUpload(t, h.upload(t, loadFixture(t), nil)).ID

	resp := h.postJSON(t, "/v1/vault/archive", fmt.Sprintf(`{"ids":[%q]}`, id))
	var hid struct {
		Batch string `json:"batch"`
	}
	decode(t, resp, &hid)

	// By batch, not by id: the toast is the only place the operation is named,
	// and by the time Undo is clicked the grid has been redrawn.
	resp = h.postJSON(t, "/v1/vault/restore", fmt.Sprintf(`{"batch":%q}`, hid.Batch))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("undo returned %d: %s", resp.StatusCode, out)
	}
	var back struct {
		Restored int `json:"restored"`
	}
	decode(t, resp, &back)
	if back.Restored != 1 {
		t.Errorf("the undo restored %d, want 1", back.Restored)
	}
	if n := h.timelineTotal(t, "/v1/timeline/days"); n != 1 {
		t.Errorf("the library holds %d items after the undo, want 1", n)
	}
	if n := h.timelineTotal(t, "/v1/vault/archive/timeline/days"); n != 0 {
		t.Errorf("the archive still holds %d items", n)
	}
}

// The two rows on the collections page say nothing about their contents until
// the password is in hand — not even how much is in them.
func TestTheCollectionsPageOnlyCountsTheVaultWhenItIsOpen(t *testing.T) {
	h := newHarness(t)

	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	h.hidden(t, "archive", loadFixture(t))

	var open struct {
		Vault map[string]int `json:"vault"`
	}
	decode(t, h.get(t, "/v1/collections"), &open)
	if open.Vault["archive"] != 1 {
		t.Errorf("an open vault reported %v, want one item in the archive", open.Vault)
	}

	h.postJSON(t, "/v1/vault/lock", `{}`)

	var shut struct {
		Vault map[string]int `json:"vault"`
	}
	decode(t, h.get(t, "/v1/collections"), &shut)
	if shut.Vault != nil {
		t.Errorf("a locked vault reported %v, want the counts withheld entirely", shut.Vault)
	}
}

func TestAnUnknownBucketIsNotAVault(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/v1/vault/secret/timeline", "/v1/vault/trash/collections"} {
		if resp := h.get(t, path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", path, resp.StatusCode)
		}
	}
}

// --- plumbing -------------------------------------------------------------

type vaultStatusBody struct {
	Exists   bool `json:"exists"`
	Unlocked bool `json:"unlocked"`
}

func (h *harness) vaultStatus(t *testing.T) vaultStatusBody {
	t.Helper()
	var out vaultStatusBody
	decode(t, h.get(t, "/v1/vault"), &out)
	return out
}

// timelineTotal reads a day table's total, which is how every count in this
// file is asked for: it is the same number the gallery draws its scrollbar from.
func (h *harness) timelineTotal(t *testing.T, path string) int {
	t.Helper()
	var table struct {
		Total int `json:"total"`
	}
	decode(t, h.get(t, path), &table)
	return table.Total
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s returned %d: %s", resp.Request.URL.Path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", resp.Request.URL.Path, err)
	}
}

// The order the two halves of an album restore run in is load-bearing, and this
// is the test that says so.
//
// A photograph rejoins an album only if that album is back in the library, so
// restoring the photographs while the album row is still marked hidden puts
// them in the library and leaves the album empty — which is the wrong half of
// "it goes back where it was", and is what this did before the two were swapped.
func TestRestoringAHiddenAlbumPutsThePhotosBackIntoIt(t *testing.T) {
	h := newHarness(t)

	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}

	id := decodeUpload(t, h.upload(t, loadFixture(t), nil)).ID
	h.describe(t, id, "Mock Album")

	album := h.oneAlbum(t)
	resp := h.postJSON(t, "/v1/vault/hidden/albums/"+album, `{}`)
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("hide album returned %d: %s", resp.StatusCode, out)
	}

	// Gone from the collections page along with its photographs, and present on
	// the bucket's own.
	if albums := h.albumCounts(t, "/v1/collections"); len(albums) != 0 {
		t.Errorf("the library still lists %v", albums)
	}
	if albums := h.albumCounts(t, "/v1/vault/hidden/collections"); albums["Mock Album"] != 1 {
		t.Errorf("the bucket lists %v, want the album with its photo in it", albums)
	}

	resp = h.postJSON(t, "/v1/vault/restore", fmt.Sprintf(`{"bucket":"hidden","album":%q}`, album))
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("restore album returned %d: %s", resp.StatusCode, out)
	}

	if albums := h.albumCounts(t, "/v1/collections"); albums["Mock Album"] != 1 {
		t.Errorf("the restored album lists %v, want its photo back in it", albums)
	}
}

// A photograph taken out of an album that is still hidden goes to the library
// and nowhere else — it must not quietly rejoin a hidden album. The same check
// in CommitUnvault settles both cases.
func TestRestoringOnePhotoOutOfAStillHiddenAlbumDoesNotRejoinIt(t *testing.T) {
	h := newHarness(t)

	if resp := h.postJSON(t, "/v1/vault/setup", `{"password":"a password for the vault"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	id := decodeUpload(t, h.upload(t, loadFixture(t), nil)).ID
	h.describe(t, id, "Mock Album")

	album := h.oneAlbum(t)
	if resp := h.postJSON(t, "/v1/vault/hidden/albums/"+album, `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("hide album returned %d", resp.StatusCode)
	}
	if resp := h.postJSON(t, "/v1/vault/restore", fmt.Sprintf(`{"bucket":"hidden","ids":[%q]}`, id)); resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("restore returned %d: %s", resp.StatusCode, out)
	}

	if n := h.timelineTotal(t, "/v1/timeline/days"); n != 1 {
		t.Errorf("the library holds %d items, want the restored photo", n)
	}
	if albums := h.albumCounts(t, "/v1/collections"); len(albums) != 0 {
		t.Errorf("the library lists %v; the album is still hidden and should stay off this page", albums)
	}
}

// describe attaches an import sidecar, which is the only way an album comes
// into existence in this archive.
func (h *harness) describe(t *testing.T, assetID, album string) {
	t.Helper()
	body := fmt.Sprintf(`{"source":"google-takeout","sidecar":{"title":"mock.heic"},"albums":[{"title":%q}]}`, album)
	resp := h.postJSON(t, "/v1/assets/"+assetID+"/import-metadata", body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("import-metadata returned %d: %s", resp.StatusCode, out)
	}
}

func (h *harness) oneAlbum(t *testing.T) string {
	t.Helper()
	var page struct {
		Albums []struct{ ID, Title string } `json:"albums"`
	}
	decode(t, h.get(t, "/v1/collections"), &page)
	if len(page.Albums) != 1 {
		t.Fatalf("the archive holds %d albums, want 1", len(page.Albums))
	}
	return page.Albums[0].ID
}

func (h *harness) albumCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	var page struct {
		Albums []struct {
			Title string `json:"title"`
			Count int    `json:"count"`
		} `json:"albums"`
	}
	decode(t, h.get(t, path), &page)
	out := map[string]int{}
	for _, a := range page.Albums {
		out[a.Title] = a.Count
	}
	return out
}
