package video

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Part is one input to Join: a clip, and the caption layer that belongs over it.
//
// Overlay is per part rather than per join because it has to be. Snapchat caps
// a memory at ten seconds and a longer recording is exported as several of
// them, each one separately drawn on — so the six files that make up one minute
// can carry six different captions, appearing and vanishing at the ten-second
// boundaries exactly as they did when somebody watched them in the app.
type Part struct {
	Path string
	// Overlay is a transparent image to burn into every frame of this part, or
	// empty. Setting it forces the re-encode path: pixels cannot be changed by
	// a stream copy, which is the entire definition of a stream copy.
	Overlay string
}

// JoinResult is what a join did, which is worth knowing even when it worked.
type JoinResult struct {
	// Copied is true when the parts went into the output untouched — no decode,
	// no encode, the original frames bit for bit. False means they had to be
	// normalised first, and the output is a generation further from the camera
	// than its inputs are.
	Copied bool
	// DurationSeconds is the joined running time, read back off the file rather
	// than added up from the inputs.
	DurationSeconds float64
	// Why is the reason the copy path was not taken, empty when it was. Written
	// into the merged asset's sidecar, because "this file was re-encoded" is a
	// fact about an archived original that somebody will want the reason for
	// long after the parts have been purged.
	Why string
}

// joinDurationSlack is how far the joined file may fall from the sum of its
// parts before the join is treated as having failed.
//
// A tenth of a second across a minute of video. Concatenation does not
// resample, so the arithmetic should be exact; what this is really guarding
// against is ffmpeg having silently dropped a part — which costs ten whole
// seconds and could not possibly hide under this.
const joinDurationSlack = 0.1

// Join concatenates parts into one file at dst, in the order given.
//
// It prefers to copy. Two consecutive segments of one Snapchat recording came
// out of the same encoder on the same phone a tenth of a second apart, so they
// agree about everything a container cares about, and joining them is a matter
// of writing their packets into one file in order — no decode, no re-encode,
// and the output holds the camera's own frames rather than a second-generation
// copy of them. Across this archive that is the path all but a handful of
// groups take.
//
// When they do not agree — a resolution that changed mid-recording, a part with
// no audio, a caption layer that has to go into the pixels — every part is
// first normalised to a common format and the join copies those instead. It is
// the same shape of decision derive makes about composing an overlay: do the
// cheap exact thing when the inputs allow it, and be explicit about the
// occasion when they do not.
func (t *Tool) Join(ctx context.Context, parts []Part, dst string) (JoinResult, error) {
	if len(parts) < 2 {
		return JoinResult{}, fmt.Errorf("join: %d parts is not something to join", len(parts))
	}

	profiles := make([]streamProfile, len(parts))
	var expected float64
	for i, part := range parts {
		p, err := t.profile(ctx, part.Path)
		if err != nil {
			return JoinResult{}, err
		}
		profiles[i] = p
		expected += p.Duration
	}

	work, err := os.MkdirTemp(filepath.Dir(dst), "join-*")
	if err != nil {
		return JoinResult{}, fmt.Errorf("join: create work directory: %w", err)
	}
	defer os.RemoveAll(work)

	sources := make([]string, len(parts))
	why := uniform(parts, profiles)
	if why == "" {
		for i, part := range parts {
			sources[i] = part.Path
		}
	} else {
		width, height := commonFrame(profiles)
		for i, part := range parts {
			out := filepath.Join(work, fmt.Sprintf("part-%03d.mp4", i))
			if err := t.normalize(ctx, part, profiles[i], width, height, out); err != nil {
				return JoinResult{}, err
			}
			sources[i] = out
		}
	}

	if err := t.concat(ctx, sources, filepath.Join(work, "list.txt"), dst); err != nil {
		return JoinResult{}, err
	}

	// Read the result back rather than trusting the arithmetic. A join that
	// quietly dropped a part is the one failure here that produces a playable
	// file, and a playable file is what every other check in this package
	// accepts as success.
	joined, err := t.Probe(ctx, dst)
	if err != nil {
		return JoinResult{}, fmt.Errorf("join: %w", err)
	}
	if math.Abs(joined.DurationSeconds-expected) > joinDurationSlack {
		return JoinResult{}, fmt.Errorf(
			"join: %d parts totalling %.3fs came out as %.3fs; refusing to archive that",
			len(parts), expected, joined.DurationSeconds)
	}

	return JoinResult{Copied: why == "", DurationSeconds: joined.DurationSeconds, Why: why}, nil
}

