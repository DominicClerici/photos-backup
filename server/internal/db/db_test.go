package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func sampleAsset() Asset {
	captured := time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC)
	return Asset{
		SHA256:           "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		MD5:              "d41d8cd98f00b204e9800998ecf8427e",
		ByteSize:         2_411_255,
		OriginalFilename: "IMG_8071.HEIC",
		Ext:              ".heic",
		ContentType:      "image/heic",
		CapturedAt:       &captured,
		DeviceID:         "iphone-14-pro",
		LocalID:          "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001",
	}
}

func TestRecordAssetReturnsNewID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, inserted, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	if !inserted {
		t.Errorf("inserted = false, want true for a new sha256")
	}
	if id == "" {
		t.Error("RecordAsset returned an empty id")
	}
}

func TestRecordAssetIsIdempotentOnSHA256(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	firstID, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}

	// Same bytes arriving again from a different local asset must not create a
	// second row; the archive is addressed by content, not by phone identity.
	dup := sampleAsset()
	dup.LocalID = "a-different-local-id"
	secondID, inserted, err := s.RecordAsset(ctx, dup)
	if err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}
	if inserted {
		t.Errorf("inserted = true on duplicate sha256, want false")
	}
	if secondID != firstID {
		t.Errorf("duplicate returned id %q, want the original %q", secondID, firstID)
	}

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 {
		t.Errorf("table holds %d rows after duplicate upload, want 1", len(page.Items))
	}
}

// The mapping has to survive content dedup. If it does not, sync/check asks for
// the second local id on every run and the phone re-uploads it forever.
func TestRecordAssetMapsBothLocalIDsOfDuplicateContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, first); err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}
	second := sampleAsset()
	second.LocalID = "a-different-local-id"
	assetID, _, err := s.RecordAsset(ctx, second)
	if err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}

	known, err := s.KnownMappings(ctx, first.DeviceID, []LocalRef{
		{LocalID: first.LocalID},
		{LocalID: second.LocalID},
	})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if len(known) != 2 {
		t.Fatalf("KnownMappings returned %d mappings, want 2: %v", len(known), known)
	}
	if known[first.LocalID] != assetID || known[second.LocalID] != assetID {
		t.Errorf("both local ids should resolve to %q, got %v", assetID, known)
	}
}

func TestKnownMappingsSkipsOtherDevices(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	known, err := s.KnownMappings(ctx, "some-other-phone", []LocalRef{{LocalID: a.LocalID}})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if len(known) != 0 {
		t.Errorf("another device's local id resolved: %v", known)
	}
}

// A photo edited in Photos keeps its local identifier but changes its bytes, so
// a stale modification time must not be reported as already archived.
func TestKnownMappingsRejectsChangedModificationTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	modified := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	a := sampleAsset()
	a.ModifiedAt = &modified
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	same, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{{LocalID: a.LocalID, ModifiedAt: &modified}})
	if err != nil {
		t.Fatalf("KnownMappings unchanged: %v", err)
	}
	if len(same) != 1 {
		t.Errorf("unchanged asset did not resolve: %v", same)
	}

	edited := modified.Add(time.Hour)
	after, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{{LocalID: a.LocalID, ModifiedAt: &edited}})
	if err != nil {
		t.Fatalf("KnownMappings edited: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("edited asset resolved as already archived: %v", after)
	}
}

// Rows written before modification times were tracked hold null, and a phone
// that reports no modification time must still match them.
func TestKnownMappingsMatchesTwoNullModificationTimes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	known, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{{LocalID: a.LocalID}})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if len(known) != 1 {
		t.Errorf("null modification times did not match: %v", known)
	}
}

// One null and one value is genuinely unknown, so it must fall through to a
// content check rather than be assumed equal.
func TestKnownMappingsRejectsOneSidedNullModificationTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	reported := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	known, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{{LocalID: a.LocalID, ModifiedAt: &reported}})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if len(known) != 0 {
		t.Errorf("stored null matched a reported time: %v", known)
	}
}

func TestAssetsByContentResolvesSingleMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	assetID, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	key := ContentKey{MD5: a.MD5, ByteSize: a.ByteSize}
	found, err := s.AssetsByContent(ctx, []ContentKey{key})
	if err != nil {
		t.Fatalf("AssetsByContent: %v", err)
	}
	match, ok := found[key]
	if !ok {
		t.Fatalf("content key did not resolve: %v", found)
	}
	if match.Matches != 1 {
		t.Errorf("Matches = %d, want 1", match.Matches)
	}
	if match.AssetID != assetID {
		t.Errorf("AssetID = %q, want %q", match.AssetID, assetID)
	}
}

