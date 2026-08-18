package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// The database half of the vault, with the encryption stubbed out: what these
// tests are about is the scrub, the memberships, and the restore putting both
// back. See internal/vault for the tests about the crypto.

// hide runs the two halves of a vault operation the way the service does, with
// the sealed document standing in for itself: the point of these tests is what
// the row and the membership tables look like afterwards, and a real seal would
// only make the document unreadable to the assertions.
func hide(t *testing.T, s *Store, bucket string, sel Selection) VaultResult {
	t.Helper()
	ctx := context.Background()

	candidates, err := s.VaultCandidates(ctx, sel)
	if err != nil {
		t.Fatalf("VaultCandidates: %v", err)
	}
	items := make([]SealedItem, len(candidates))
	for i, c := range candidates {
		items[i] = SealedItem{AssetID: c.AssetID, Sealed: c.Doc}
	}
	result, err := s.CommitVault(ctx, bucket, items)
	if err != nil {
		t.Fatalf("CommitVault: %v", err)
	}
	return result
}

// unhide is the same trick in reverse.
func unhide(t *testing.T, s *Store, ids []string) int {
	t.Helper()
	ctx := context.Background()

	rows, err := s.VaultSealed(ctx, ids)
	if err != nil {
		t.Fatalf("VaultSealed: %v", err)
	}
	items := make([]Restoration, 0, len(rows))
	for _, r := range rows {
		var doc struct {
			Asset  json.RawMessage `json:"asset"`
			Albums []struct {
				ID string `json:"id"`
			} `json:"albums"`
			People []string `json:"people"`
		}
		if err := json.Unmarshal(r.Sealed, &doc); err != nil {
			t.Fatalf("read sealed document: %v", err)
		}
		albums := make([]string, 0, len(doc.Albums))
		for _, a := range doc.Albums {
			albums = append(albums, a.ID)
		}
		items = append(items, Restoration{
			AssetID: r.AssetID, Asset: doc.Asset,
			AlbumIDs: albums, People: doc.People, Item: true,
		})
	}
	restored, err := s.CommitUnvault(ctx, items)
	if err != nil {
		t.Fatalf("CommitUnvault: %v", err)
	}
	return restored
}

func TestHidingTakesAPhotoOutOfTheLibraryAndTheTrash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 4)

	result := hide(t, s, VaultHidden, Selection{IDs: []string{ids[1]}})
	if result.Count != 1 {
		t.Fatalf("hid %d items, want 1", result.Count)
	}

	library := timelineIDs(t, s, TimelineFilter{})
	if !same(library, []string{ids[0], ids[2], ids[3]}) {
		t.Errorf("library = %v, want the other three", library)
	}
	// The trash is not a way around the vault: an item in one is out of both.
	if trash := timelineIDs(t, s, TimelineFilter{Trash: true}); len(trash) != 0 {
		t.Errorf("trash = %v, want nothing", trash)
	}

	counts, err := s.VaultCounts(ctx)
	if err != nil {
		t.Fatalf("VaultCounts: %v", err)
	}
	if counts[VaultHidden] != 1 || counts[VaultArchive] != 0 {
		t.Errorf("counts = %v, want one hidden and nothing archived", counts)
	}
}

// The scrub is the feature. A row left holding the filename would be a vault
// that tells you what is in it.
func TestHidingEmptiesTheRowAndRestoringFillsItBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, id, heicSidecar, AlbumRef{Title: "Iceland 2025"})

	before, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if before.OriginalFilename == "" || before.Description == "" {
		t.Fatal("the fixture has nothing worth hiding")
	}

	hide(t, s, VaultArchive, Selection{IDs: []string{id}})

	hidden, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load hidden asset: %v", err)
	}
	if hidden.Vault != VaultArchive {
		t.Errorf("vault = %q, want %q", hidden.Vault, VaultArchive)
	}
	for what, got := range map[string]string{
		"filename":     hidden.OriginalFilename,
		"extension":    hidden.Ext,
		"content type": hidden.ContentType,
		"description":  hidden.Description,
		"camera":       hidden.CameraMake,
	} {
		if got != "" {
			t.Errorf("the row still holds the %s: %q", what, got)
		}
	}
	if hidden.Favorite {
		t.Error("the row still says this was a favourite")
	}
	if hidden.GPSLat != nil || hidden.CapturedAt != nil {
		t.Error("the row still holds where and when the photo was taken")
	}
	// The two that deliberately stay: without them sync/check would offer the
	// photograph back to the phone on the next backup.
	if hidden.SHA256 != before.SHA256 || hidden.MD5 != before.MD5 || hidden.ByteSize != before.ByteSize {
		t.Error("the content key was scrubbed; the next backup would upload this again")
	}

	unhide(t, s, []string{id})

	back, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load restored asset: %v", err)
	}
	if back.Vault != "" {
		t.Errorf("vault = %q after a restore, want empty", back.Vault)
	}
	if back.OriginalFilename != before.OriginalFilename {
		t.Errorf("filename = %q, want %q", back.OriginalFilename, before.OriginalFilename)
	}
	if back.Description != before.Description {
		t.Errorf("description = %q, want %q", back.Description, before.Description)
	}
	if back.Favorite != before.Favorite || back.Ext != before.Ext {
		t.Error("the restore did not put the flags and the extension back")
	}
	if back.GPSLat == nil || before.GPSLat == nil || *back.GPSLat != *before.GPSLat {
		t.Error("the restore did not put the coordinates back")
	}
}

