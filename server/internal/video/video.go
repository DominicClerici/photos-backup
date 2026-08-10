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
	"fmt"
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
	seek := 1.0
	if info.DurationSeconds > 0 && info.DurationSeconds < 2 {
		seek = info.DurationSeconds / 2
	}

	cmd := exec.CommandContext(ctx, t.ffmpeg(),
		"-nostdin", "-y",
		"-v", "error",
		// Before -i, so ffmpeg seeks by keyframe instead of decoding forward to
		// the timestamp. On a 3GB clip that is the difference between instant
		// and minutes.
		"-ss", strconv.FormatFloat(seek, 'f', 3, 64),
		"-i", src,
		"-frames:v", "1",
		"-q:v", "2",
		dst,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract poster frame from %s: %w: %s", src, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
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

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("transcode %s: %w: %s", src, err, bytes.TrimSpace(stderr.Bytes()))
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
