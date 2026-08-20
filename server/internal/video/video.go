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
	"slices"
	"strconv"
	"strings"
	"sync"
)

// PlaybackMaxEdge bounds the transcode. 1080p is the point past which a home
// gallery gains nothing visible and starts costing real disk and encode time;
// the original is always one click away for anything more.
const PlaybackMaxEdge = 1920

// LiveThumbTarget is one square size of a Live Photo's motion and the file it
// should be written to. The sizes match the still thumbnails exactly, because
// the two are shown in the same cell and anything else makes the picture jump
// as one replaces the other.
type LiveThumbTarget struct {
	Size int
	Path string
}

// LiveThumbMaxSeconds bounds it in time. A Live Photo is about three seconds,
// and this is here for the day something else is labelled as one: a
// mis-declared feature film should cost a truncated hover animation, not a
// metadata worker parked for an hour.
const LiveThumbMaxSeconds = 6

// LiveThumbCRF is the motion thumbnail's quality target on x264's scale, which
// videoArgs translates for a hardware encoder. Looser than the playback
// rendition on purpose.
const LiveThumbCRF = 30

type Tool struct {
	FFmpeg  string
	FFprobe string
	// Encoder is the ffmpeg video encoder. libx264 works everywhere; the
	// archive machine has an NVIDIA card and can try h264_nvenc without a code
	// change.
	Encoder string
	// CRF is the quality target, named for x264's scale because that is the
	// portable default. NVENC is given the same picture through its own
	// vocabulary rather than being left to its stock rate control — see
	// videoArgs, where ignoring this was a silent and expensive mistake.
	CRF int
	// LiveConcurrency caps simultaneous on-demand Live Photo renditions. Same
	// reasoning as derive.Converter's preview cap, and a lower number, because
	// an ffmpeg costs considerably more than an ImageMagick.
	LiveConcurrency int

	once sync.Once
	live chan struct{}
}

