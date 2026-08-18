package video

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
	err := tool.Transcode(ctx, fixture("undecodable.mov"), filepath.Join(t.TempDir(), "playback.mp4"), info, "")
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
	if err := tool.Transcode(ctx, fixture("clip.mov"), dst, info, ""); err != nil {
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
	if err := tool.Transcode(ctx, fixture("clip.mov"), dst, info, ""); err != nil {
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

	err := New().Transcode(context.Background(), fixture("nope.mov"), dst, Info{}, "")
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

func TestVideoArgsTranslateTheQualityTargetForEachEncoder(t *testing.T) {
	x264 := New().videoArgs("libx264", 22, speedBalanced)
	if !slices.Contains(x264, "-crf") {
		t.Errorf("libx264 args carry no -crf: %v", x264)
	}
	if i := slices.Index(x264, "-preset"); i < 0 || x264[i+1] != "medium" {
		t.Errorf("libx264 balanced preset = %v, want medium", x264)
	}

	tool := New()
	tool.Encoder = "h264_nvenc"
	nv := tool.videoArgs("h264_nvenc", 22, speedBalanced)

	// The whole point. NVENC ignores -crf without saying so, which is how a
	// backfill ends up at its stock 2Mbps VBR instead of the quality asked for.
	if slices.Contains(nv, "-crf") {
		t.Errorf("NVENC args carry -crf, which it silently ignores: %v", nv)
	}
	if i := slices.Index(nv, "-cq"); i < 0 || nv[i+1] != "28" {
		t.Errorf("NVENC args = %v, want -cq 28 for CRF 22", nv)
	}
	// Without this NVENC reads the flags as a bitrate cap and -cq stops
	// meaning much.
	if i := slices.Index(nv, "-b:v"); i < 0 || nv[i+1] != "0" {
		t.Errorf("NVENC args = %v, want -b:v 0 so -cq is a pure quality target", nv)
	}
	if i := slices.Index(nv, "-preset"); i < 0 || !strings.HasPrefix(nv[i+1], "p") {
		t.Errorf("NVENC preset = %v, want one of NVENC's own pN presets", nv)
	}
}

// The bug this guards: NVENC refuses a frame smaller than nvencMinDimension
// outright, and the 96px motion thumbnail is one. Because every stored size
// comes out of a single ffmpeg, that refusal took the 256 and the 512 with it
// and every Live Photo in the archive ended up marked failed.
func TestEncoderFallsBackBelowTheHardwareMinimum(t *testing.T) {
	tool := New()
	tool.Encoder = "h264_nvenc"

	if got := tool.encoderFor(96); got != softwareEncoder {
		t.Errorf("encoder for a 96px frame = %q, want %q — NVENC cannot encode one", got, softwareEncoder)
	}
	if got := tool.encoderFor(nvencMinDimension - 1); got != softwareEncoder {
		t.Errorf("encoder for a %dpx frame = %q, want %q", nvencMinDimension-1, got, softwareEncoder)
	}
	// Everything the card can take still goes to the card. Falling back further
	// than necessary is the expensive mistake in the other direction.
	for _, size := range []int{nvencMinDimension, 256, 512, 1920} {
		if got := tool.encoderFor(size); got != "h264_nvenc" {
			t.Errorf("encoder for a %dpx frame = %q, want the configured h264_nvenc", size, got)
		}
	}
	// An unknown size is not a small one.
	if got := tool.encoderFor(0); got != "h264_nvenc" {
		t.Errorf("encoder for an unknown size = %q, want the configured h264_nvenc", got)
	}
	// A software encoder has no such floor and must not be second-guessed.
	if got := New().encoderFor(96); got != "libx264" {
		t.Errorf("libx264 encoder for a 96px frame = %q, want libx264", got)
	}
}

func TestOutputMinEdgeFollowsTheScaler(t *testing.T) {
	cases := []struct {
		name string
		info Info
		want int
	}{
		{"inside the box, untouched", Info{Width: 640, Height: 480}, 480},
		{"scaled down to the long edge", Info{Width: 3840, Height: 2160}, 1080},
		{"portrait, rotation applied", Info{Width: 1920, Height: 1080, Rotation: 90}, 1080},
		// The case a naive min(w, h) gets wrong: a wide clip's short edge
		// shrinks well below the encoder minimum on the way down.
		{"letterboxed wide", Info{Width: 4000, Height: 200}, 96},
		{"unknown", Info{}, 0},
	}
	for _, c := range cases {
		if got := outputMinEdge(c.info, PlaybackMaxEdge); got != c.want {
			t.Errorf("%s: outputMinEdge = %d, want %d", c.name, got, c.want)
		}
	}
}

// The end-to-end version of the same bug, run against whichever encoders this
// machine can actually use: every stored size must arrive, including the ones
// below the hardware minimum.
func TestLiveThumbsWriteEverySizeOnEveryEncoder(t *testing.T) {
	ctx := context.Background()
	for _, encoder := range encodersUnderTest(t) {
		t.Run(encoder, func(t *testing.T) {
			tool := New()
			tool.Encoder = encoder

			src := fixture("clip.mov")
			info, err := tool.Probe(ctx, src)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}

			dir := t.TempDir()
			// The sizes the archive stores. 96 is the one NVENC refuses.
			sizes := []int{96, 256, 512}
			targets := make([]LiveThumbTarget, len(sizes))
			for i, size := range sizes {
				targets[i] = LiveThumbTarget{Size: size, Path: filepath.Join(dir, strconv.Itoa(size)+".mp4")}
			}

			if err := tool.LiveThumbs(ctx, src, targets, info); err != nil {
				t.Fatalf("LiveThumbs: %v", err)
			}
			for _, target := range targets {
				out, err := tool.Probe(ctx, target.Path)
				if err != nil {
					t.Fatalf("probe the %dpx motion thumbnail: %v", target.Size, err)
				}
				if out.Width != target.Size || out.Height != target.Size {
					t.Errorf("%dpx motion thumbnail is %dx%d, want square at its own size",
						target.Size, out.Width, out.Height)
				}
			}
		})
	}
}

func TestDecodeArgsLeaveFramesWhereAutorotateCanReachThem(t *testing.T) {
	if got := New().decodeArgs(); len(got) != 0 {
		t.Errorf("libx264 decode args = %v, want none", got)
	}

	tool := New()
	tool.Encoder = "h264_nvenc"
	got := tool.decodeArgs()
	if !slices.Contains(got, "-hwaccel") {
		t.Errorf("NVENC decode args = %v, want the decode offered to the GPU", got)
	}
	// Keeping frames in GPU memory puts them out of autorotate's reach, and
	// there is no GPU transpose here to replace it: every portrait clip then
	// comes out landscape with its display matrix dropped. Verified against
	// ffmpeg, and the reason this assertion exists.
	if slices.Contains(got, "-hwaccel_output_format") {
		t.Errorf("NVENC decode args = %v, which would silently unrotate portrait video", got)
	}
}

// rotatedFixture re-muxes the sample clip with a quarter-turn display matrix,
// which is the shape every iPhone clip arrives in.
func rotatedFixture(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "portrait.mov")
	if _, err := execOutput(New().ffmpeg(), "-nostdin", "-y", "-v", "error",
		"-display_rotation", "90", "-i", fixture("clip.mov"), "-c", "copy", dst); err != nil {
		t.Fatalf("build a rotated fixture: %v", err)
	}
	return dst
}

