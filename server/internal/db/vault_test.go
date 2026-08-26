package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
		items[i] = SealedItem{AssetID: c.AssetID, Sealed: c.Doc, SealedAnalysis: c.Analysis}
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
			AlbumIDs: albums, People: doc.People,
			Analysis: r.SealedAnalysis, Item: true,
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
		items[i] = SealedItem{AssetID: c.AssetID, Sealed: c.Doc, SealedAnalysis: c.Analysis}
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

// What the archive thinks a photograph is goes into the vault with it.
//
// The probe this test is written from: caption a photograph, read the text off
// it, tag it, embed it, then hide it — and then go looking in Postgres for any
// of it. A caption is a sentence in English saying what the picture is of and
// an OCR row is the account number off a bank statement, which is exactly what
// migration 0012 meant by "the metadata that would let somebody reconstruct
// what the picture was without ever seeing it".
func TestHidingTakesTheWordsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	describeForVault(t, s, id)

	hide(t, s, VaultHidden, Selection{IDs: []string{id}})

	if got := analysisRows(t, s, id); got != (analysisCounts{}) {
		t.Errorf("a hidden photograph still says %+v about itself", got)
	}
	if tsv := searchText(t, s, id); tsv != "" {
		t.Errorf("a hidden photograph is still in the search index as %q", tsv)
	}
	// And the document is what makes that affordable rather than destructive.
	var sealed []byte
	if err := s.pool.QueryRow(ctx,
		`select sealed_analysis from vault_items where asset_id = $1::uuid`, id).
		Scan(&sealed); err != nil {
		t.Fatalf("read the sealed analysis: %v", err)
	}
	if len(sealed) == 0 {
		t.Fatal("nothing was sealed, so the words were destroyed rather than hidden")
	}
}

// Restoring is the other half of the promise. A photograph that came back out
// of the vault without its caption would have paid for being hidden.
func TestRestoringGivesTheWordsBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	describeForVault(t, s, id)
	before, err := s.AssetAnalysis(ctx, id)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}

	hide(t, s, VaultHidden, Selection{IDs: []string{id}})
	unhide(t, s, []string{id})

	after, err := s.AssetAnalysis(ctx, id)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}
	if after.Caption != before.Caption || after.CaptionModel != before.CaptionModel {
		t.Errorf("caption came back as %q from %q, want %q from %q",
			after.Caption, after.CaptionModel, before.Caption, before.CaptionModel)
	}
	if after.CaptionedAt == nil || !after.CaptionedAt.Equal(*before.CaptionedAt) {
		t.Errorf("captioned_at came back as %v, want %v", after.CaptionedAt, before.CaptionedAt)
	}
	if after.Text != before.Text || after.TextModel != before.TextModel {
		t.Errorf("recognised text came back as %q from %q, want %q from %q",
			after.Text, after.TextModel, before.Text, before.TextModel)
	}
	if len(after.Tags) != len(before.Tags) {
		t.Fatalf("tags came back as %+v, want %+v", after.Tags, before.Tags)
	}
	for i := range after.Tags {
		if after.Tags[i] != before.Tags[i] {
			t.Errorf("tag %d came back as %+v, want %+v", i, after.Tags[i], before.Tags[i])
		}
	}
	if after.Frames != before.Frames || after.VisionModel != before.VisionModel {
		t.Errorf("frames came back as %d from %q, want %d from %q",
			after.Frames, after.VisionModel, before.Frames, before.VisionModel)
	}
	// The vector itself, not just the count: an embedding that came back as a
	// different 1152 numbers is a photograph that has quietly stopped being
	// findable by what it looks like.
	if got := vectorLiteral(t, s, id); got != VectorLiteral(unit(7)) {
		t.Errorf("the restored vector is not the one that was sealed")
	}
	// And the tsvector, which is rebuilt rather than carried.
	if tsv := searchText(t, s, id); tsv == "" || !strings.Contains(tsv, "sofa") {
		t.Errorf("search row came back as %q, want the caption in it", tsv)
	}
}