// concat writes the demuxer's list file and runs the copy.
//
// The concat *demuxer* rather than the concat *protocol* or the concat filter:
// the protocol only works on formats that can be joined by butting the bytes
// together, which MP4 cannot, and the filter decodes. This one reads several
// containers and writes one, moving packets and rewriting timestamps.
func (t *Tool) concat(ctx context.Context, sources []string, listPath, dst string) error {
	var list bytes.Buffer
	for _, src := range sources {
		abs, err := filepath.Abs(src)
		if err != nil {
			return fmt.Errorf("join: resolve %s: %w", src, err)
		}
		// Single quotes are the demuxer's own escaping, and a quote inside a
		// path is written by doubling out of them. Blob paths are hex digests
		// and cannot contain one; the work directory is ours. This is here so
		// that stays true of a caller that joins something else.
		fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(abs, "'", `'\''`))
	}
	if err := os.WriteFile(listPath, list.Bytes(), 0o644); err != nil {
		return fmt.Errorf("join: write concat list: %w", err)
	}

	args := []string{
		"-nostdin", "-y", "-v", "error",
		// -safe 0 permits absolute paths in the list. The list is written two
		// lines above from paths this process resolved, so there is no
		// untrusted name anywhere in it.
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy",
		// Timestamps restart at zero in every part; the demuxer offsets them,
		// and this keeps a rounding error at a boundary from being emitted as a
		// packet that goes backwards.
		"-fflags", "+genpts",
		"-movflags", "+faststart",
		dst,
	}
	return t.encode(ctx, args, listPath, []string{dst}, "joined video")
}

// uniform reports why the parts cannot simply be copied, or "" when they can.
//
// A sentence rather than a boolean because it is kept: it goes into the merged
// asset's sidecar, and "the third part is 1080x1920 where the first is 540x1110"
// is what somebody needs a year later when they wonder why one archived
// original in ten thousand went through an encoder.
func uniform(parts []Part, profiles []streamProfile) string {
	for i, part := range parts {
		if part.Overlay != "" {
			return fmt.Sprintf("part %d carries a caption layer, which has to be drawn into the pixels", i+1)
		}
	}
	first := profiles[0]
	for i, p := range profiles[1:] {
		if diff := first.differs(p); diff != "" {
			return fmt.Sprintf("part %d %s", i+2, diff)
		}
	}
	return ""
}

// streamProfile is everything about a file that has to match across a stream
// copy. It is deliberately not video.Info: that one describes a picture, and
// this one describes a container's willingness to be concatenated with another.
type streamProfile struct {
	Duration float64

	VideoCodec string
	Width      int
	Height     int
	Rotation   int
	PixFmt     string
	// Profile and Level are the encoder's own settings. Two H.264 streams at
	// different profiles have incompatible sequence headers, and a player
	// handed the join gets one of them and decodes the other with it.
	Profile string
	Level   int

	HasAudio      bool
	AudioCodec    string
	SampleRate    int
	Channels      int
	ChannelLayout string
}