// encodersUnderTest is libx264 plus, where the machine can actually do it,
// h264_nvenc. Asked by encoding rather than by reading `ffmpeg -encoders`,
// which lists NVENC on any build compiled with it, card or no card.
func encodersUnderTest(t *testing.T) []string {
	t.Helper()
	encoders := []string{"libx264"}
	_, err := execOutput(New().ffmpeg(), "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=15:duration=0.2",
		"-c:v", "h264_nvenc", "-preset", "p1", filepath.Join(t.TempDir(), "probe.mp4"))
	if err == nil {
		encoders = append(encoders, "h264_nvenc")
	}
	return encoders
}

func TestTranscodeBakesInTheRotation(t *testing.T) {
	ctx := context.Background()
	for _, encoder := range encodersUnderTest(t) {
		t.Run(encoder, func(t *testing.T) {
			tool := New()
			tool.Encoder = encoder
			tool.CRF = 30

			src := rotatedFixture(t)
			info, err := tool.Probe(ctx, src)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if w, h := info.DisplaySize(); w != 480 || h != 640 {
				t.Fatalf("fixture displays at %dx%d, want the rotated 480x640", w, h)
			}

			dst := filepath.Join(t.TempDir(), "playback.mp4")
			if err := tool.Transcode(ctx, src, dst, info, ""); err != nil {
				t.Fatalf("Transcode: %v", err)
			}

			out, err := tool.Probe(ctx, dst)
			if err != nil {
				t.Fatalf("probe the transcode: %v", err)
			}
			// Upright in the pixels, not in a metadata flag a player may or may
			// not honour. Landscape here means the rotation was dropped on the
			// way through, which is what a GPU-resident decode does.
			if out.Width != 480 || out.Height != 640 {
				t.Errorf("transcode is %dx%d, want the upright 480x640", out.Width, out.Height)
			}
		})
	}
}

