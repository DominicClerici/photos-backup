package video

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(name string) string { return filepath.Join("..", "..", "testdata", name) }

func TestProbeReadsSizeAndDuration(t *testing.T) {
	got, err := New().Probe(context.Background(), fixture("clip.mov"))
	if err != nil {
		t.Fatalf("Probe: %v\n\nIs ffmpeg installed?", err)
	}

	if got.Width != 640 || got.Height != 480 {
		t.Errorf("size = %dx%d, want 640x480", got.Width, got.Height)
	}
	if got.DurationSeconds < 0.5 || got.DurationSeconds > 2 {
		t.Errorf("DurationSeconds = %v, want ~1", got.DurationSeconds)
	}
}

func TestProbeRejectsAFileWithNoVideoStream(t *testing.T) {
	_, err := New().Probe(context.Background(), fixture("photo.jpg"))
	// A JPEG probes as a single-frame mjpeg stream, so what must not happen is
	// a panic or a silently zeroed Info — either outcome here is acceptable as
	// long as it is honest.
	if err == nil {
		info, _ := New().Probe(context.Background(), fixture("photo.jpg"))
		if info.Width == 0 {
			t.Error("Probe reported a zero-width stream without an error")
		}
	}
}

func TestProbeReportsAMissingFile(t *testing.T) {
	if _, err := New().Probe(context.Background(), fixture("nope.mov")); err == nil {
		t.Fatal("Probe succeeded on a missing file")
	}
}

func TestDisplaySizeSwapsForAQuarterTurn(t *testing.T) {
	landscape := Info{Width: 1920, Height: 1080}

	for _, rotation := range []int{90, 270, -90} {
		info := landscape
		info.Rotation = rotation
		w, h := info.DisplaySize()
		if w != 1080 || h != 1920 {
			t.Errorf("rotation %d: DisplaySize = %dx%d, want 1080x1920", rotation, w, h)
		}
	}

	for _, rotation := range []int{0, 180, 360} {
		info := landscape
		info.Rotation = rotation
		w, h := info.DisplaySize()
		if w != 1920 || h != 1080 {
			t.Errorf("rotation %d: DisplaySize = %dx%d, want 1920x1080", rotation, w, h)
		}
	}
}

func TestPosterFrameProducesAStill(t *testing.T) {
	tool := New()
	ctx := context.Background()

	info, err := tool.Probe(ctx, fixture("clip.mov"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "poster.jpg")
	if err := tool.PosterFrame(ctx, fixture("clip.mov"), dst, info); err != nil {
		t.Fatalf("PosterFrame: %v", err)
	}

	stat, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat poster: %v", err)
	}
	if stat.Size() == 0 {
		t.Error("poster frame is empty")
	}
}

// A clip shorter than the default seek point must still yield a frame rather
// than seeking past its own end and producing nothing.
func TestPosterFrameHandlesAClipShorterThanTheSeek(t *testing.T) {
	tool := New()
	ctx := context.Background()

	info := Info{Width: 640, Height: 480, DurationSeconds: 1}
	dst := filepath.Join(t.TempDir(), "poster.jpg")
	if err := tool.PosterFrame(ctx, fixture("clip.mov"), dst, info); err != nil {
		t.Fatalf("PosterFrame: %v", err)
	}

	stat, err := os.Stat(dst)
	if err != nil || stat.Size() == 0 {
		t.Fatalf("poster frame missing or empty: %v", err)
	}
}

// The fixture is exactly 1.000s, so the old unconditional 1s seek landed past
// its last frame. Probe reporting no duration at all is the case that used to
// take that seek blindly.
func TestPosterFrameSeeksFromTheStartWhenTheDurationIsUnknown(t *testing.T) {
	tool := New()
	dst := filepath.Join(t.TempDir(), "poster.jpg")

	if err := tool.PosterFrame(context.Background(), fixture("clip.mov"), dst, Info{Width: 640, Height: 480}); err != nil {
		t.Fatalf("PosterFrame with an unknown duration: %v", err)
	}
	if stat, err := os.Stat(dst); err != nil || stat.Size() == 0 {
		t.Fatalf("poster frame missing or empty: %v", err)
	}
}

// A duration that overstates the clip is what a container with stale metadata
// looks like. The seek it produces overshoots, and the retry has to catch it.
func TestPosterFrameFallsBackWhenTheSeekOvershoots(t *testing.T) {
	tool := New()
	dst := filepath.Join(t.TempDir(), "poster.jpg")

	info := Info{Width: 640, Height: 480, DurationSeconds: 30}
	if err := tool.PosterFrame(context.Background(), fixture("clip.mov"), dst, info); err != nil {
		t.Fatalf("PosterFrame over an overstated duration: %v", err)
	}
	if stat, err := os.Stat(dst); err != nil || stat.Size() == 0 {
		t.Fatalf("poster frame missing or empty: %v", err)
	}
}