func TestAssetsByContentOmitsUnknownContent(t *testing.T) {
	s := testStore(t)

	found, err := s.AssetsByContent(context.Background(), []ContentKey{
		{MD5: "ffffffffffffffffffffffffffffffff", ByteSize: 99},
	})
	if err != nil {
		t.Fatalf("AssetsByContent: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("unarchived content resolved: %v", found)
	}
}

// Two assets sharing an md5 and a size means md5 is not identifying anything
// here. The caller must be able to see that and re-upload instead of guessing.
func TestAssetsByContentReportsAmbiguousMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}
	collision := sampleAsset()
	collision.SHA256 = "aaaa" + a.SHA256[4:]
	collision.LocalID = "another-local-id"
	if _, _, err := s.RecordAsset(ctx, collision); err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}

	key := ContentKey{MD5: a.MD5, ByteSize: a.ByteSize}
	found, err := s.AssetsByContent(ctx, []ContentKey{key})
	if err != nil {
		t.Fatalf("AssetsByContent: %v", err)
	}
	if found[key].Matches != 2 {
		t.Errorf("Matches = %d, want 2 so the caller refuses to pick one", found[key].Matches)
	}
}

func TestRecordMappingsMakesContentReachableByLocalID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	assetID, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	modified := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	err = s.RecordMappings(ctx, a.DeviceID, []Mapping{
		{LocalID: "newly-seen-local-id", AssetID: assetID, ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("RecordMappings: %v", err)
	}

	known, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{
		{LocalID: "newly-seen-local-id", ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if known["newly-seen-local-id"] != assetID {
		t.Errorf("recorded mapping did not resolve: %v", known)
	}
}

func TestRecordMappingsOverwritesAStaleMapping(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first := sampleAsset()
	firstID, _, err := s.RecordAsset(ctx, first)
	if err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}
	edited := sampleAsset()
	edited.SHA256 = "bbbb" + first.SHA256[4:]
	edited.MD5 = "0000d8cd98f00b204e9800998ecf8427"
	edited.LocalID = "throwaway-local-id"
	editedID, _, err := s.RecordAsset(ctx, edited)
	if err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}

	modified := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	err = s.RecordMappings(ctx, first.DeviceID, []Mapping{
		{LocalID: first.LocalID, AssetID: editedID, ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("RecordMappings: %v", err)
	}

	known, err := s.KnownMappings(ctx, first.DeviceID, []LocalRef{
		{LocalID: first.LocalID, ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if known[first.LocalID] == firstID {
		t.Error("mapping still points at the superseded asset")
	}
	if known[first.LocalID] != editedID {
		t.Errorf("mapping = %q, want the newer asset %q", known[first.LocalID], editedID)
	}
}

func TestAssetRoundTripsFields(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	want := sampleAsset()

	id, _, err := s.RecordAsset(ctx, want)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("Asset: %v", err)
	}
	if got.SHA256 != want.SHA256 || got.MD5 != want.MD5 || got.ByteSize != want.ByteSize {
		t.Errorf("digest/size lost in round trip: %+v", got)
	}
	if got.OriginalFilename != want.OriginalFilename || got.Ext != want.Ext || got.ContentType != want.ContentType {
		t.Errorf("file metadata lost in round trip: %+v", got)
	}
	if got.DeviceID != want.DeviceID || got.LocalID != want.LocalID {
		t.Errorf("device metadata lost in round trip: %+v", got)
	}
	if got.CapturedAt == nil || !got.CapturedAt.Equal(*want.CapturedAt) {
		t.Errorf("CapturedAt = %v, want %v", got.CapturedAt, want.CapturedAt)
	}
	if got.UploadedAt.IsZero() {
		t.Error("UploadedAt was not populated by the database")
	}
}

func TestAssetReportsNotFoundForUnknownID(t *testing.T) {
	s := testStore(t)

	_, err := s.Asset(context.Background(), "6b3e2c1a-0000-4000-8000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Asset error = %v, want ErrNotFound", err)
	}
}

func TestRecordAssetQueuesItsDerivativeWork(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	var queued int
	if err := s.pool.QueryRow(ctx,
		`select count(*) from jobs where asset_id = $1::uuid and kind = 'metadata'`, id).Scan(&queued); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued %d metadata jobs, want 1 committed with the asset row", queued)
	}
}

// Duplicate content already has derivatives, or work queued to build them.
// Enqueuing again would mean every re-upload of the same photo re-ran ffmpeg.
func TestRecordAssetDoesNotQueueWorkForDuplicateContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}
	dup := sampleAsset()
	dup.LocalID = "a-different-local-id"
	if _, _, err := s.RecordAsset(ctx, dup); err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}

	var queued int
	if err := s.pool.QueryRow(ctx,
		`select count(*) from jobs where asset_id = $1::uuid`, id).Scan(&queued); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if queued != 1 {
		t.Errorf("queued %d jobs, want 1", queued)
	}
}

func TestTimelineOrdersNewestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	times := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	for i, ts := range times {
		seedAsset(t, s, i, ts)
	}

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(page.Items))
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty on the last page", page.NextCursor)
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1].TakenAt.Before(page.Items[i].TakenAt) {
			t.Errorf("items out of order at %d: %v before %v",
				i, page.Items[i-1].TakenAt, page.Items[i].TakenAt)
		}
	}
}

// The file's capture time is what the timeline sorts on. A photo taken in 2019
// but imported into Photos today must land in 2019, not at the top.
func TestTimelinePrefersTheCaptureTimeReadFromTheFile(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	imported := seedAsset(t, s, 0, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	recent := seedAsset(t, s, 1, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	shotIn2019 := time.Date(2019, 4, 2, 9, 30, 0, 0, time.UTC)
	if err := s.ApplyMetadata(ctx, imported, Metadata{ExifCapturedAt: &shotIn2019}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(page.Items))
	}
	if page.Items[0].ID != recent {
		t.Errorf("first item = %s, want the 2026 asset %s", page.Items[0].ID, recent)
	}
	if !page.Items[1].TakenAt.Equal(shotIn2019) {
		t.Errorf("second item taken at %v, want the EXIF time %v", page.Items[1].TakenAt, shotIn2019)
	}
}

// The grid groups photos under a day heading, so it needs the file's own UTC
// offset. Without it a photo taken at 23:50 local would be filed under the next
// day by any browser east of the camera.
func TestTimelineCarriesTheFilesUTCOffset(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	zoned := seedAsset(t, s, 0, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	unzoned := seedAsset(t, s, 1, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	shot := time.Date(2026, 8, 4, 23, 50, 36, 0, time.UTC)
	offset := -240
	if err := s.ApplyMetadata(ctx, zoned, Metadata{ExifCapturedAt: &shot, ExifOffsetMinutes: &offset}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}
	if err := s.ApplyMetadata(ctx, unzoned, Metadata{}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	byID := make(map[string]TimelineItem, len(page.Items))
	for _, it := range page.Items {
		byID[it.ID] = it
	}

	got := byID[zoned].OffsetMinutes
	if got == nil || *got != offset {
		t.Errorf("zoned asset offset = %v, want %d", got, offset)
	}
	// Null rather than zero: "no zone recorded" and "UTC" are different claims,
	// and the viewer must not pass the first off as the second.
	if byID[unzoned].OffsetMinutes != nil {
		t.Errorf("asset with no recorded zone reported offset %v, want null", *byID[unzoned].OffsetMinutes)
	}

	states, err := s.TimelineStates(ctx, []string{zoned})
	if err != nil {
		t.Fatalf("TimelineStates: %v", err)
	}
	if got := states[zoned].OffsetMinutes; got == nil || *got != offset {
		t.Errorf("TimelineStates offset = %v, want %d", got, offset)
	}
}

func TestTimelinePagesWithoutSkippingOrRepeating(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const total = 7
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := range total {
		seedAsset(t, s, i, base.Add(time.Duration(i)*time.Hour))
	}

	var seen []string
	var cursor *Cursor
	for range total { // bounded so a broken cursor cannot spin forever
		page, err := s.Timeline(ctx, TimelineFilter{}, cursor, 3)
		if err != nil {
			t.Fatalf("Timeline: %v", err)
		}
		for _, it := range page.Items {
			seen = append(seen, it.ID)
		}
		if page.NextCursor == "" {
			break
		}
		c, err := DecodeCursor(page.NextCursor)
		if err != nil {
			t.Fatalf("DecodeCursor: %v", err)
		}
		cursor = &c
	}

	if len(seen) != total {
		t.Fatalf("paged through %d items, want %d", len(seen), total)
	}
	unique := make(map[string]bool, len(seen))
	for _, id := range seen {
		if unique[id] {
			t.Errorf("asset %s was returned on two pages", id)
		}
		unique[id] = true
	}
}

// Assets sharing a timestamp are the case a naive timestamp-only cursor gets
// wrong: it either loses them or serves them twice.
func TestTimelinePagesThroughAssetsSharingATimestamp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	same := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		seedAsset(t, s, i, same)
	}

	unique := make(map[string]bool)
	var cursor *Cursor
	for range 5 {
		page, err := s.Timeline(ctx, TimelineFilter{}, cursor, 2)
		if err != nil {
			t.Fatalf("Timeline: %v", err)
		}
		for _, it := range page.Items {
			if unique[it.ID] {
				t.Errorf("asset %s was returned twice", it.ID)
			}
			unique[it.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		c, err := DecodeCursor(page.NextCursor)
		if err != nil {
			t.Fatalf("DecodeCursor: %v", err)
		}
		cursor = &c
	}

	if len(unique) != 5 {
		t.Errorf("saw %d of 5 assets sharing a timestamp", len(unique))
	}
}

// A pending asset that the timeline hid would be invisible for the whole
// backfill, and a permanently failed one would be invisible forever.
func TestTimelineIncludesAssetsWhoseDerivativesAreNotReady(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	pending := seedAsset(t, s, 0, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	failed := seedAsset(t, s, 1, time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC))
	if err := s.SetDerivedState(ctx, failed, DerivedFailed); err != nil {
		t.Fatalf("SetDerivedState: %v", err)
	}

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}

	states := make(map[string]string, len(page.Items))
	for _, it := range page.Items {
		states[it.ID] = it.State
	}
	if states[pending] != DerivedPending {
		t.Errorf("pending asset state = %q, want %q", states[pending], DerivedPending)
	}
	if states[failed] != DerivedFailed {
		t.Errorf("failed asset state = %q, want %q", states[failed], DerivedFailed)
	}
}

func TestTimelineStatesReportsOnlyTheAssetsAskedFor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	watched := seedAsset(t, s, 0, base)
	other := seedAsset(t, s, 1, base.Add(-time.Hour))

	if err := s.ApplyMetadata(ctx, watched, Metadata{}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}

	states, err := s.TimelineStates(ctx, []string{watched})
	if err != nil {
		t.Fatalf("TimelineStates: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d states, want 1", len(states))
	}
	if states[watched].State != DerivedReady {
		t.Errorf("state = %q, want %q", states[watched].State, DerivedReady)
	}
	if _, ok := states[other]; ok {
		t.Error("TimelineStates returned an asset that was not asked about")
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, token := range []string{"not-base64!!", "", "Zm9v"} {
		if _, err := DecodeCursor(token); !errors.Is(err, ErrBadCursor) {
			t.Errorf("DecodeCursor(%q) error = %v, want ErrBadCursor", token, err)
		}
	}
}

func TestApplyMetadataMarksTheAssetReady(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 0, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	width, height, offset := 4032, 3024, -240
	lat, lon := 44.4759, -73.2121
	shot := time.Date(2026, 4, 30, 18, 12, 0, 0, time.UTC)

	err := s.ApplyMetadata(ctx, id, Metadata{
		Width: &width, Height: &height,
		CameraMake: "Apple", CameraModel: "iPhone 14 Pro",
		Lens:   "iPhone 14 Pro back triple camera 6.86mm f/1.78",
		GPSLat: &lat, GPSLon: &lon,
		ExifCapturedAt: &shot, ExifOffsetMinutes: &offset,
	})
	if err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("Asset: %v", err)
	}
	if got.DerivedState != DerivedReady {
		t.Errorf("DerivedState = %q, want %q", got.DerivedState, DerivedReady)
	}
	if got.Width == nil || *got.Width != width {
		t.Errorf("Width = %v, want %d", got.Width, width)
	}
	if got.CameraModel != "iPhone 14 Pro" {
		t.Errorf("CameraModel = %q", got.CameraModel)
	}
	if got.ExifOffsetMinutes == nil || *got.ExifOffsetMinutes != offset {
		t.Errorf("ExifOffsetMinutes = %v, want %d", got.ExifOffsetMinutes, offset)
	}
	// The phone's value survives the worker writing the file's value.
	if got.CapturedAt == nil {
		t.Error("ApplyMetadata cleared captured_at; the phone's value must be kept")
	}
	if !got.SortTime.Equal(shot) {
		t.Errorf("SortTime = %v, want the EXIF capture time %v", got.SortTime, shot)
	}
}

func TestApplyMetadataReportsAnUnknownAsset(t *testing.T) {
	s := testStore(t)

	err := s.ApplyMetadata(context.Background(), "6b3e2c1a-0000-4000-8000-000000000000", Metadata{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ApplyMetadata error = %v, want ErrNotFound", err)
	}
}

// seedAsset records a distinct asset captured at the given time, returning its
// id. The index only has to make sha256 and local id unique.
func seedAsset(t *testing.T, s *Store, i int, captured time.Time) string {
	t.Helper()
	a := sampleAsset()
	a.SHA256 = fmt.Sprintf("%064x", i+1)
	a.MD5 = fmt.Sprintf("%032x", i+1)
	a.LocalID = fmt.Sprintf("%s-%d", a.LocalID, i)
	a.CapturedAt = &captured

	id, _, err := s.RecordAsset(context.Background(), a)
	if err != nil {
		t.Fatalf("RecordAsset %d: %v", i, err)
	}
	return id
}