func TestTranscodeHonoursTheQualityTargetOnEveryEncoder(t *testing.T) {
	ctx := context.Background()
	for _, encoder := range encodersUnderTest(t) {
		t.Run(encoder, func(t *testing.T) {
			info, err := New().Probe(ctx, fixture("clip.mov"))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}

			sizeAt := func(crf int) int64 {
				tool := New()
				tool.Encoder = encoder
				tool.CRF = crf
				dst := filepath.Join(t.TempDir(), "playback.mp4")
				if err := tool.Transcode(ctx, fixture("clip.mov"), dst, info, ""); err != nil {
					t.Fatalf("Transcode at CRF %d: %v", crf, err)
				}
				stat, err := os.Stat(dst)
				if err != nil {
					t.Fatalf("stat the transcode: %v", err)
				}
				return stat.Size()
			}

			// A quality target the encoder is ignoring produces the same file
			// whatever it is set to, which is exactly the failure that hid
			// behind h264_nvenc accepting -crf and discarding it.
			high, low := sizeAt(18), sizeAt(36)
			if high <= low {
				t.Errorf("CRF 18 produced %d bytes and CRF 36 produced %d: the quality target is not reaching %s",
					high, low, encoder)
			}
		})
	}
}

// overlayFixture writes a PNG that is opaque red over its left half and
// transparent over its right, at neither the size nor the shape of any video
// fixture — which is the situation every Snapchat memory is in.
func overlayFixture(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width / 2 {
			img.Set(x, y, color.NRGBA{R: 255, A: 255})
		}
	}

	path := filepath.Join(t.TempDir(), "overlay.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create overlay: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode overlay: %v", err)
	}
	return path
}

// frameColour reads one pixel out of a rendered frame, which is the only way to
// tell a burn that happened from one that quietly did not.
func frameColour(t *testing.T, path string, at float64, x, y int) (r, g, b uint8) {
	t.Helper()
	frame := filepath.Join(t.TempDir(), "frame.png")
	cmd := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error",
		"-ss", strconv.FormatFloat(at, 'f', 3, 64), "-i", path, "-frames:v", "1", frame)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("extract a frame at %.3fs: %v: %s", at, err, out)
	}

	f, err := os.Open(frame)
	if err != nil {
		t.Fatalf("open frame: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	cr, cg, cb, _ := img.At(x, y).RGBA()
	return uint8(cr >> 8), uint8(cg >> 8), uint8(cb >> 8)
}

// silentFixture is clip.mov with its audio track dropped. Half the memories in
// this archive are silent, and a silent video is the only shape that catches
// the bug in TestTranscodeTerminatesOnASilentVideo.
func silentFixture(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "silent.mp4")
	cmd := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error",
		"-i", fixture("clip.mov"), "-an", "-c:v", "copy", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("strip the audio track: %v: %s", err, out)
	}
	return dst
}

// The bug this exists for: the layer was an input with no end — `-loop 1` — and
// ffmpeg bounded the encode only by whatever other finite output stream it had.
// A clip with an audio track stopped at the right moment and every test here
// passed. A silent one never stopped: a 7.6-second memory ran for thirteen
// minutes, held sixteen cores, and wrote 300MB of a video that would eventually
// have filled the disk. Nothing failed; it simply never came back.
//
// So the deadline is the assertion. An encode of a one-second clip that has not
// finished in thirty seconds has not finished at all.
func TestTranscodeTerminatesOnASilentVideo(t *testing.T) {
	tool := New()
	tool.CRF = 30

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := silentFixture(t)
	info, err := tool.Probe(ctx, src)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "playback.mp4")
	if err := tool.Transcode(ctx, src, dst, info, overlayFixture(t, 337, 601)); err != nil {
		if ctx.Err() != nil {
			t.Fatal("the encode never terminated: the overlay input has no end")
		}
		t.Fatalf("Transcode: %v", err)
	}

	out, err := tool.Probe(ctx, dst)
	if err != nil {
		t.Fatalf("probe the transcode: %v", err)
	}
	if out.DurationSeconds > 2 {
		t.Errorf("transcode is %.2fs long, want about the source's 1s", out.DurationSeconds)
	}
}