// The bug this replaced: ffmpeg exits 0 leaving an unusable file, and the first
// thing to notice is ImageMagick, two subprocesses and one confusing error
// message later.
func TestCheckJPEGRejectsWhatImageMagickWouldChokeOn(t *testing.T) {
	valid, err := os.ReadFile(fixture("photo.jpg"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "empty"},
		{"truncated", valid[:len(valid)/2], "end-of-image"},
		{"not a jpeg at all", []byte("nowhere near a JPEG"), "do not start as a JPEG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "poster.jpg")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			err := checkJPEG(path)
			if err == nil {
				t.Fatal("checkJPEG accepted a file ImageMagick cannot read")
			}
			if !errors.Is(err, ErrNoFrame) {
				t.Errorf("error does not wrap ErrNoFrame, so the retry will not fire: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "poster.jpg")
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := checkJPEG(path); err != nil {
		t.Errorf("checkJPEG rejected a valid JPEG: %v", err)
	}
}

// undecodable.mov is clip.mov remuxed with +faststart and cut off just after
// its moov, so ffprobe reads a complete 640x480 h264 header and ffmpeg cannot
// decode a single frame from it. That is the shape of the Live Photo that
// started all this: describable, not decodable.
func TestPosterFrameReportsNoFrameForAnUndecodableFile(t *testing.T) {
	tool := New()
	ctx := context.Background()

	info, err := tool.Probe(ctx, fixture("undecodable.mov"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Width != 640 || info.Height != 480 {
		t.Fatalf("Probe read %dx%d; the fixture is meant to describe itself fine", info.Width, info.Height)
	}

	err = tool.PosterFrame(ctx, fixture("undecodable.mov"), filepath.Join(t.TempDir(), "poster.jpg"), info)
	if err == nil {
		t.Fatal("PosterFrame reported success on a file with no decodable frames")
	}
	// The worker degrades on this specific sentinel, so the wrapping matters as
	// much as the failure.
	if !errors.Is(err, ErrNoFrame) {
		t.Errorf("error does not wrap ErrNoFrame, so the worker will fail the asset instead of degrading: %v", err)
	}
}

// The bug this guards: ffmpeg exits 0 having encoded nothing, and the rendition
// is marked ready and plays nothing.
func TestTranscodeRejectsAnUnplayableResult(t *testing.T) {
	tool := New()
	tool.CRF = 30
	ctx := context.Background()

	info := Info{Width: 640, Height: 480, DurationSeconds: 1}
	err := tool.Transcode(ctx, fixture("undecodable.mov"), filepath.Join(t.TempDir(), "playback.mp4"), info)
	if err == nil {
		t.Fatal("Transcode reported success on a file with no decodable frames")
	}
	if !errors.Is(err, ErrNotPlayable) {
		t.Errorf("error does not wrap ErrNotPlayable: %v", err)
	}
}

func TestCheckPlayableRejectsAnEmptyOrBogusFile(t *testing.T) {
	tool := New()
	ctx := context.Background()
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tool.checkPlayable(ctx, empty); err == nil || !errors.Is(err, ErrNotPlayable) {
		t.Errorf("checkPlayable on an empty file = %v, want ErrNotPlayable", err)
	}

	if err := tool.checkPlayable(ctx, filepath.Join(dir, "absent.mp4")); err == nil || !errors.Is(err, ErrNotPlayable) {
		t.Errorf("checkPlayable on a missing file = %v, want ErrNotPlayable", err)
	}
}

func TestTranscodeProducesAPlayableMP4(t *testing.T) {
	tool := New()
	// The fixture is tiny; a fast preset keeps the test quick without changing
	// what is being verified.
	tool.CRF = 30
	ctx := context.Background()

	info, err := tool.Probe(ctx, fixture("clip.mov"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "playback.mp4")
	if err := tool.Transcode(ctx, fixture("clip.mov"), dst, info); err != nil {
		t.Fatalf("Transcode: %v", err)
	}

	out, err := tool.Probe(ctx, dst)
	if err != nil {
		t.Fatalf("probe the transcode: %v", err)
	}
	// Under the cap, so the dimensions must survive untouched.
	if out.Width != 640 || out.Height != 480 {
		t.Errorf("transcode is %dx%d, want the source's 640x480", out.Width, out.Height)
	}
	if out.DurationSeconds < 0.5 {
		t.Errorf("transcode duration = %v, want ~1", out.DurationSeconds)
	}

	assertCodec(t, dst, "h264")
}

func TestTranscodeScalesDownOversizedVideo(t *testing.T) {
	tool := New()
	tool.CRF = 30
	ctx := context.Background()

	// Claim the source is 4K so the scaler engages. The filter reads the real
	// frames either way; what is under test is that the branch fires and
	// produces valid output.
	info := Info{Width: 3840, Height: 2160, DurationSeconds: 1}

	dst := filepath.Join(t.TempDir(), "playback.mp4")
	if err := tool.Transcode(ctx, fixture("clip.mov"), dst, info); err != nil {
		t.Fatalf("Transcode: %v", err)
	}

	out, err := tool.Probe(ctx, dst)
	if err != nil {
		t.Fatalf("probe the transcode: %v", err)
	}
	if out.Width > PlaybackMaxEdge || out.Height > PlaybackMaxEdge {
		t.Errorf("transcode is %dx%d, want both edges within %d", out.Width, out.Height, PlaybackMaxEdge)
	}
	// force_divisible_by=2 matters: H.264 cannot encode odd dimensions in
	// yuv420p, and the failure is an obscure ffmpeg error rather than a clear one.
	if out.Width%2 != 0 || out.Height%2 != 0 {
		t.Errorf("transcode is %dx%d, want even dimensions", out.Width, out.Height)
	}
}

func TestTranscodeReportsFFmpegStderr(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "playback.mp4")

	err := New().Transcode(context.Background(), fixture("nope.mov"), dst, Info{})
	if err == nil {
		t.Fatal("Transcode succeeded on a missing file")
	}
	// This text ends up in jobs.last_error, so it has to name the input.
	if !strings.Contains(err.Error(), "nope.mov") {
		t.Errorf("error does not mention the failing input: %v", err)
	}
}

func assertCodec(t *testing.T, path, want string) {
	t.Helper()
	tool := New()
	cmd := []string{"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", path}

	out, err := execOutput(tool.ffprobe(), cmd...)
	if err != nil {
		t.Fatalf("read codec: %v", err)
	}
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("codec = %q, want %q — browsers outside Safari will not play anything else", got, want)
	}
}