func New() *Tool {
	return &Tool{FFmpeg: "ffmpeg", FFprobe: "ffprobe", Encoder: "libx264", CRF: 22, LiveConcurrency: 2}
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
//
// overlay, when set, names an image to burn into every frame: a Snapchat
// memory's caption layer, which arrives as its own file and has to be drawn
// somewhere. A still can be composed at the moment it is served, because
// ImageMagick is already in the request path. A video cannot — no browser will
// composite a transparent PNG onto a playing <video> — so for these the layer
// goes into the pixels. The original is untouched in the blob store either way,
// and the worker writes a second rendition without this one so the viewer can
// still be shown the photograph underneath.
func (t *Tool) Transcode(ctx context.Context, src, dst string, info Info, overlay string) error {
	args := []string{"-nostdin", "-y", "-v", "error"}
	args = append(args, t.decodeArgs()...)
	args = append(args, "-i", src)
	if overlay != "" {
		// Read once, not looped. The filter holds the single frame for the
		// length of the clip on its own — see overlayGraph, where that is now
		// said out loud rather than left to a default.
		//
		// `-loop 1` is the obvious way to write this and it is a trap. It makes
		// the layer an input with no end, and ffmpeg then bounds the encode only
		// by whatever *other* finite output stream it has: on a clip with an
		// audio track it stops at the right moment, and on a silent one it never
		// stops at all. Measured, not feared — a 7.6-second silent memory ran
		// for thirteen minutes, held sixteen cores, and wrote 300MB of a video
		// that would have filled the disk. `-shortest` does not save it, which
		// is the part worth remembering.
		args = append(args, "-i", overlay)
	}
	args = append(args, "-map_metadata", "0")

	// Only scale when there is something to scale down. ffmpeg's scaler has no
	// "shrink only" mode, so the decision is made here from the probed size
	// rather than expressed in the filter.
	switch w, h := info.DisplaySize(); {
	case overlay != "":
		out, err := overlayGraph(info)
		if err != nil {
			return fmt.Errorf("transcode %s: %w", src, err)
		}
		args = append(args,
			"-filter_complex", out,
			"-map", "[v]",
			// Explicit, and optional, because -filter_complex turns ffmpeg's
			// automatic stream selection off: without it a memory with sound
			// comes out silent and nothing says so. The `?` is what lets a
			// silent one through, and half of these are silent.
			"-map", "0:a?")
	case w > PlaybackMaxEdge || h > PlaybackMaxEdge:
		args = append(args, "-vf", fmt.Sprintf(
			"scale=w=%d:h=%d:force_original_aspect_ratio=decrease:force_divisible_by=2:flags=lanczos",
			PlaybackMaxEdge, PlaybackMaxEdge))
	}

	args = append(args, t.videoArgs(t.encoderFor(outputMinEdge(info, PlaybackMaxEdge)), t.crf(), speedBalanced)...)
	args = append(args,
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

// overlayGraph is the filter that composes a memory: both inputs scaled to the
// frame the encoder will write, then one laid over the other at the origin.
//
// Both are scaled explicitly to the same numbers rather than one being fitted to
// the other, because the two files are the same moment recorded at different
// resolutions — Snapchat's layer is the phone's screen and the video is what the
// camera captured to fill it, and across this archive they never once agree. The
// aspect ratios do agree, to within a rounding error, so stretching the layer to
// the frame lands the caption where the person put it.
//
// eof_action=repeat is what makes one still cover a clip of any length: the
// layer's single frame is held for every frame of the video after it. It is
// ffmpeg's default and it is written out anyway, because the alternative way to
// get this — looping the input — turns the layer into a stream with no end and
// hangs the encode outright on a video with no audio track. See Transcode.
//
// A video whose size ffprobe could not report is refused rather than guessed at.
// The alternative is a burn at the wrong scale, which is a rendition that looks
// deliberate and is wrong in a way nothing downstream can detect.
func overlayGraph(info Info) (string, error) {
	w, h := info.DisplaySize()
	if w <= 0 || h <= 0 {
		return "", errors.New("cannot burn in an overlay without the video's dimensions")
	}
	if long := max(w, h); long > PlaybackMaxEdge {
		w = w * PlaybackMaxEdge / long
		h = h * PlaybackMaxEdge / long
	}
	// yuv420p halves both chroma planes, so an odd edge has no representation.
	w, h = max(w&^1, 2), max(h&^1, 2)

	return fmt.Sprintf(
		"[0:v]scale=w=%d:h=%d:flags=lanczos[base];"+
			"[1:v]scale=w=%d:h=%d:flags=lanczos[layer];"+
			"[base][layer]overlay=0:0:eof_action=repeat:format=auto[v]",
		w, h, w, h), nil
}

// LiveThumbs writes a Live Photo's motion thumbnail at every size asked for:
// square, silent, and small enough that a grid can pull one down the instant a
// pointer lands on a tile.
//
// Square-cropped rather than fitted, matching the still thumbnail's own crop,
// because the two are shown in the same cell and the video replaces the image
// in place. A fitted video would letterbox against the tile and the picture
// would visibly jump on hover.
//
// Silent because nothing can play audio here: this fires on hover, without a
// user gesture, and every browser blocks unmuted autoplay without one. Dropping
// the track rather than muting the element saves the bytes too.
//
// One ffmpeg for all of them, like the stills: the clip is decoded once, cropped
// once at the largest size, and the smaller ones are scaled off that same
// filter graph. Three separate runs would decode the same three seconds three
// times over, which during a backfill is the whole cost of the feature.
func (t *Tool) LiveThumbs(ctx context.Context, src string, targets []LiveThumbTarget, info Info) error {
	if len(targets) == 0 {
		return nil
	}
	ordered := slices.SortedFunc(slices.Values(targets), func(a, b LiveThumbTarget) int {
		return b.Size - a.Size
	})

	// increase-then-crop is ImageMagick's `-resize ^` and `-extent` in ffmpeg's
	// vocabulary: fill the box, then cut the overflow away. Everything after the
	// split is already square, so the smaller sizes are a plain scale.
	largest := ordered[0].Size
	graph := fmt.Sprintf(
		"[0:v]scale=w=%d:h=%d:force_original_aspect_ratio=increase:flags=lanczos,crop=%d:%d,split=%d[o0]",
		largest, largest, largest, largest, len(ordered))
	for i := 1; i < len(ordered); i++ {
		graph += fmt.Sprintf("[s%d]", i)
	}
	for i := 1; i < len(ordered); i++ {
		graph += fmt.Sprintf(";[s%d]scale=w=%d:h=%d:flags=lanczos[o%d]", i, ordered[i].Size, ordered[i].Size, i)
	}

	args := []string{"-nostdin", "-y", "-v", "error"}
	args = append(args, t.decodeArgs()...)
	args = append(args,
		// Before -i, so the limit applies to what is read rather than having to
		// be repeated on every output.
		"-t", strconv.Itoa(LiveThumbMaxSeconds),
		"-i", src,
		"-filter_complex", graph,
	)
	dsts := make([]string, len(ordered))
	for i, target := range ordered {
		dsts[i] = target.Path
		args = append(args,
			// Only the filter's video comes through, so the paired clip's audio
			// track is dropped by never being mapped.
			"-map", fmt.Sprintf("[o%d]", i))
		// Looser than the playback rendition. At thumbnail size the difference
		// is invisible and the file is a third of the size, which is what a
		// grid firing these off on hover needs it to be.
		//
		// Per output, because the smallest sizes are below what a hardware
		// encoder will take and one refusal fails the whole run — the outputs
		// share an ffmpeg, so they cannot share a fate. See nvencMinDimension.
		args = append(args, t.videoArgs(t.encoderFor(target.Size), LiveThumbCRF, speedFast)...)
		args = append(args,
			"-pix_fmt", "yuv420p",
			"-movflags", "+faststart",
			target.Path,
		)
	}
	return t.encode(ctx, args, src, dsts, "live thumbnail")
}

// LivePreview writes the rendition the viewer plays on press-and-hold: 1080p,
// with its audio, from the same three seconds.
//
// Nothing calls this from the worker. It is rendered per request and never
// stored, exactly like a photo's 2048px preview and for the same reason — one
// viewer plays one of these at a time, the bytes are a pure function of a
// digest, and `Cache-Control: immutable` does what a file on disk would.
//
// The audio is kept here and only here. A press-and-hold is a user gesture, so
// a browser will let it make noise, which is what makes it feel like the Live
// Photo on the phone rather than a silent loop.
func (t *Tool) LivePreview(ctx context.Context, src, dst string, info Info) error {
	if err := t.acquire(ctx); err != nil {
		return err
	}
	defer t.release()

	args := []string{"-nostdin", "-y", "-v", "error"}
	args = append(args, t.decodeArgs()...)
	args = append(args, "-i", src)
	if w, h := info.DisplaySize(); w > PlaybackMaxEdge || h > PlaybackMaxEdge {
		args = append(args, "-vf", fmt.Sprintf(
			"scale=w=%d:h=%d:force_original_aspect_ratio=decrease:force_divisible_by=2:flags=lanczos",
			PlaybackMaxEdge, PlaybackMaxEdge))
	}
	// The fast end of the curve, where the stored rendition takes the balanced
	// one. This is being waited on by somebody who has already opened the
	// photo, and it is three seconds long: latency is worth more here than
	// bitrate.
	args = append(args, t.videoArgs(t.encoderFor(outputMinEdge(info, PlaybackMaxEdge)), t.crf(), speedFast)...)
	args = append(args,
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
		dst,
	)
	return t.encode(ctx, args, src, []string{dst}, "live preview")
}

// SampleFrames walks a clip and returns count square grey planes taken at even
// fractions of its running time — the video's answer to derive.Sample, in
// exactly the format internal/imagehash reads, so a frame is hashed by the same
// code that hashes a photograph.
//
// Sampled by *time* rather than by frame number, which is the whole reason this
// survives a re-encode. A clip at 15fps and the same clip at 30fps have nothing
// in common at frame index seven and the same picture 35% of the way through,
// so the fps filter — which resamples a stream to a fixed rate by choosing the
// frame nearest each instant — puts the two sequences side by side. Handing
// ffmpeg `select=eq(n\,7)` instead would compare the seventh frame of one
// recording with a moment half a second earlier in the other, and no threshold
// makes that work.
//
// One ffmpeg for the whole clip rather than one seek per frame. Twenty accurate
// seeks into a 60-second video is twenty decodes from the preceding keyframe;
// this is one pass, and across seven thousand videos that is the difference
// between an afternoon and a coffee.
//
// The caption layer is deliberately *not* burned in here, where derive.Sample
// does burn it in for a still. The asymmetry is real and it is cheap: for a
// still the composite already exists in the pipeline and costs one more argv
// word, while for a video it is a filter graph and a decode of a second input.
// What it would buy is nothing — two copies of one memory are sampled the same
// way with or without the layer, so they match either way. What it costs is the
// unlikely case of two different captions over the same footage, which arrives
// at a review page for somebody to look at rather than anywhere destructive.
func (t *Tool) SampleFrames(ctx context.Context, src string, count, edge int, info Info) ([][]byte, error) {
	switch {
	case count <= 0:
		return nil, fmt.Errorf("sample %s: %d is not a number of frames", src, count)
	case edge <= 0:
		return nil, fmt.Errorf("sample %s: %d is not an edge length", src, edge)
	case info.DurationSeconds <= 0:
		// Refused rather than guessed at. Without a duration there is no way to
		// space the samples, and a fallback that sampled the first count frames
		// would produce a signature that looks like every other signature taken
		// from the opening second of a video.
		return nil, fmt.Errorf("sample %s: the clip has no duration to sample across", src)
	}

	if err := t.acquire(ctx); err != nil {
		return nil, err
	}
	defer t.release()

	args := []string{"-nostdin", "-v", "error"}
	args = append(args, t.decodeArgs()...)
	args = append(args,
		"-i", src,
		// Squashed to a square, matching derive.Sample: the two feed the same
		// hash and have to distort a picture the same way.
		"-vf", fmt.Sprintf("fps=%d/%f,scale=%d:%d:flags=lanczos,format=gray",
			count, info.DurationSeconds, edge, edge),
		"-frames:v", fmt.Sprint(count),
		"-f", "rawvideo",
		"-",
	)

	cmd := exec.CommandContext(ctx, t.ffmpeg(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sample %s: %w: %s", src, err, strings.TrimSpace(stderr.String()))
	}

	// Short is allowed and long is not. ffmpeg can hand back one frame fewer
	// than asked for when the last sampling instant lands past the final frame,
	// and that costs nothing: the frames it did return are at the same instants
	// they would have been at anyway, so two clips still line up index for
	// index. Anything else means the filter chain did not do what this thinks
	// it did, and a signature built on that is worse than none.
	raw := stdout.Bytes()
	plane := edge * edge
	got := len(raw) / plane
	switch {
	case len(raw)%plane != 0:
		return nil, fmt.Errorf("sample %s: %d bytes is not a whole number of %dx%d planes",
			src, len(raw), edge, edge)
	case got == 0:
		return nil, fmt.Errorf("sample %s: ffmpeg wrote no frames (stderr: %s)",
			src, orNone(strings.TrimSpace(stderr.String())))
	case got > count:
		return nil, fmt.Errorf("sample %s: asked for %d frames and got %d", src, count, got)
	}

	frames := make([][]byte, got)
	for i := range frames {
		frames[i] = raw[i*plane : (i+1)*plane]
	}
	return frames, nil
}

// encode runs one ffmpeg and insists that something playable came out of every
// output it was given, for the same reason Transcode does: an ffmpeg that exits
// 0 having encoded nothing is a failure disguised as a success, and it surfaces
// later as a player that shows a black rectangle.
func (t *Tool) encode(ctx context.Context, args []string, src string, dsts []string, what string) error {
	cmd := exec.CommandContext(ctx, t.ffmpeg(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	complaint := strings.TrimSpace(stderr.String())

	for _, dst := range dsts {
		if err := t.checkPlayable(ctx, dst); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("render %s of %s: %w", what, src, ctx.Err())
			}
			return fmt.Errorf("render %s of %s: %w (ffmpeg %s; stderr: %s)",
				what, src, err, exitStatus(runErr), orNone(complaint))
		}
	}
	if runErr != nil {
		return fmt.Errorf("render %s of %s: %w: %s", what, src, runErr, complaint)
	}
	return nil
}

func (t *Tool) acquire(ctx context.Context) error {
	t.once.Do(func() {
		n := t.LiveConcurrency
		if n <= 0 {
			n = 2
		}
		t.live = make(chan struct{}, n)
	})

	select {
	case t.live <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *Tool) release() { <-t.live }

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

// speed places an encode on the quality-for-time curve. The two encoder
// families spell the same intent differently and their names do not translate
// — x264's "veryfast" is a visibly worse picture than NVENC's p1 — so the
// intent is named here and each family maps it to its own vocabulary.
type speed int

const (
	// speedBalanced is for a rendition written once and watched later, where a
	// few extra seconds of encode buy bitrate back for the life of the archive.
	speedBalanced speed = iota
	// speedFast is for the renditions somebody is waiting on.
	speedFast
)

// nvencCQ converts the x264 CRF the call sites ask for into the constant
// quality NVENC wants. Both scales are QP-derived but they are not aligned,
// and NVENC's runs leaner for the same number: measured on this archive's own
// footage, x264 -crf 22 sits at NVENC -cq 28 (equal VMAF, slightly smaller
// file) and the 256px Live Photo thumbnail's -crf 30 sits at -cq 36 (equal
// bytes). Six is an empirical fit at the two resolutions that are actually
// encoded here, not a law — anything moved far from them wants re-measuring.
const nvencCQOffset = 6

// nvencMinDimension is the smallest edge NVENC's H.264 encoder will accept.
// Below it InitializeEncoder refuses the whole session — "Frame Dimension less
// than the minimum supported value" — and nothing is written.
//
// It is here because that refusal cost this archive its Live Photos. LiveThumbs
// writes every stored size from a single ffmpeg, so the 96px motion thumbnail
// took the 256 and the 512 down with it: setting VIDEO_ENCODER=h264_nvenc
// marked every paired video in the library live_state = 'failed', and a still
// whose motion failed draws as an ordinary photo. The stills still deduped, so
// the only symptom was that nothing anywhere came to life.
//
// 145 is measured on this machine's card rather than read from a spec: 144
// square is refused and 145 square encodes.
const nvencMinDimension = 145

// softwareEncoder stands in for the configured one on frames it cannot take.
// libx264 has no such floor, and the outputs that need it are by definition
// tiny — a three-second 96px square is cheaper in software than the fallback
// logic that avoids it.
const softwareEncoder = "libx264"

func isNVENC(encoder string) bool { return strings.Contains(encoder, "nvenc") }

// encoderFor picks the encoder for an output whose shortest edge is minEdge,
// which is the configured one except where hardware cannot encode a frame that
// small. minEdge of 0 means the size is not known — the configured encoder
// stands, because a guess in either direction is worse than the failure it
// would be guessing about.
func (t *Tool) encoderFor(minEdge int) string {
	if enc := t.encoder(); !isNVENC(enc) || minEdge <= 0 || minEdge >= nvencMinDimension {
		return enc
	}
	return softwareEncoder
}

// outputMinEdge is the shorter edge of what the scaler will actually write,
// which is what decides whether the hardware encoder can take it. The scale
// filter only ever shrinks, so an input already inside the box passes through
// at its own size.
func outputMinEdge(info Info, maxEdge int) int {
	w, h := info.DisplaySize()
	if w <= 0 || h <= 0 {
		return 0
	}
	if long := max(w, h); long > maxEdge {
		w = w * maxEdge / long
		h = h * maxEdge / long
	}
	return min(w, h)
}

// videoArgs returns the encoder, its rate control and its preset. The encoder
// is passed in rather than read off the Tool because it is not the same for
// every output: see encoderFor.
//
// These cannot be written once and shared, which is the whole reason this
// function exists. x264 reads -crf and named presets; NVENC reads -cq and pN
// presets, accepts "medium" as a deprecated alias, and ignores -crf in
// silence. So the obvious VIDEO_ENCODER=h264_nvenc encodes perfectly happily
// and quietly abandons the quality target for NVENC's stock 2Mbps VBR: on a
// sample clip that measured VMAF 76.0 against 91.6 for the libx264 it
// replaced, at a third of the bitrate. Nothing fails; the archive just fills
// up with worse video than it was asked for.
func (t *Tool) videoArgs(encoder string, crf int, s speed) []string {
	if !isNVENC(encoder) {
		preset := "medium"
		if s == speedFast {
			preset = "veryfast"
		}
		return []string{"-c:v", encoder, "-crf", strconv.Itoa(crf), "-preset", preset}
	}

	// p4 rather than p5/p6: on a saturated queue NVENC is the bottleneck, and
	// p4 measured within 0.05 VMAF and 0.4% of p5's size for two thirds of its
	// encode time. p1 for the renditions that are being waited on, where it is
	// still better than the x264 preset it stands in for.
	preset := "p4"
	if s == speedFast {
		preset = "p1"
	}
	// -b:v 0 is what makes -cq a pure quality target; without it NVENC treats
	// the flags as a bitrate cap and the quality number stops meaning much.
	return []string{
		"-c:v", encoder,
		"-preset", preset,
		"-tune", "hq",
		"-rc", "vbr",
		"-cq", strconv.Itoa(crf + nvencCQOffset),
		"-b:v", "0",
	}
}

// decodeArgs offers the decode to the GPU when the encode is already there.
// They go before -i, and they are a plain "-hwaccel cuda" with no output
// format: ffmpeg then decodes on NVDEC and hands ordinary frames back to the
// filter chain, which halved decode CPU on a 1080p60 HEVC clip (15.2s to 7.6s)
// and costs nothing when it cannot be done — an input NVDEC does not handle
// falls back to software decode on its own.
//
// Emphatically not "-hwaccel_output_format cuda", which is the faster-looking
// version of this and is wrong. Keeping frames in GPU memory puts them out of
// reach of ffmpeg's autorotate, and this build has no NPP or other GPU
// transpose to put in its place, so ffmpeg silently skips the rotation *and*
// drops the display matrix: every portrait iPhone video comes out 1920x1080
// landscape with nothing left to tell a player otherwise. Verified, not
// feared.
func (t *Tool) decodeArgs() []string {
	if !isNVENC(t.encoder()) {
		return nil
	}
	return []string{"-hwaccel", "cuda"}
}
