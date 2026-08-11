package db

import (
	"context"
	"fmt"
	"testing"
)

const sampleContentID = "064002F7-7F1B-41FA-A07C-DB8B0D52E7FF"

// importedPair is what a Google Takeout delivers: two files that declare
// nothing about each other, carrying the identifier Apple stamped into both at
// capture. No device, no local ids that mean anything, no LiveParentLocalID.
func importedPair(t *testing.T, i int) (still, video Asset) {
	t.Helper()

	still = sampleAsset()
	still.SHA256 = fmt.Sprintf("%064x", i*2+1)
	still.MD5 = fmt.Sprintf("%032x", i*2+1)
	still.DeviceID = "google-takeout"
	still.LocalID = fmt.Sprintf("Photos from 2025/IMG_%d.HEIC", 5874+i)
	still.ContentID = sampleContentID

	video = still
	video.SHA256 = fmt.Sprintf("%064x", i*2+2)
	video.MD5 = fmt.Sprintf("%032x", i*2+2)
	video.LocalID = fmt.Sprintf("Photos from 2025/IMG_%d.MP4", 5874+i)
	video.OriginalFilename = fmt.Sprintf("IMG_%d.MP4", 5874+i)
	video.Ext = ".mp4"
	video.ContentType = "video/mp4"
	video.MediaKind = MediaVideo
	return still, video
}

func TestImportedPairResolvesWhenTheStillArrivesFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := importedPair(t, 0)

	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}
	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record paired video: %v", err)
	}

	got, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("load paired video: %v", err)
	}
	if got.LiveParentAssetID == nil || *got.LiveParentAssetID != stillID {
		t.Fatalf("LiveParentAssetID = %v, want %s — the identifier did not pair them",
			got.LiveParentAssetID, stillID)
	}
	if !got.IsLivePair() {
		t.Error("IsLivePair = false on a video paired by content id")
	}
	if got.LiveState != DerivedPending {
		t.Errorf("LiveState = %q, want %q so the motion rendition gets built", got.LiveState, DerivedPending)
	}
}

// The export is imported stills-first, but nothing guarantees it: an album
// folder can hold a video whose still is in a folder read later, and a second
// zip can arrive months after the first.
func TestImportedPairResolvesWhenTheVideoArrivesFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := importedPair(t, 0)

	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record paired video: %v", err)
	}
	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}

	got, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("reload paired video: %v", err)
	}
	if got.LiveParentAssetID == nil || *got.LiveParentAssetID != stillID {
		t.Errorf("LiveParentAssetID = %v, want %s", got.LiveParentAssetID, stillID)
	}
	if _, err := s.LiveVideoFor(ctx, stillID); err != nil {
		t.Errorf("LiveVideoFor(still): %v", err)
	}
}

// The decision the whole design turns on. A third of the sample export is
// paired videos whose still is not in it, and hiding those would archive them
// into invisibility — the archive would hold bytes nothing could ever show.
func TestAPairedVideoWithNoStillStaysOnTheTimeline(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, video := importedPair(t, 0)

	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record orphan video: %v", err)
	}

	got, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("load orphan video: %v", err)
	}
	if got.ContentID != sampleContentID {
		t.Errorf("ContentID = %q, want it recorded even with no still to pair to", got.ContentID)
	}
	if got.IsLivePair() {
		t.Error("an orphan video counted as a Live Photo's half, so it would get no poster and no playback")
	}

	page, err := s.Timeline(ctx, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != videoID {
		t.Fatalf("got %d timeline items, want the orphan video visible as an ordinary one", len(page.Items))
	}
}

// And it disappears by itself the day its still is imported, rather than
// needing anyone to notice.
func TestAnOrphanVideoLeavesTheTimelineWhenItsStillArrives(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := importedPair(t, 0)

	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record orphan video: %v", err)
	}
	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}

	page, err := s.Timeline(ctx, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d timeline items, want only the still", len(page.Items))
	}
	if page.Items[0].ID != stillID {
		t.Errorf("timeline showed %s, want the still %s", page.Items[0].ID, stillID)
	}
	if page.Items[0].LiveState != DerivedPending {
		t.Errorf("LiveState = %q, want the still to report motion", page.Items[0].LiveState)
	}
	_ = videoID
}

// SetContentID is the worker's route in, and the one that matters: an import
// can declare nothing, so for most of an export this is the first moment
// anything knows the two files are related.
func TestSetContentIDPairsAndRequeuesAnAlreadyDerivedVideo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := importedPair(t, 0)

	// The video arrives with nothing declared, exactly as a file copied off a
	// disk would, and goes all the way through the pipeline as an ordinary one.
	video.ContentID = ""
	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record video: %v", err)
	}
	if err := s.ApplyMetadata(ctx, videoID, Metadata{}); err != nil {
		t.Fatalf("apply metadata: %v", err)
	}
	if _, _, err := s.SetContentID(ctx, videoID, sampleContentID); err != nil {
		t.Fatalf("set content id on the video: %v", err)
	}

	still.ContentID = ""
	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}

	// The still's own metadata job reads the identifier and finds the video.
	_, requeued, err := s.SetContentID(ctx, stillID, sampleContentID)
	if err != nil {
		t.Fatalf("set content id on the still: %v", err)
	}
	if len(requeued) != 1 || requeued[0] != videoID {
		t.Fatalf("requeued = %v, want [%s]: the video was already derived as an ordinary video "+
			"and has no motion rendition", requeued, videoID)
	}

	got, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("reload video: %v", err)
	}
	if got.LiveParentAssetID == nil || *got.LiveParentAssetID != stillID {
		t.Errorf("LiveParentAssetID = %v, want %s", got.LiveParentAssetID, stillID)
	}
	if got.LiveState != DerivedPending {
		t.Errorf("LiveState = %q, want %q", got.LiveState, DerivedPending)
	}
}

