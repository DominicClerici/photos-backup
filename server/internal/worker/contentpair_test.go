package worker

import (
	"context"
	"os"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// The two fixtures carry the same Apple content identifier and say nothing else
// about each other: no phone declared them a pair, and their filenames do not
// match. This is what an import delivers, and the identifier is the whole of
// the evidence.
const (
	stillFixture = "iphone-portrait.heic"
	videoFixture = "live-clip.mov"
)

// The end-to-end claim. Two files arrive with nothing declared, the metadata
// job reads the identifier off each, and what comes out is a Live Photo.
func TestTheMetadataJobPairsAnImportByContentIdentifier(t *testing.T) {
	h := newHarness(t)
	still := h.ingest(t, stillFixture, db.MediaImage)
	video := h.ingest(t, videoFixture, db.MediaVideo)

	if still.IsLivePair() || video.IsLivePair() {
		t.Fatal("something paired them before a single byte was read")
	}

	h.claimAndRun(t, jobs.KindMetadata) // the still
	h.claimAndRun(t, jobs.KindMetadata) // the video

	paired := h.reload(t, video.ID)
	if paired.ContentID == "" {
		t.Fatal("no content identifier was read off the video")
	}
	if paired.LiveParentAssetID == nil || *paired.LiveParentAssetID != still.ID {
		t.Fatalf("LiveParentAssetID = %v, want the still %s", paired.LiveParentAssetID, still.ID)
	}
	if paired.LiveState != db.DerivedReady {
		t.Errorf("LiveState = %q, want %q", paired.LiveState, db.DerivedReady)
	}

	// The motion rendition, which is what makes the still come to life. An
	// ordinary video would have got a poster frame and a queued transcode.
	for _, size := range derivstore.ThumbSizes {
		if _, err := h.Derivatives.Open(paired.SHA256, derivstore.LiveSuffix(size)); err != nil {
			t.Errorf("no motion rendition at %d: %v", size, err)
		}
	}
	if _, err := h.Derivatives.Open(paired.SHA256, derivstore.ThumbSuffix(derivstore.ThumbSizes[0])); err == nil {
		t.Error("a poster frame was built for a Live Photo's video; its tile belongs to the still")
	}
	if paired.PlaybackState != db.PlaybackNone {
		t.Errorf("PlaybackState = %q, want %q — nothing opens a player on three seconds of a Live Photo",
			paired.PlaybackState, db.PlaybackNone)
	}
}

// The order is not something an import controls: an album folder can hold a
// video whose still is read later, and a Takeout arrives as a stack of zips. A
// video that was already through the pipeline as an ordinary one has to be put
// back through it once its still turns up, because it has a poster and no
// motion.
func TestAVideoIsRederivedWhenItsStillArrivesLater(t *testing.T) {
	h := newHarness(t)
	video := h.ingest(t, videoFixture, db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	asOrdinary := h.reload(t, video.ID)
	if asOrdinary.IsLivePair() {
		t.Fatal("a video paired itself with a still that is not in the archive")
	}
	if asOrdinary.PlaybackState != db.DerivedPending {
		t.Fatalf("PlaybackState = %q, want a transcode queued for what is still an ordinary video",
			asOrdinary.PlaybackState)
	}
	if _, err := h.Derivatives.Open(asOrdinary.SHA256, derivstore.ThumbSuffix(derivstore.ThumbSizes[0])); err != nil {
		t.Fatalf("an ordinary video got no poster frame: %v", err)
	}

	// The still arrives, and its own metadata job is what notices.
	still := h.ingest(t, stillFixture, db.MediaImage)
	h.claimAndRun(t, jobs.KindMetadata)

	if state := h.jobState(t, video.ID, jobs.KindMetadata); state != string(jobs.StatePending) {
		t.Fatalf("the video's metadata job is %q, want it requeued as %q", state, jobs.StatePending)
	}
	h.claimAndRun(t, jobs.KindMetadata)

	paired := h.reload(t, video.ID)
	if paired.LiveParentAssetID == nil || *paired.LiveParentAssetID != still.ID {
		t.Fatalf("LiveParentAssetID = %v, want the still %s", paired.LiveParentAssetID, still.ID)
	}
	if paired.LiveState != db.DerivedReady {
		t.Errorf("LiveState = %q, want %q", paired.LiveState, db.DerivedReady)
	}
	for _, size := range derivstore.ThumbSizes {
		if _, err := h.Derivatives.Open(paired.SHA256, derivstore.LiveSuffix(size)); err != nil {
			t.Errorf("no motion rendition at %d after re-derivation: %v", size, err)
		}
	}
}

// The transcode queued while it looked like an ordinary video must not run once
// it is a Live Photo's motion: the rendition would have no reader.
func TestTheQueuedTranscodeSettlesOnceTheVideoBecomesAPair(t *testing.T) {
	h := newHarness(t)
	video := h.ingest(t, videoFixture, db.MediaVideo)
	h.claimAndRun(t, jobs.KindMetadata)

	h.ingest(t, stillFixture, db.MediaImage)
	h.claimAndRun(t, jobs.KindMetadata)

	h.claimAndRun(t, jobs.KindPlayback)
	paired := h.reload(t, video.ID)
	if paired.PlaybackState != db.PlaybackNone {
		t.Errorf("PlaybackState = %q, want %q", paired.PlaybackState, db.PlaybackNone)
	}
	if _, err := h.Derivatives.Open(paired.SHA256, derivstore.Playback); !os.IsNotExist(err) {
		t.Error("a playback rendition was built for a Live Photo's motion")
	}
}

// A file with no identifier stays exactly what it was. Most of an archive is
// this case, and the pairing must not reach into it.
func TestAVideoWithNoIdentifierIsNeverPaired(t *testing.T) {
	h := newHarness(t)
	h.ingest(t, stillFixture, db.MediaImage)
	plain := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, plain.ID)
	if got.ContentID != "" {
		t.Errorf("ContentID = %q on a file that carries none", got.ContentID)
	}
	if got.IsLivePair() {
		t.Error("an ordinary video was paired to a Live Photo's still")
	}
	if got.PlaybackState != db.DerivedPending {
		t.Errorf("PlaybackState = %q, want a transcode queued", got.PlaybackState)
	}

	page, err := h.store.Timeline(context.Background(), nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("got %d timeline items, want both the photo and the ordinary video", len(page.Items))
	}
}
