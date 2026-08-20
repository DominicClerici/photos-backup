package worker

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/imagehash"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/merge"
	"github.com/dominicclerici/photos-backup/server/internal/snapchat"
)

// snapClip builds a clip shaped like a piece of an exported Snapchat memory:
// ten seconds of moving picture with sound, at the export's own resolution.
//
// Generated rather than a fixture because what these tests are about is several
// files becoming one, which needs several files whose contents differ.
func snapClip(t *testing.T, dir, name string, seconds float64, hue int) []byte {
	t.Helper()
	path := filepath.Join(dir, name)

	out, err := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i",
		"testsrc=size=180x320:rate=15:duration="+ftoa(seconds),
		"-f", "lavfi", "-i",
		"sine=frequency="+itoa(220+hue*40)+":sample_rate=44100:duration="+ftoa(seconds),
		"-vf", "hue=h="+itoa(hue*30),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-ac", "2", "-ar", "44100",
		"-shortest", path).CombinedOutput()
	if err != nil {
		t.Skipf("could not build a fixture clip (is ffmpeg installed?): %v: %s", err, out)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return body
}

func ftoa(v float64) string { return trimZeros(v) }

func trimZeros(v float64) string {
	s := []byte(nil)
	s = appendFloat(s, v)
	return string(s)
}

func appendFloat(dst []byte, v float64) []byte {
	// Two decimals is more precision than any duration here needs and avoids
	// scientific notation reaching ffmpeg's parser.
	whole := int(v)
	frac := int(math.Round((v - float64(whole)) * 100))
	dst = append(dst, []byte(itoa(whole))...)
	dst = append(dst, '.')
	if frac < 10 {
		dst = append(dst, '0')
	}
	return append(dst, []byte(itoa(frac))...)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// segments ingests n pieces of one recording, spaced exactly ten seconds apart
// in the way memories_history.json spaces them, with the sidecar an import
// would have written.
func (h *harness) segments(t *testing.T, n int) []db.Asset {
	t.Helper()
	dir := t.TempDir()
	start := time.Date(2018, 10, 7, 5, 42, 5, 0, time.UTC)

	out := make([]db.Asset, n)
	for i := range n {
		seconds := 10.0
		if i == n-1 {
			seconds = 6.0 // the tail of a recording
		}
		name := "2018-10-07_piece" + itoa(i) + "-main.mp4"
		asset := h.ingestBytes(t, name, db.MediaVideo, snapClip(t, dir, name, seconds, i))

		at := start.Add(time.Duration(i) * 10 * time.Second)
		sidecar, err := json.Marshal(snapchat.Sidecar{
			Export:           snapchat.Source,
			Kind:             "memories",
			File:             name,
			MediaID:          "piece" + itoa(i),
			Role:             snapchat.RoleMain,
			CapturedAt:       &at,
			CapturedAtSource: snapchat.TimeFromHistory,
			History:          json.RawMessage(`{"Date":"2018-10-07 05:42:05 UTC","Media Type":"Video","Location":"Latitude, Longitude: 40.72073, -73.979485"}`),
			HistoryMatch:     snapchat.MatchExact,
			Subtypes:         []string{snapchat.SubtypeMemory},
		})
		if err != nil {
			t.Fatal(err)
		}
		meta, err := db.ImportMetadataFrom(db.SourceSnapchat, sidecar, nil)
		if err != nil {
			t.Fatalf("build sidecar: %v", err)
		}
		// The history row above carries one Date for every piece, which is what
		// Snapchat writes; the per-piece instant is the one this importer
		// worked out and it is what the scan chains on.
		meta.TakenAt = &at
		if err := h.store.ApplyImportMetadata(context.Background(), asset.ID, meta); err != nil {
			t.Fatalf("describe piece %d: %v", i, err)
		}

		// The metadata job is what writes the duration the scan needs.
		h.claimAndRun(t, jobs.KindMetadata)
		out[i] = h.reload(t, asset.ID)
	}
	return out
}

// The whole of the split-video half, end to end: real clips, a real scan, a
// real ffmpeg, a real blob committed to a real store.
func TestScanJoinsASplitRecording(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	pieces := h.segments(t, 3)

	found, err := h.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if found.Segments != 1 {
		t.Fatalf("Scan found %d segment groups, want 1", found.Segments)
	}
	if found.Queued != 1 {
		t.Fatalf("Scan queued %d joins, want 1", found.Queued)
	}

	h.claimAndRun(t, jobs.KindMerge)

	// Every piece is in the trash, and something new is in the library.
	for _, piece := range pieces {
		if got := h.reload(t, piece.ID); got.DeletedAt == nil {
			t.Errorf("piece %s is still in the library", piece.OriginalFilename)
		}
	}

	groups, err := h.store.Groups(ctx, merge.KindSegments, db.MergeMerged, 10)
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 1 || groups[0].KeeperAssetID == nil {
		t.Fatalf("got %d merged groups; want one with a keeper", len(groups))
	}

	joined := h.reload(t, *groups[0].KeeperAssetID)
	if joined.MediaKind != db.MediaVideo {
		t.Errorf("the joined asset is a %s", joined.MediaKind)
	}
	if joined.DeletedAt != nil {
		t.Error("the joined recording went to the trash")
	}

	// It really is on the disk, and it really is the right length.
	path := h.Blobs.Path(joined.SHA256, joined.Ext)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the joined blob is not in the store: %v", err)
	}
	info, err := h.Video.Probe(ctx, path)
	if err != nil {
		t.Fatalf("probe the joined recording: %v", err)
	}
	if want := 26.0; math.Abs(info.DurationSeconds-want) > 0.5 {
		t.Errorf("joined duration = %.2fs, want ~%.0fs (10 + 10 + 6)", info.DurationSeconds, want)
	}

	// And the capture instant is the first piece's, so it lands in the timeline
	// where the recording began rather than where it ended.
	if joined.CapturedAt == nil {
		t.Fatal("the joined recording has no capture time")
	}
	if want := pieces[0].CapturedAt; want != nil && !joined.CapturedAt.Equal(*want) {
		t.Errorf("captured_at = %s, want the first piece's %s", joined.CapturedAt, *want)
	}
}

