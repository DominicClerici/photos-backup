// Package video drives ffmpeg and ffprobe. Like the other media packages it
// builds argv and parses output; the decision of when to run any of this lives
// in the worker.
//
// The transcode exists because iPhone video is HEVC in a QuickTime container,
// which Safari plays, Firefox does not, and Chrome handles inconsistently
// depending on platform and hardware. A browser cannot be asked to cope, so the
// archive keeps the original untouched and serves a plain H.264/AAC MP4
// alongside it.
package video

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// PlaybackMaxEdge bounds the transcode. 1080p is the point past which a home
// gallery gains nothing visible and starts costing real disk and encode time;
// the original is always one click away for anything more.
const PlaybackMaxEdge = 1920

type Tool struct {
	FFmpeg  string
	FFprobe string
	// Encoder is the ffmpeg video encoder. libx264 works everywhere; the
	// archive machine has an NVIDIA card and can try h264_nvenc without a code
	// change.
	Encoder string
	// CRF is the x264 quality target. Ignored by hardware encoders, which is
	// fine — they read their own rate-control flags and default sensibly.
	CRF int
}

func New() *Tool {
	return &Tool{FFmpeg: "ffmpeg", FFprobe: "ffprobe", Encoder: "libx264", CRF: 22}
}

// Info is what ffprobe could tell us about a video's picture.
type Info struct {
	Width           int
	Height          int
	DurationSeconds float64
	// Rotation is the display matrix angle in degrees. ffmpeg applies it
	// automatically when filtering, so it matters here only for working out the
	// dimensions a viewer will actually see.
	Rotation int
}

// DisplaySize returns the dimensions after rotation, which is what a player
// shows and what the transcode's scaler sees.
func (i Info) DisplaySize() (width, height int) {
	switch ((i.Rotation % 360) + 360) % 360 {
	case 90, 270:
		return i.Height, i.Width
	}
	return i.Width, i.Height
}