// A video whose metadata job has not run yet needs no requeueing: the job it
// already has will see the pairing when it gets there.
func TestSetContentIDDoesNotRequeueAVideoStillWaitingOnItsFirstJob(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := importedPair(t, 0)

	if _, _, err := s.RecordAsset(ctx, video); err != nil {
		t.Fatalf("record video: %v", err)
	}
	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}
	_, requeued, err := s.SetContentID(ctx, stillID, sampleContentID)
	if err != nil {
		t.Fatalf("SetContentID: %v", err)
	}
	if len(requeued) != 0 {
		t.Errorf("requeued = %v, want none: that video has never been derived", requeued)
	}
}

// The file is the authority. A client that declares an identifier the bytes do
// not carry costs a pairing until the worker reads it, and nothing after — the
// asset it wrongly hid comes back.
func TestSetContentIDUndoesAPairingTheFileDoesNotSupport(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := importedPair(t, 0)

	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}
	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record video: %v", err)
	}
	if _, err := s.LiveVideoFor(ctx, stillID); err != nil {
		t.Fatalf("the two should be paired to begin with: %v", err)
	}

	if _, _, err := s.SetContentID(ctx, videoID, ""); err != nil {
		t.Fatalf("SetContentID: %v", err)
	}
	got, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("reload video: %v", err)
	}
	if got.LiveParentAssetID != nil {
		t.Error("the pairing survived an identifier the file turned out not to carry")
	}

	page, err := s.Timeline(ctx, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("got %d timeline items, want the wrongly hidden video back", len(page.Items))
	}
}

// A phone's declaration is evidence in its own right and outlives a content id
// that turns out to be wrong or absent.
func TestSetContentIDKeepsAPairingThePhoneDeclared(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := livePair(t, 0)

	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}
	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record video: %v", err)
	}

	if _, _, err := s.SetContentID(ctx, videoID, ""); err != nil {
		t.Fatalf("SetContentID: %v", err)
	}
	got, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("reload video: %v", err)
	}
	if got.LiveParentAssetID == nil || *got.LiveParentAssetID != stillID {
		t.Error("a pairing the phone declared was dropped because the file carries no identifier")
	}
}

// The sample export holds a HEIC and a JPEG re-export of one capture, both
// stamped with the same UUID. Refusing to pair on the ambiguity would lose the
// motion on a photo that plainly has some, so one is chosen — and the choice
// has to be stable, or a reindex would move it.
func TestAnAmbiguousIdentifierPairsToTheFirstStillArchived(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := importedPair(t, 0)

	firstID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record the HEIC: %v", err)
	}

	reexport := still
	reexport.SHA256 = fmt.Sprintf("%064x", 99)
	reexport.MD5 = fmt.Sprintf("%032x", 99)
	reexport.LocalID = "Photos from 2025/BC0963DF.jpg"
	reexport.OriginalFilename = "BC0963DF.jpg"
	reexport.Ext = ".jpg"
	reexport.ContentType = "image/jpeg"
	if _, _, err := s.RecordAsset(ctx, reexport); err != nil {
		t.Fatalf("record the JPEG re-export: %v", err)
	}

	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record video: %v", err)
	}
	got, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("load video: %v", err)
	}
	if got.LiveParentAssetID == nil {
		t.Fatal("an ambiguous identifier paired to nothing at all")
	}
	if *got.LiveParentAssetID != firstID {
		t.Errorf("paired to %s, want the first still archived, %s", *got.LiveParentAssetID, firstID)
	}
}

// Two stills sharing an identifier is a duplicate; two videos is not a reason
// to pair one to the other.
func TestAnIdentifierNeverPairsTwoVideos(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, video := importedPair(t, 0)

	firstID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record first video: %v", err)
	}
	second := video
	second.SHA256 = fmt.Sprintf("%064x", 98)
	second.MD5 = fmt.Sprintf("%032x", 98)
	second.LocalID = "Photos from 2025/IMG_9999.MP4"
	secondID, _, err := s.RecordAsset(ctx, second)
	if err != nil {
		t.Fatalf("record second video: %v", err)
	}

	for _, id := range []string{firstID, secondID} {
		got, err := s.Asset(ctx, id)
		if err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		if got.LiveParentAssetID != nil {
			t.Errorf("video %s was paired to another video", id)
		}
	}
}

// Garbage in the tag must not pair anything. Two files whose maker note holds
// the same non-UUID string would otherwise hide one behind the other.
func TestAMalformedIdentifierIsNotStored(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	a.ContentID = "not-a-uuid"
	id, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if got.ContentID != "" {
		t.Errorf("ContentID = %q, want it discarded", got.ContentID)
	}
}

// The identifier is written in one spelling wherever it entered from, so the
// two halves of a pair can meet regardless of which route each took.
func TestIdentifierSpellingIsNormalizedOnTheWayIn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := importedPair(t, 0)

	still.ContentID = "{" + lower(sampleContentID) + "}"
	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}
	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record video: %v", err)
	}

	got, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("load video: %v", err)
	}
	if got.LiveParentAssetID == nil || *got.LiveParentAssetID != stillID {
		t.Error("two spellings of one identifier failed to meet")
	}
}

func lower(s string) string {
	out := []rune(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		}
	}
	return string(out)
}