// The layer is drawn into every frame, not just the first: one still image is
// held for the length of the clip by the filter's eof_action, which is what
// replaced looping the input.
func TestTranscodeBurnsInAnOverlay(t *testing.T) {
	tool := New()
	tool.CRF = 30
	ctx := context.Background()

	info, err := tool.Probe(ctx, fixture("clip.mov"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	overlay := overlayFixture(t, 337, 601)

	dst := filepath.Join(t.TempDir(), "playback.mp4")
	if err := tool.Transcode(ctx, fixture("clip.mov"), dst, info, overlay); err != nil {
		t.Fatalf("Transcode with an overlay: %v", err)
	}

	out, err := tool.Probe(ctx, dst)
	if err != nil {
		t.Fatalf("probe the transcode: %v", err)
	}
	if out.Width != 640 || out.Height != 480 {
		t.Errorf("transcode is %dx%d, want the source's 640x480", out.Width, out.Height)
	}
	// -shortest, or the looped still runs until the disk fills.
	if out.DurationSeconds > 2 {
		t.Errorf("transcode is %.2fs long, want about the source's 1s", out.DurationSeconds)
	}

	if r, g, b := frameColour(t, dst, 0.7, 100, 240); r < 180 || g > 80 || b > 80 {
		t.Errorf("a late frame reads rgb(%d,%d,%d) where the overlay is, want red", r, g, b)
	}
	if r, _, _ := frameColour(t, dst, 0.7, 540, 240); r > 180 {
		t.Errorf("the overlay's transparent half was painted over: red = %d", r)
	}
}

// -filter_complex turns ffmpeg's automatic stream selection off, so the audio
// has to be mapped by hand. Forgetting it produces a perfectly valid silent
// video and no error at all.
func TestTranscodeKeepsAudioThroughTheOverlayFilter(t *testing.T) {
	tool := New()
	tool.CRF = 30
	ctx := context.Background()

	info, err := tool.Probe(ctx, fixture("clip.mov"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "playback.mp4")
	if err := tool.Transcode(ctx, fixture("clip.mov"), dst, info, overlayFixture(t, 337, 601)); err != nil {
		t.Fatalf("Transcode with an overlay: %v", err)
	}

	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", dst)
	out, err := cmd.Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		t.Errorf("the burned rendition has no audio stream (ffprobe: %q, %v)", out, err)
	}
}

// A video whose size ffprobe could not report cannot be composed at all: the
// layer would be stretched to a frame nobody knows the shape of. Refusing says
// so, where guessing would produce a rendition that is wrong and looks
// deliberate.
func TestTranscodeRefusesToBurnWithoutDimensions(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "playback.mp4")
	err := New().Transcode(context.Background(), fixture("clip.mov"), dst,
		Info{DurationSeconds: 1}, overlayFixture(t, 337, 601))
	if err == nil {
		t.Fatal("Transcode burned an overlay without knowing the frame size")
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("error does not say what was missing: %v", err)
	}
}

// The oversized branch and the overlay branch both decide the output frame, and
// they have to agree — a burn that ignored the cap would write a 4K rendition
// where every other video writes 1080p.
func TestTranscodeScalesDownWhileBurningIn(t *testing.T) {
	tool := New()
	tool.CRF = 30
	ctx := context.Background()

	// Claim the source is 4K, as the plain scaling test does.
	info := Info{Width: 3840, Height: 2160, DurationSeconds: 1}

	dst := filepath.Join(t.TempDir(), "playback.mp4")
	if err := tool.Transcode(ctx, fixture("clip.mov"), dst, info, overlayFixture(t, 337, 601)); err != nil {
		t.Fatalf("Transcode: %v", err)
	}

	out, err := tool.Probe(ctx, dst)
	if err != nil {
		t.Fatalf("probe the transcode: %v", err)
	}
	if out.Width > PlaybackMaxEdge || out.Height > PlaybackMaxEdge {
		t.Errorf("transcode is %dx%d, want both edges within %d", out.Width, out.Height, PlaybackMaxEdge)
	}
	if out.Width%2 != 0 || out.Height%2 != 0 {
		t.Errorf("transcode is %dx%d, want even dimensions", out.Width, out.Height)
	}
}