// "Removed from any albums, categories, or people it is under", and put back
// into the ones that still exist.
func TestHidingLeavesItsAlbumsAndPeopleAndRestoringRejoinsThem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	other := seedAsset(t, s, 2, time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, id, heicSidecar, AlbumRef{Title: "Iceland 2025"})
	applySidecar(t, s, other, heicSidecar, AlbumRef{Title: "Iceland 2025"})

	hide(t, s, VaultHidden, Selection{IDs: []string{id}})

	albums, err := s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 1 || albums[0].Count != 1 {
		t.Fatalf("album count = %v, want the album holding only the photo that is still in the library", albums)
	}

	var members int
	if err := s.pool.QueryRow(ctx,
		`select count(*)::int from album_assets where asset_id = $1::uuid`, id).Scan(&members); err != nil {
		t.Fatalf("count membership: %v", err)
	}
	if members != 0 {
		t.Errorf("the hidden photo is still in %d albums", members)
	}

	var tags int
	if err := s.pool.QueryRow(ctx,
		`select count(*)::int from asset_people where asset_id = $1::uuid`, id).Scan(&tags); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if tags != 0 {
		t.Errorf("the hidden photo is still tagged with %d people", tags)
	}

	unhide(t, s, []string{id})

	albums, err = s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums after restore: %v", err)
	}
	if len(albums) != 1 || albums[0].Count != 2 {
		t.Errorf("album count = %v, want both photos back in it", albums)
	}
	if err := s.pool.QueryRow(ctx,
		`select count(*)::int from asset_people where asset_id = $1::uuid`, id).Scan(&tags); err != nil {
		t.Fatalf("count tags after restore: %v", err)
	}
	if tags != 2 {
		t.Errorf("restored with %d people, want the two the sidecar named", tags)
	}
}

// The genuinely ambiguous case, settled the way the feature says: no warning,
// no resurrected album, the photograph simply comes back to the library.
func TestRestoringIntoAnAlbumThatIsGoneJustReturnsToTheLibrary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, id, heicSidecar, AlbumRef{Title: "Iceland 2025"})

	hide(t, s, VaultHidden, Selection{IDs: []string{id}})
	if _, err := s.pool.Exec(ctx, `delete from albums`); err != nil {
		t.Fatalf("delete the album: %v", err)
	}

	if restored := unhide(t, s, []string{id}); restored != 1 {
		t.Fatalf("restored %d, want 1", restored)
	}
	if library := timelineIDs(t, s, TimelineFilter{}); !same(library, []string{id}) {
		t.Errorf("library = %v, want the restored photo", library)
	}
	albums, err := s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 0 {
		t.Errorf("albums = %v, want the deleted one to stay deleted", albums)
	}
}

// Hiding twice must not re-stamp the batch, or the second operation's Undo
// would drag the first one's photographs back out too. Same guard the trash has.
func TestHidingAnOverlappingSelectionLeavesTheFirstBatchAlone(t *testing.T) {
	s := testStore(t)
	ids := seedDays(t, s, 4)

	first := hide(t, s, VaultHidden, Selection{IDs: []string{ids[0]}})
	second := hide(t, s, VaultHidden, Selection{IDs: []string{ids[0], ids[1]}})
	if second.Count != 1 {
		t.Errorf("the second operation counted %d, want only the one it actually moved", second.Count)
	}

	assets, _, _, err := s.VaultBatch(context.Background(), first.Batch)
	if err != nil {
		t.Fatalf("VaultBatch: %v", err)
	}
	if len(assets) != 1 || assets[0] != ids[0] {
		t.Errorf("the first batch names %v, want just the photo it hid", assets)
	}
}

