package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// livePair returns a still and the paired video that declares it, both from one
// device and sharing a capture time — which is what the phone actually delivers.
func livePair(t *testing.T, i int) (still, video Asset) {
	t.Helper()
	captured := time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC)

	still = sampleAsset()
	still.SHA256 = fmt.Sprintf("%064x", i*2+1)
	still.MD5 = fmt.Sprintf("%032x", i*2+1)
	still.LocalID = fmt.Sprintf("LIVE-%d/L0/001", i)
	still.CapturedAt = &captured

	video = sampleAsset()
	video.SHA256 = fmt.Sprintf("%064x", i*2+2)
	video.MD5 = fmt.Sprintf("%032x", i*2+2)
	video.LocalID = still.LocalID + "#live"
	video.OriginalFilename = "IMG_8071.MOV"
	video.Ext = ".mov"
	video.ContentType = "video/quicktime"
	video.MediaKind = MediaVideo
	video.CapturedAt = &captured
	video.LiveParentLocalID = still.LocalID
	return still, video
}

func TestLivePairResolvesWhenTheStillArrivesFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := livePair(t, 0)

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
		t.Errorf("LiveParentAssetID = %v, want %s", got.LiveParentAssetID, stillID)
	}
	if got.LiveState != DerivedPending {
		t.Errorf("LiveState = %q, want %q", got.LiveState, DerivedPending)
	}
}

// The phone queues both halves under the same capture time and the queue orders
// by capture time, so nothing decides which of the two goes up first. The
// pairing has to resolve from the still's side as readily as from the video's.
func TestLivePairResolvesWhenTheVideoArrivesFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := livePair(t, 0)

	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record paired video: %v", err)
	}

	unresolved, err := s.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("load paired video: %v", err)
	}
	if unresolved.LiveParentAssetID != nil {
		t.Errorf("LiveParentAssetID = %v before the still arrived, want nil", *unresolved.LiveParentAssetID)
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
}

// A still that is not a Live Photo must not adopt a video just because a local
// id looks close, and a second device's photo must not adopt this one's video.
func TestLivePairIgnoresAnotherDevicesStill(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := livePair(t, 0)

	if _, _, err := s.RecordAsset(ctx, video); err != nil {
		t.Fatalf("record paired video: %v", err)
	}

	other := still
	other.DeviceID = "someone-elses-phone"
	other.SHA256 = fmt.Sprintf("%064x", 99)
	other.MD5 = fmt.Sprintf("%032x", 99)
	otherID, _, err := s.RecordAsset(ctx, other)
	if err != nil {
		t.Fatalf("record the other device's still: %v", err)
	}

	if _, err := s.LiveVideoFor(ctx, otherID); err == nil {
		t.Error("a still on another device adopted this device's paired video")
	}
}

// The declaration is only meaningful on a video. A still that carries one would
// otherwise vanish from the timeline, which is the one failure here that loses
// something the archive is meant to show.
func TestLiveDeclarationIsIgnoredOnAStill(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	a.LiveParentLocalID = "some-other-photo"
	id, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if got.IsLivePair() {
		t.Errorf("LiveParentLocalID = %q on an image, want it dropped", got.LiveParentLocalID)
	}

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d timeline items, want the still to still be visible", len(page.Items))
	}
}

func TestTimelineHidesPairedVideosAndReportsTheirState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := livePair(t, 0)

	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}
	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record paired video: %v", err)
	}

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d items, want only the still", len(page.Items))
	}
	if page.Items[0].ID != stillID {
		t.Errorf("timeline showed %s, want the still %s", page.Items[0].ID, stillID)
	}
	if page.Items[0].LiveState != DerivedPending {
		t.Errorf("LiveState = %q, want %q while the rendition is queued", page.Items[0].LiveState, DerivedPending)
	}

	if err := s.SetLiveState(ctx, videoID, DerivedReady); err != nil {
		t.Fatalf("SetLiveState: %v", err)
	}
	states, err := s.TimelineStates(ctx, []string{stillID})
	if err != nil {
		t.Fatalf("TimelineStates: %v", err)
	}
	if states[stillID].LiveState != DerivedReady {
		t.Errorf("polled LiveState = %q, want %q", states[stillID].LiveState, DerivedReady)
	}
}

// An ordinary photo reports no motion at all rather than a string saying it has
// none, so the field's presence is the whole answer the grid needs.
func TestTimelineOmitsLiveStateForAnOrdinaryPhoto(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedAsset(t, s, 0, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if page.Items[0].LiveState != "" {
		t.Errorf("LiveState = %q on a plain photo, want empty", page.Items[0].LiveState)
	}
}

// Two phones holding the same photo dedup to one asset row, and each can attach
// its own copy of the paired video to it. A plain join would draw that photo
// once per attached video.
func TestTimelineDrawsAStillOnceWhenTwoDevicesAttachMotion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	still, video := livePair(t, 0)

	if _, _, err := s.RecordAsset(ctx, still); err != nil {
		t.Fatalf("record still: %v", err)
	}
	if _, _, err := s.RecordAsset(ctx, video); err != nil {
		t.Fatalf("record paired video: %v", err)
	}

	// The second phone: same still content, its own local ids, its own video.
	secondStill, secondVideo := still, video
	secondStill.DeviceID, secondVideo.DeviceID = "second-phone", "second-phone"
	secondVideo.SHA256 = fmt.Sprintf("%064x", 77)
	secondVideo.MD5 = fmt.Sprintf("%032x", 77)
	if _, _, err := s.RecordAsset(ctx, secondStill); err != nil {
		t.Fatalf("record the second device's still: %v", err)
	}
	if _, _, err := s.RecordAsset(ctx, secondVideo); err != nil {
		t.Fatalf("record the second device's paired video: %v", err)
	}

	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d items, want the one still exactly once", len(page.Items))
	}
}

func TestSetLiveStateRefusesAnAssetWithNoMotion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	if err := s.SetLiveState(ctx, id, DerivedReady); err != nil {
		t.Fatalf("SetLiveState: %v", err)
	}

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if got.LiveState != PlaybackNone {
		t.Errorf("LiveState = %q on a plain photo, want %q", got.LiveState, PlaybackNone)
	}
}

func TestLiveVideoForReportsNotFoundWithoutAPair(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	if _, err := s.LiveVideoFor(ctx, id); err == nil {
		t.Fatal("LiveVideoFor found a paired video for a plain photo")
	}
}
