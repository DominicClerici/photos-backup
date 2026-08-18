package worker

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// ingestLivePair archives a still and the paired video that declares it, the
// way an upload from the phone would.
func (h *harness) ingestLivePair(t *testing.T) (still, paired db.Asset) {
	t.Helper()
	still = h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "clip.mov"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sum := md5.Sum(body)
	res, err := h.Blobs.Put(bytes.NewReader(body), ".mov", blobstore.Expected{
		MD5:  hex.EncodeToString(sum[:]),
		Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}

	id, _, err := h.store.RecordAsset(context.Background(), db.Asset{
		SHA256:            res.SHA256,
		MD5:               res.MD5,
		ByteSize:          res.Size,
		OriginalFilename:  "IMG_8071.MOV",
		Ext:               ".mov",
		ContentType:       "video/quicktime",
		MediaKind:         db.MediaVideo,
		DeviceID:          "test-device",
		LocalID:           still.LocalID + "#live",
		LiveParentLocalID: still.LocalID,
	})
	if err != nil {
		t.Fatalf("record paired video: %v", err)
	}
	paired, err = h.store.Asset(context.Background(), id)
	if err != nil {
		t.Fatalf("load paired video: %v", err)
	}
	return still, paired
}

// A Live Photo's video takes a different route through the metadata job than an
// ordinary one: it builds the motion thumbnail, skips the poster nothing would
// draw, and queues no transcode.
func TestMetadataJobBuildsTheMotionThumbnailForALivePair(t *testing.T) {
	h := newHarness(t)
	_, paired := h.ingestLivePair(t)

	h.claimAndRun(t, jobs.KindMetadata) // the still
	h.claimAndRun(t, jobs.KindMetadata) // the paired video

	got := h.reload(t, paired.ID)
	if got.LiveState != db.DerivedReady {
		t.Fatalf("LiveState = %q, want ready", got.LiveState)
	}
	for _, size := range derivstore.ThumbSizes {
		if !h.Derivatives.Exists(paired.SHA256, derivstore.LiveSuffix(size)) {
			t.Errorf("no %dpx motion thumbnail was written", size)
		}
	}
	if h.Derivatives.Exists(paired.SHA256, derivstore.Thumb) {
		t.Error("a poster thumbnail was written for a paired video; nothing would ever draw it")
	}
	if got.DurationSeconds == nil || *got.DurationSeconds <= 0 {
		t.Error("the paired video's duration was not probed")
	}
}

// The stored H.264 rendition is what a Live Photo deliberately does not get.
// Roughly a third of an iPhone library is one, and three seconds each through
// the transcode queue would swamp the videos that actually need it.
func TestLivePairQueuesNoTranscode(t *testing.T) {
	h := newHarness(t)
	_, paired := h.ingestLivePair(t)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, paired.ID)
	if got.PlaybackState != db.PlaybackNone {
		t.Errorf("PlaybackState = %q, want none", got.PlaybackState)
	}
	if _, err := h.Queue.Claim(context.Background(), []jobs.Kind{jobs.KindPlayback}, "t"); err == nil {
		t.Error("a Live Photo's paired video queued a transcode")
	}
	if h.Derivatives.Exists(paired.SHA256, derivstore.Playback) {
		t.Error("a playback rendition was stored for a Live Photo's paired video")
	}
}

// The motion thumbnail has to be the same square as the still thumbnail beside
// it. It replaces that image in the same cell, and any other shape makes the
// picture visibly jump the moment a pointer lands on it.
func TestMotionThumbnailIsTheSameSquareAsTheStillThumbnail(t *testing.T) {
	h := newHarness(t)
	_, paired := h.ingestLivePair(t)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMetadata)

	for _, size := range derivstore.ThumbSizes {
		info, err := h.Video.Probe(context.Background(), h.Derivatives.Path(paired.SHA256, derivstore.LiveSuffix(size)))
		if err != nil {
			t.Fatalf("probe the %dpx motion thumbnail: %v", size, err)
		}
		if w, hgt := info.DisplaySize(); w != size || hgt != size {
			t.Errorf("motion thumbnail is %dx%d, want %[3]dx%[3]d", w, hgt, size)
		}
	}
}

// A container ffprobe can describe and ffmpeg cannot decode costs the animation
// and nothing else. The original is archived and verified either way, and the
// still it is paired with still draws.
func TestALivePairThatWillNotDecodeKeepsItsStill(t *testing.T) {
	h := newHarness(t)
	still := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "undecodable.mov"))
	if err != nil {
		t.Skipf("no undecodable fixture: %v", err)
	}
	sum := md5.Sum(body)
	res, err := h.Blobs.Put(bytes.NewReader(body), ".mov", blobstore.Expected{
		MD5:  hex.EncodeToString(sum[:]),
		Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}
	id, _, err := h.store.RecordAsset(context.Background(), db.Asset{
		SHA256: res.SHA256, MD5: res.MD5, ByteSize: res.Size,
		OriginalFilename: "broken.MOV", Ext: ".mov", ContentType: "video/quicktime",
		MediaKind: db.MediaVideo, DeviceID: "test-device",
		LocalID:           still.LocalID + "#live",
		LiveParentLocalID: still.LocalID,
	})
	if err != nil {
		t.Fatalf("record paired video: %v", err)
	}

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, id)
	if got.DerivedState != db.DerivedReady {
		t.Errorf("DerivedState = %q, want ready — what was readable is still recorded", got.DerivedState)
	}
	if got.LiveState != db.DerivedFailed {
		t.Errorf("LiveState = %q, want failed so the grid stops waiting on it", got.LiveState)
	}
	if h.reload(t, still.ID).DerivedState != db.DerivedReady {
		t.Error("the still lost its own thumbnail over its paired video")
	}
}