// An archived original with no manifest line is a blob a rebuild would not know
// what to do with, so the join records itself twice: once as bytes, once as the
// document saying what those bytes are.
func TestAJoinedRecordingIsWrittenToTheManifest(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.segments(t, 3)

	if _, err := h.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	h.claimAndRun(t, jobs.KindMerge)

	entries, err := manifest.Read(filepath.Join(h.root, "photos", "manifest.jsonl"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var asset, meta *manifest.Entry
	for i := range entries {
		switch {
		case entries[i].IsAsset() && filepath.Ext(entries[i].Filename) == ".mp4" &&
			entries[i].Filename == "2018-10-07_piece0-joined.mp4":
			asset = &entries[i]
		case entries[i].Type == manifest.KindMetadata && entries[i].ImportSource == db.SourceSnapchat:
			meta = &entries[i]
		}
	}
	if asset == nil {
		t.Fatalf("no asset line for the joined recording; got %d entries", len(entries))
	}
	if asset.SHA256 == "" || asset.Size == 0 {
		t.Error("the asset line names no bytes")
	}
	if meta == nil {
		t.Fatal("no metadata line describing the joined recording")
	}

	var sidecar snapchat.Sidecar
	if err := json.Unmarshal(meta.ImportSidecar, &sidecar); err != nil {
		t.Fatalf("decode the joined sidecar: %v", err)
	}
	if sidecar.Joined == nil {
		t.Fatal("the sidecar does not record that this file was joined")
	}
	if len(sidecar.Joined.Parts) != 3 {
		t.Errorf("the sidecar names %d parts, want 3", len(sidecar.Joined.Parts))
	}
	for i, part := range sidecar.Joined.Parts {
		if part.SHA256 == "" {
			t.Errorf("part %d is named but not addressed; the digest is how the blob is found again", i)
		}
	}
	// Identical pieces off one encoder, so this should not have needed a
	// re-encode — the joined file holds the camera's own frames.
	if sidecar.Joined.Method != "stream-copy" {
		t.Errorf("Method = %q (%s), want a stream copy of identical pieces",
			sidecar.Joined.Method, sidecar.Joined.Reason)
	}
	// And Snapchat's own row travelled across untouched, which is where the
	// joined file's coordinates come from.
	if len(sidecar.History) == 0 {
		t.Error("the first piece's history row did not travel onto the joined recording")
	}
}

// Running the scan twice must not join the same recording twice, and must not
// propose a group whose pieces are already in the trash.
func TestScanningTwiceJoinsOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.segments(t, 3)

	if _, err := h.Scan(ctx); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	h.claimAndRun(t, jobs.KindMerge)

	found, err := h.Scan(ctx)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if found.Segments != 0 {
		t.Errorf("the second scan proposed %d segment groups, want 0", found.Segments)
	}

	groups, err := h.store.Groups(ctx, merge.KindSegments, db.MergeMerged, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Errorf("%d merged groups, want 1", len(groups))
	}
}

// A merge job whose group was dismissed between the queueing and the claim has
// nothing to do, and must not treat that as a failure.
func TestAMergeWhoseGroupWentAwayIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.segments(t, 3)

	if _, err := h.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	groups, err := h.store.Groups(ctx, merge.KindSegments, db.MergePending, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("%d pending groups, want 1", len(groups))
	}
	if err := h.store.DismissGroup(ctx, groups[0].ID); err != nil {
		t.Fatalf("DismissGroup: %v", err)
	}

	job := h.claimAndRun(t, jobs.KindMerge)
	if state := jobStateByID(t, h, job.ID); state != string(jobs.StateDone) {
		t.Errorf("job state = %q, want done", state)
	}
	for _, m := range groups[0].Members {
		if got := h.reload(t, m.AssetID); got.DeletedAt != nil {
			t.Errorf("piece %s was trashed by a merge that had nothing to do", m.AssetID)
		}
	}
}