// The sweep, which is how an archive hidden under an older build catches up.
//
// Seeding it means writing the rows behind the guards' backs, because the
// guards are what stop this happening now: the state under test is one no
// current code path can produce and every existing vault is in.
func TestTheSweepSealsWordsAnOlderBuildLeftBehind(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	describeForVault(t, s, id)
	hide(t, s, VaultHidden, Selection{IDs: []string{id}})

	// Rewind to the leak: the document goes away and the rows come back.
	if _, err := s.pool.Exec(ctx,
		`update vault_items set sealed_analysis = null where asset_id = $1::uuid`, id); err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		insert into asset_descriptions (asset_id, model, caption)
		values ($1::uuid, $2, 'a man asleep on a sofa in a bedroom')`, id, CaptionModel); err != nil {
		t.Fatalf("re-leak the caption: %v", err)
	}

	left, err := s.VaultAnalysisLeftBehind(ctx)
	if err != nil {
		t.Fatalf("VaultAnalysisLeftBehind: %v", err)
	}
	if len(left) != 1 || left[0].AssetID != id {
		t.Fatalf("the sweep found %d assets, want the one that is leaking", len(left))
	}

	sealed := []SealedItem{{AssetID: id, SealedAnalysis: left[0].Analysis}}
	swept, err := s.CommitVaultAnalysis(ctx, sealed)
	if err != nil {
		t.Fatalf("CommitVaultAnalysis: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d assets, want 1", swept)
	}
	if got := analysisRows(t, s, id); got != (analysisCounts{}) {
		t.Errorf("the sweep left %+v behind", got)
	}
	if again, err := s.VaultAnalysisLeftBehind(ctx); err != nil || len(again) != 0 {
		t.Errorf("the sweep found %d assets on a second pass (err %v), want none", len(again), err)
	}
}

// A photograph that was restored between the sweep's read and its write keeps
// its words. The sweep deletes on the strength of a fact it read some
// milliseconds ago, and the fact it is deleting is the library's own metadata.
func TestTheSweepLeavesARestoredPhotographAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	describeForVault(t, s, id)
	hide(t, s, VaultHidden, Selection{IDs: []string{id}})
	unhide(t, s, []string{id})

	// Whatever the sweep read a moment ago, aimed at a row that is now back in
	// the library.
	swept, err := s.CommitVaultAnalysis(ctx, []SealedItem{{AssetID: id, SealedAnalysis: []byte("stale")}})
	if err != nil {
		t.Fatalf("CommitVaultAnalysis: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept %d restored photographs, want none", swept)
	}
	if got := analysisRows(t, s, id); got.captions != 1 {
		t.Errorf("the sweep took the words off a restored photograph: %+v", got)
	}
}

// describeForVault gives one asset the full set: a caption, a handful of tags,
// the text off it, and a vector.
func describeForVault(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()

	if err := s.PutDescription(ctx, id, CaptionModel,
		"a man asleep on a sofa in a bedroom",
		[]Tag{{Name: "sofa", Confidence: 0.9}, {Name: "bedroom", Confidence: 0.5}},
	); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
	if err := s.PutOCR(ctx, id, OCRModel, "ACCOUNT 1234 5678 BALANCE 4210.55"); err != nil {
		t.Fatalf("PutOCR: %v", err)
	}
	if err := s.PutEmbeddings(ctx, id, VisionModel, []Embedding{{Frame: 0, Vector: unit(7)}}); err != nil {
		t.Fatalf("PutEmbeddings: %v", err)
	}
}

// analysisCounts is what the four tables hold about one asset. The zero value
// is the whole assertion in two of the tests above.
type analysisCounts struct{ captions, ocr, tags, embeddings int }

func analysisRows(t *testing.T, s *Store, id string) analysisCounts {
	t.Helper()

	var c analysisCounts
	err := s.pool.QueryRow(context.Background(), `
		select (select count(*) from asset_descriptions where asset_id = $1::uuid),
		       (select count(*) from asset_ocr          where asset_id = $1::uuid),
		       (select count(*) from asset_tags         where asset_id = $1::uuid),
		       (select count(*) from asset_embeddings   where asset_id = $1::uuid)`, id).
		Scan(&c.captions, &c.ocr, &c.tags, &c.embeddings)
	if err != nil {
		t.Fatalf("count what the archive says about %s: %v", id, err)
	}
	return c
}

// searchText is the tsvector as text, or empty when the asset has no row.
func searchText(t *testing.T, s *Store, id string) string {
	t.Helper()

	var tsv *string
	err := s.pool.QueryRow(context.Background(),
		`select tsv::text from asset_search where asset_id = $1::uuid`, id).Scan(&tsv)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the search row of %s: %v", id, err)
	}
	return text(tsv)
}

func vectorLiteral(t *testing.T, s *Store, id string) string {
	t.Helper()

	var vec string
	if err := s.pool.QueryRow(context.Background(),
		`select embedding::text from asset_embeddings where asset_id = $1::uuid`, id).
		Scan(&vec); err != nil {
		t.Fatalf("read the vector of %s: %v", id, err)
	}
	return vec
}