// differs names the first disagreement, in words, or "".
func (a streamProfile) differs(b streamProfile) string {
	switch {
	case a.VideoCodec != b.VideoCodec:
		return fmt.Sprintf("is %s where the first is %s", orNone(b.VideoCodec), orNone(a.VideoCodec))
	case a.Width != b.Width || a.Height != b.Height:
		return fmt.Sprintf("is %dx%d where the first is %dx%d", b.Width, b.Height, a.Width, a.Height)
	case a.Rotation != b.Rotation:
		return fmt.Sprintf("is rotated %d° where the first is rotated %d°", b.Rotation, a.Rotation)
	case a.PixFmt != b.PixFmt:
		return fmt.Sprintf("is %s where the first is %s", orNone(b.PixFmt), orNone(a.PixFmt))
	case a.Profile != b.Profile || a.Level != b.Level:
		return fmt.Sprintf("is %s@%d where the first is %s@%d",
			orNone(b.Profile), b.Level, orNone(a.Profile), a.Level)
	case a.HasAudio != b.HasAudio:
		if b.HasAudio {
			return "has sound where the first has none"
		}
		return "is silent where the first has sound"
	case a.AudioCodec != b.AudioCodec:
		return fmt.Sprintf("has %s audio where the first has %s", orNone(b.AudioCodec), orNone(a.AudioCodec))
	case a.SampleRate != b.SampleRate:
		return fmt.Sprintf("is %dHz where the first is %dHz", b.SampleRate, a.SampleRate)
	case a.Channels != b.Channels || a.ChannelLayout != b.ChannelLayout:
		return fmt.Sprintf("has %d audio channels where the first has %d", b.Channels, a.Channels)
	}
	return ""
}