func jobStateByID(t *testing.T, h *harness, id int64) string {
	t.Helper()
	var state string
	if err := h.store.Pool().QueryRow(context.Background(),
		`select state from jobs where id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("read job state: %v", err)
	}
	return state
}

// The signature half, end to end: a real photograph decoded, hashed, and stored
// in a form the scan can read back.
func TestSignatureJobDescribesAPhotograph(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	asset := h.ingest(t, "photo.jpg", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindSignature)

	sigs, err := h.store.SignaturesForScan(ctx)
	if err != nil {
		t.Fatalf("SignaturesForScan: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("%d signatures, want 1", len(sigs))
	}
	got := sigs[0]
	if got.ID != asset.ID {
		t.Errorf("signature is for %s, want %s", got.ID, asset.ID)
	}
	// A real photograph is not flat, so neither hash should be degenerate.
	if got.Difference == 0 && got.Perceptual == 0 {
		t.Error("both hashes are zero; nothing was decoded")
	}
	if got.Aspect <= 0 {
		t.Error("no aspect ratio was recorded, and it is the cheapest guard there is")
	}
	if len(got.FrameDifference) != 0 {
		t.Errorf("a still came back with %d sampled frames", len(got.FrameDifference))
	}
}

// The video half of the same: twenty frames along the length of the clip, in
// the same format a still is hashed in.
func TestSignatureJobSamplesAVideo(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	dir := t.TempDir()
	h.ingestBytes(t, "clip.mp4", db.MediaVideo, snapClip(t, dir, "clip.mp4", 8, 2))

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindSignature)

	sigs, err := h.store.SignaturesForScan(ctx)
	if err != nil {
		t.Fatalf("SignaturesForScan: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("%d signatures, want 1", len(sigs))
	}
	got := sigs[0]
	if len(got.FrameDifference) < merge.FrameCount-1 {
		t.Fatalf("%d sampled frames, want about %d", len(got.FrameDifference), merge.FrameCount)
	}
	if len(got.FramePerceptual) != len(got.FrameDifference) {
		t.Errorf("%d difference hashes against %d perceptual ones",
			len(got.FrameDifference), len(got.FramePerceptual))
	}
	// The whole-picture hash is the middle of the sequence, not a poster frame
	// cropped a different way. See clipSignature.
	mid := len(got.FrameDifference) / 2
	if got.Difference != got.FrameDifference[mid] {
		t.Error("a clip's whole-picture hash is not the middle of its own sequence")
	}
	// A test source moves, so the frames must not all be the same.
	same := 0
	for _, h := range got.FrameDifference {
		if h == got.FrameDifference[0] {
			same++
		}
	}
	if same == len(got.FrameDifference) {
		t.Error("every sampled frame hashed identically; the sampler is not walking the clip")
	}
}

// Two copies of one photograph, one of them recompressed hard, found by the
// real pipeline rather than by hand-written hashes.
//
// iphone-portrait.heic rather than photo.jpg, and the difference matters. The
// latter is a synthetic vertical gradient — every row of it is uniform left to
// right — so its difference hash is zero by construction and stays zero however
// it is mangled. A real photograph put through half the resolution and quality
// 40 comes out one bit away on the difference hash and zero on the perceptual
// one, which is the measurement the threshold of nine is generous against.
func TestScanFindsARecompressedCopy(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	dir := t.TempDir()

	source := filepath.Join("..", "..", "testdata", "iphone-portrait.heic")
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	// Half the size and a punishing quality setting: the round trip an export
	// puts a photograph through.
	shrunk := filepath.Join(dir, "shrunk.jpg")
	if out, err := exec.Command("magick", source,
		"-resize", "50%", "-quality", "40", shrunk).CombinedOutput(); err != nil {
		t.Skipf("could not build a recompressed copy (is ImageMagick installed?): %v: %s", err, out)
	}
	copyBody, err := os.ReadFile(shrunk)
	if err != nil {
		t.Fatal(err)
	}

	a := h.ingestBytes(t, "original.heic", db.MediaImage, original)
	b := h.ingestBytes(t, "shrunk.jpg", db.MediaImage, copyBody)
	for range 2 {
		h.claimAndRun(t, jobs.KindMetadata)
	}
	for range 2 {
		h.claimAndRun(t, jobs.KindSignature)
	}

	found, err := h.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if found.Duplicates != 1 {
		t.Fatalf("Scan found %d duplicate groups, want 1", found.Duplicates)
	}

	groups, err := h.store.Groups(ctx, merge.KindDuplicate, db.MergePending, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("got %d groups; want one pair", len(groups))
	}
	// Best first, which here is unambiguous: the original is four times the
	// pixels of the copy.
	if groups[0].Members[0].AssetID != a.ID {
		t.Errorf("the suggested keeper is %s, want the full-size original %s",
			groups[0].Members[0].AssetID, a.ID)
	}
	if groups[0].Members[1].AssetID != b.ID {
		t.Errorf("second member is %s, want the shrunken copy", groups[0].Members[1].AssetID)
	}
}

// Two pictures that are not the same picture must not be grouped, or the review
// page is noise.
//
// This particular pair is the case the second hash exists for, and it is worth
// having in the suite rather than only in imagehash's unit tests. photo.jpg is a
// smooth vertical gradient and bare.jpg is a flat grey rectangle, so *both* have
// a difference hash of exactly zero — a detector built on that hash alone finds
// them identical. What separates them is the perceptual hash, which describes
// structure rather than gradient.
func TestScanLeavesUnrelatedPicturesAlone(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	h.ingest(t, "photo.jpg", db.MediaImage)
	h.ingest(t, "bare.jpg", db.MediaImage)
	for range 2 {
		h.claimAndRun(t, jobs.KindMetadata)
	}
	for range 2 {
		h.claimAndRun(t, jobs.KindSignature)
	}

	sigs, err := h.store.SignaturesForScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 2 {
		t.Fatalf("%d signatures, want 2", len(sigs))
	}
	if sigs[0].Difference != sigs[1].Difference {
		t.Skip("the fixtures no longer share a difference hash; this test is about the case where two do")
	}

	found, err := h.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if found.Duplicates != 0 {
		t.Errorf("two pictures sharing a difference hash were grouped as %d duplicates; "+
			"the perceptual hash is what is supposed to separate them", found.Duplicates)
	}
}

// The signature is a description of the picture, and the vault's whole promise
// is that this server cannot hold one.
func TestSignatureJobSkipsTheVault(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	asset := h.ingest(t, "photo.jpg", db.MediaImage)
	h.claimAndRun(t, jobs.KindMetadata)

	if _, err := h.store.Pool().Exec(ctx,
		`update assets set vault = 'hidden' where id = $1::uuid`, asset.ID); err != nil {
		t.Fatal(err)
	}

	h.claimAndRun(t, jobs.KindSignature)

	var count int
	if err := h.store.Pool().QueryRow(ctx,
		`select count(*)::int from asset_signatures where asset_id = $1::uuid`, asset.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Error("a hidden photograph was described anyway")
	}
}

// Nothing here should ever be handed a plane of the wrong size, but the sample
// edge is shared between three call sites in two packages and this is the one
// assertion that keeps them in step.
func TestSampleEdgeMatchesWhatTheDecodersProduce(t *testing.T) {
	h := newHarness(t)
	gray, err := h.Images.Sample(context.Background(),
		filepath.Join("..", "..", "testdata", "photo.jpg"), nil, imagehash.SampleEdge)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(gray) != imagehash.SampleSize {
		t.Fatalf("sample is %d bytes, want %d", len(gray), imagehash.SampleSize)
	}
	if _, err := imagehash.Compute(gray); err != nil {
		t.Fatalf("Compute: %v", err)
	}
}