// A hidden album and a hidden person are rows of their own, so that hiding one
// outlives the photographs it was about.
func TestHidingAPersonKeepsThemOutOfTheCollectionsPage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, id, heicSidecar)

	batch, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	if err := s.VaultPerson(ctx, "Brody", VaultHidden, batch); err != nil {
		t.Fatalf("VaultPerson: %v", err)
	}

	people, err := s.People(ctx, 50)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	for _, p := range people {
		if p.Name == "Brody" {
			t.Fatal("a hidden person is still on the collections page")
		}
	}

	hidden, err := s.VaultedPeople(ctx, VaultHidden)
	if err != nil {
		t.Fatalf("VaultedPeople: %v", err)
	}
	if len(hidden) != 1 || hidden[0] != "Brody" {
		t.Errorf("vaulted people = %v, want Brody", hidden)
	}

	if err := s.UnvaultPerson(ctx, "Brody"); err != nil {
		t.Fatalf("UnvaultPerson: %v", err)
	}
	people, err = s.People(ctx, 50)
	if err != nil {
		t.Fatalf("People after restore: %v", err)
	}
	found := false
	for _, p := range people {
		found = found || p.Name == "Brody"
	}
	if !found {
		t.Error("Brody did not come back to the collections page")
	}
}

func TestHidingAnAlbumTakesItOffTheCollectionsPage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, id, heicSidecar, AlbumRef{Title: "Iceland 2025"})

	albums, err := s.Albums(ctx)
	if err != nil || len(albums) != 1 {
		t.Fatalf("Albums: %v %v", albums, err)
	}

	candidates, err := s.VaultAlbumCandidates(ctx, albums[0].ID)
	if err != nil {
		t.Fatalf("VaultAlbumCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want the album's one photo", len(candidates))
	}

	batch, _ := NewBatch()
	if err := s.VaultAlbum(ctx, albums[0].ID, VaultArchive, batch); err != nil {
		t.Fatalf("VaultAlbum: %v", err)
	}

	albums, err = s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums after hiding: %v", err)
	}
	if len(albums) != 0 {
		t.Errorf("albums = %v, want the hidden one gone from the page", albums)
	}

	vaulted, err := s.VaultedAlbums(ctx, VaultArchive)
	if err != nil {
		t.Fatalf("VaultedAlbums: %v", err)
	}
	if len(vaulted) != 1 || vaulted[0].Title != "Iceland 2025" {
		t.Errorf("vaulted albums = %v, want Iceland 2025", vaulted)
	}
}

// A Live Photo's motion is not an item, and it must not be left behind in the
// library when its still is hidden. Same family rule the trash uses.
func TestHidingAStillCarriesItsMotion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	stillAsset, videoAsset := livePair(t, 0)
	still, _, err := s.RecordAsset(ctx, stillAsset)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}
	video, _, err := s.RecordAsset(ctx, videoAsset)
	if err != nil {
		t.Fatalf("record paired video: %v", err)
	}

	candidates, err := s.VaultCandidates(ctx, Selection{IDs: []string{still}})
	if err != nil {
		t.Fatalf("VaultCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want the still and its motion", len(candidates))
	}

	items := make([]SealedItem, len(candidates))
	found := false
	for i, c := range candidates {
		items[i] = SealedItem{AssetID: c.AssetID, Sealed: c.Doc}
		found = found || c.AssetID == video
	}
	if !found {
		t.Fatal("the paired video was not carried along")
	}

	result, err := s.CommitVault(ctx, VaultHidden, items)
	if err != nil {
		t.Fatalf("CommitVault: %v", err)
	}
	// One item, two rows: the count is in the units somebody selected in.
	if result.Count != 1 {
		t.Errorf("count = %d, want 1 item", result.Count)
	}

	var vaulted int
	if err := s.pool.QueryRow(ctx,
		`select count(*)::int from assets where vault <> ''`).Scan(&vaulted); err != nil {
		t.Fatalf("count vaulted rows: %v", err)
	}
	if vaulted != 2 {
		t.Errorf("%d rows are hidden, want both halves", vaulted)
	}
}