type probeOutput struct {
	Streams []struct {
		CodecType   string `json:"codec_type"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Duration    string `json:"duration"`
		Rotation    any    `json:"rotation"`
		SideDataLst []struct {
			Rotation any `json:"rotation"`
		} `json:"side_data_list"`
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func (t *Tool) Probe(ctx context.Context, src string) (Info, error) {
	cmd := exec.CommandContext(ctx, t.ffprobe(),
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,duration,codec_type:stream_side_data=rotation:stream_tags=rotate:format=duration",
		"-of", "json",
		src,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return Info{}, fmt.Errorf("ffprobe %s: %w: %s", src, err, bytes.TrimSpace(stderr.Bytes()))
	}

	var out probeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return Info{}, fmt.Errorf("parse ffprobe output for %s: %w", src, err)
	}
	if len(out.Streams) == 0 {
		return Info{}, fmt.Errorf("ffprobe %s: no video stream", src)
	}

	s := out.Streams[0]
	info := Info{Width: s.Width, Height: s.Height}

	// Duration lives on the stream for some containers and only on the format
	// for others.
	info.DurationSeconds = parseSeconds(s.Duration)
	if info.DurationSeconds == 0 {
		info.DurationSeconds = parseSeconds(out.Format.Duration)
	}

	// Rotation is reported in three different places depending on ffmpeg
	// version and container: stream side data, a legacy `rotate` tag, or
	// directly on the stream.
	info.Rotation = firstRotation(s.Rotation, s.Tags["rotate"])
	for _, sd := range s.SideDataLst {
		if info.Rotation != 0 {
			break
		}
		info.Rotation = firstRotation(sd.Rotation)
	}
	return info, nil
}

// PosterFrame writes a single still to dst, which must be a path ending in a
// format ffmpeg recognises.
//
// It writes to a file rather than a pipe because the frame is then handed to
// ImageMagick to be squared off, and two subprocesses joined by a pipe fail in
// ways that are miserable to diagnose — a file makes each half independently
// inspectable when something goes wrong.
func (t *Tool) PosterFrame(ctx context.Context, src, dst string, info Info) error {
	// Seek a little way in. The first frame of a phone video is often the
	// shutter still rising, so it is darker and blurrier than anything after it.
	//
	// An unknown duration is precisely when that seek cannot be justified, so it
	// is not attempted: Probe reports 0 when neither the stream nor the format
	// carries one, and a blind 1s seek into a clip that might be shorter buys a
	// marginally better frame at the risk of no frame at all.
	seek := 0.0
	switch {
	case info.DurationSeconds >= 2:
		seek = 1.0
	case info.DurationSeconds > 0:
		seek = info.DurationSeconds / 2
	}

	err := t.posterAt(ctx, src, dst, seek)
	if err != nil && seek > 0 && errors.Is(err, ErrNoFrame) {
		// The seek overshot a clip shorter than it claimed to be. A first frame
		// that is slightly dark beats a gallery tile that never appears.
		return t.posterAt(ctx, src, dst, 0)
	}
	return err
}

// ErrNoFrame marks the case where ffmpeg was run but no usable still came out
// of it, which is recoverable by seeking differently. A failure to run at all is
// not, and does not carry this.
//
// It is exported because the distinction matters to the caller too: a container
// ffmpeg can describe but not decode should cost a thumbnail, not the asset.
var ErrNoFrame = errors.New("no frame was written")

// ErrNotPlayable is Transcode's equivalent — ffmpeg ran and left behind
// something no browser will play.
var ErrNotPlayable = errors.New("no playable video was written")

func (t *Tool) posterAt(ctx context.Context, src, dst string, seek float64) error {
	args := []string{"-nostdin", "-y", "-v", "error"}
	if seek > 0 {
		// Before -i, so ffmpeg seeks by keyframe instead of decoding forward to
		// the timestamp. On a 3GB clip that is the difference between instant
		// and minutes.
		args = append(args, "-ss", strconv.FormatFloat(seek, 'f', 3, 64))
	}
	args = append(args, "-i", src, "-frames:v", "1", "-q:v", "2", dst)

	cmd := exec.CommandContext(ctx, t.ffmpeg(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	// stderr is worth keeping even when ffmpeg claims success, because the
	// interesting failures here are the ones where it does.
	complaint := strings.TrimSpace(stderr.String())

	// Whether the frame arrived matters more than what ffmpeg exited with, and
	// the two disagree in both directions. A seek past the last frame exits 234
	// on one build and 0 on another, and exiting 0 having written nothing
	// usable is the case that surfaced as an ImageMagick parse error two
	// subprocesses later, naming neither ffmpeg nor the offset that caused it.
	if err := checkJPEG(dst); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("extract poster frame from %s at %.3fs: %w", src, seek, ctx.Err())
		}
		return fmt.Errorf("extract poster frame from %s at %.3fs: %w (ffmpeg %s; stderr: %s)",
			src, seek, err, exitStatus(runErr), orNone(complaint))
	}
	if runErr != nil {
		return fmt.Errorf("extract poster frame from %s at %.3fs: %w: %s", src, seek, runErr, complaint)
	}
	return nil
}

func exitStatus(err error) string {
	if err == nil {
		return "exited 0"
	}
	return err.Error()
}

// checkJPEG rejects the empty and truncated files ffmpeg leaves behind on the
// failures it does not report. It is a shape check, not a decode: anything that
// gets past it is ImageMagick's problem, and ImageMagick reports it well.
func checkJPEG(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoFrame, err)
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoFrame, err)
	}
	if size == 0 {
		return fmt.Errorf("%w: the file is empty", ErrNoFrame)
	}

	var head [2]byte
	if _, err := f.ReadAt(head[:], 0); err != nil || head != [2]byte{0xFF, 0xD8} {
		return fmt.Errorf("%w: %d bytes that do not start as a JPEG", ErrNoFrame, size)
	}

	// The end-of-image marker is what a truncated write loses, and losing it is
	// exactly what ImageMagick calls "insufficient image data".
	tail := make([]byte, min(size, 16))
	if _, err := f.ReadAt(tail, size-int64(len(tail))); err != nil {
		return fmt.Errorf("%w: %w", ErrNoFrame, err)
	}
	if !bytes.Contains(tail, []byte{0xFF, 0xD9}) {
		return fmt.Errorf("%w: %d bytes ending without a JPEG end-of-image marker", ErrNoFrame, size)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Transcode writes an H.264/AAC MP4 that any browser can play.
func (t *Tool) Transcode(ctx context.Context, src, dst string, info Info) error {
	args := []string{
		"-nostdin", "-y",
		"-v", "error",
		"-i", src,
		"-map_metadata", "0",
	}

	// Only scale when there is something to scale down. ffmpeg's scaler has no
	// "shrink only" mode, so the decision is made here from the probed size
	// rather than expressed in the filter.
	if w, h := info.DisplaySize(); w > PlaybackMaxEdge || h > PlaybackMaxEdge {
		args = append(args, "-vf", fmt.Sprintf(
			"scale=w=%d:h=%d:force_original_aspect_ratio=decrease:force_divisible_by=2:flags=lanczos",
			PlaybackMaxEdge, PlaybackMaxEdge))
	}

	args = append(args,
		"-c:v", t.encoder(),
		"-crf", strconv.Itoa(t.crf()),
		"-preset", "medium",
		// Baseline chroma layout. Phone video is often 10-bit HDR, which most
		// browsers refuse outright.
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "128k",
		// Moves the index to the front so playback starts before the whole file
		// has arrived. Without it a 200MB clip buffers completely first.
		"-movflags", "+faststart",
		dst,
	)

	cmd := exec.CommandContext(ctx, t.ffmpeg(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	complaint := strings.TrimSpace(stderr.String())

	// Same reasoning as posterAt, and a worse outcome if skipped: an ffmpeg that
	// exits 0 having encoded nothing leaves a rendition that is marked ready and
	// plays nothing, which is a failure disguised as a success.
	if err := t.checkPlayable(ctx, dst); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("transcode %s: %w", src, ctx.Err())
		}
		return fmt.Errorf("transcode %s: %w (ffmpeg %s; stderr: %s)", src, err, exitStatus(runErr), orNone(complaint))
	}
	if runErr != nil {
		return fmt.Errorf("transcode %s: %w: %s", src, runErr, complaint)
	}
	return nil
}

// checkPlayable asks ffprobe what a browser would find in the transcode. A
// shape check is not enough here: a container with a correct header and a track
// carrying no samples is exactly what the failure looks like.
func (t *Tool) checkPlayable(ctx context.Context, path string) error {
	stat, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotPlayable, err)
	}
	if stat.Size() == 0 {
		return fmt.Errorf("%w: the file is empty", ErrNotPlayable)
	}

	info, err := t.Probe(ctx, path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotPlayable, err)
	}
	if info.Width == 0 || info.Height == 0 || info.DurationSeconds <= 0 {
		return fmt.Errorf("%w: %d bytes describing a %dx%d track of %.3fs",
			ErrNotPlayable, stat.Size(), info.Width, info.Height, info.DurationSeconds)
	}
	return nil
}

func parseSeconds(v string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// firstRotation reads whichever of ffprobe's rotation representations is
// present. It arrives as a JSON number in some versions and a string in others.
func firstRotation(values ...any) int {
	for _, v := range values {
		switch n := v.(type) {
		case float64:
			return int(n)
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
				return int(f)
			}
		}
	}
	return 0
}

func (t *Tool) ffmpeg() string {
	if t.FFmpeg == "" {
		return "ffmpeg"
	}
	return t.FFmpeg
}

func (t *Tool) ffprobe() string {
	if t.FFprobe == "" {
		return "ffprobe"
	}
	return t.FFprobe
}

func (t *Tool) encoder() string {
	if t.Encoder == "" {
		return "libx264"
	}
	return t.Encoder
}

func (t *Tool) crf() int {
	if t.CRF <= 0 {
		return 22
	}
	return t.CRF
}