type profileOutput struct {
	Streams []struct {
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		PixFmt        string `json:"pix_fmt"`
		Profile       string `json:"profile"`
		Level         int    `json:"level"`
		SampleRate    string `json:"sample_rate"`
		Channels      int    `json:"channels"`
		ChannelLayout string `json:"channel_layout"`
		Duration      string `json:"duration"`
		Rotation      any    `json:"rotation"`
		SideDataLst   []struct {
			Rotation any `json:"rotation"`
		} `json:"side_data_list"`
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func (t *Tool) profile(ctx context.Context, src string) (streamProfile, error) {
	cmd := exec.CommandContext(ctx, t.ffprobe(),
		"-v", "error",
		"-show_entries",
		"stream=codec_type,codec_name,width,height,pix_fmt,profile,level,sample_rate,channels,channel_layout,duration"+
			":stream_side_data=rotation:stream_tags=rotate:format=duration",
		"-of", "json",
		src,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return streamProfile{}, fmt.Errorf("ffprobe %s: %w: %s", src, err, bytes.TrimSpace(stderr.Bytes()))
	}

	var out profileOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return streamProfile{}, fmt.Errorf("parse ffprobe output for %s: %w", src, err)
	}

	var p streamProfile
	seenVideo := false
	for _, s := range out.Streams {
		switch s.CodecType {
		case "video":
			if seenVideo {
				continue
			}
			seenVideo = true
			p.VideoCodec = s.CodecName
			p.Width, p.Height = s.Width, s.Height
			p.PixFmt, p.Profile, p.Level = s.PixFmt, s.Profile, s.Level
			p.Duration = parseSeconds(s.Duration)
			p.Rotation = firstRotation(s.Rotation, s.Tags["rotate"])
			for _, sd := range s.SideDataLst {
				if p.Rotation != 0 {
					break
				}
				p.Rotation = firstRotation(sd.Rotation)
			}
		case "audio":
			if p.HasAudio {
				continue
			}
			p.HasAudio = true
			p.AudioCodec = s.CodecName
			p.SampleRate, _ = strconv.Atoi(s.SampleRate)
			p.Channels, p.ChannelLayout = s.Channels, s.ChannelLayout
		}
	}
	if !seenVideo {
		return streamProfile{}, fmt.Errorf("ffprobe %s: no video stream", src)
	}
	if p.Duration == 0 {
		p.Duration = parseSeconds(out.Format.Duration)
	}
	if p.Duration == 0 {
		return streamProfile{}, fmt.Errorf("ffprobe %s: no duration; refusing to join a clip of unknown length", src)
	}
	return p, nil
}

// commonFrame is the picture every part is normalised to when they cannot be
// copied: the largest displayed frame among them, rounded to even edges.
//
// The largest rather than the first or the smallest, because normalising is
// already the lossy path and there is no reason to make it lossier. Upscaling
// the small parts costs bytes and invents no detail; downscaling the large ones
// would throw away detail this archive has.
func commonFrame(profiles []streamProfile) (width, height int) {
	for _, p := range profiles {
		w, h := Info{Width: p.Width, Height: p.Height, Rotation: p.Rotation}.DisplaySize()
		if w*h > width*height {
			width, height = w, h
		}
	}
	// yuv420p halves both chroma planes, so an odd edge has no representation.
	return max(width&^1, 2), max(height&^1, 2)
}

// normalizedSampleRate and normalizedChannels are what a re-encoded part's
// audio is resampled to. Stereo at 44.1kHz is what the phone footage in this
// archive already is, so in practice the parts that had sound keep it
// untouched in every respect but the encode, and the silent ones are given a
// track that matches.
const (
	normalizedSampleRate = 44100
	normalizedChannels   = "stereo"
)

// normalize renders one part into the common format, burning in its caption
// layer if it has one.
//
// Every part gets a sound track whether or not it arrived with one. A
// concatenation of clips where some have audio and some do not is the one case
// the demuxer cannot rescue afterwards: players stop at the first part with a
// missing stream, or play the whole join silently. Generating silence for a
// part that had none costs a few kilobytes and makes the output a file rather
// than a puzzle.
func (t *Tool) normalize(ctx context.Context, part Part, p streamProfile, targetW, targetH int, dst string) error {
	args := []string{"-nostdin", "-y", "-v", "error"}
	args = append(args, t.decodeArgs()...)
	args = append(args, "-i", part.Path)

	overlayIndex := -1
	silenceIndex := -1
	next := 1
	if part.Overlay != "" {
		// Read once, not looped — see the note in Transcode, where looping the
		// layer was measured running for thirteen minutes on a silent clip.
		args = append(args, "-i", part.Overlay)
		overlayIndex = next
		next++
	}
	if !p.HasAudio {
		args = append(args, "-f", "lavfi", "-i",
			fmt.Sprintf("anullsrc=channel_layout=%s:sample_rate=%d", normalizedChannels, normalizedSampleRate))
		silenceIndex = next
	}

	graph := fmt.Sprintf("[0:v]scale=w=%d:h=%d:flags=lanczos,setsar=1[base]", targetW, targetH)
	video := "[base]"
	if overlayIndex >= 0 {
		graph += fmt.Sprintf(";[%d:v]scale=w=%d:h=%d:flags=lanczos[layer]", overlayIndex, targetW, targetH)
		graph += ";[base][layer]overlay=0:0:eof_action=repeat:format=auto[v]"
		video = "[v]"
	}

	audio := "0:a"
	if silenceIndex >= 0 {
		audio = fmt.Sprint(silenceIndex, ":a")
	}

	args = append(args, "-filter_complex", graph, "-map", video, "-map", audio)
	// -filter_complex turns automatic stream selection off, so the mapping
	// above is the whole of what reaches the output. Rotation has been applied
	// by the scaler, and the tag has to go with it or a player turns the
	// picture a second time.
	args = append(args, "-metadata:s:v:0", "rotate=0")
	args = append(args, t.videoArgs(t.encoderFor(min(targetW, targetH)), t.crf(), speedBalanced)...)
	args = append(args,
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "128k",
		"-ar", fmt.Sprint(normalizedSampleRate),
		"-ac", "2",
		// Silence has no length of its own, so the part is bounded by its
		// picture. Harmless when the audio came from the file.
		"-shortest",
		dst,
	)
	return t.encode(ctx, args, part.Path, []string{dst}, "a part of a joined video")
}
