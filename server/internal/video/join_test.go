package video

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// synth writes a clip of the given length, size and colour so a join has
// several distinguishable parts to put in order.
//
// Generated rather than checked in: what these tests are about is whether
// several files become one file of the right length, and that needs parts whose
// properties can be varied one at a time. testdata holds real camera output for
// the tests that need a real container.
func synth(t *testing.T, dir, name string, seconds float64, width, height int, opts ...string) string {
	t.Helper()
	dst := filepath.Join(dir, name)

	args := []string{"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=15:duration=%g", width, height, seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=44100:duration=%g", seconds),
	}
	args = append(args, opts...)
	args = append(args,
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-ac", "2", "-ar", "44100",
		"-shortest", dst)

	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build a fixture clip (is ffmpeg installed?): %v: %s", err, out)
	}
	return dst
}

func mustProbe(t *testing.T, path string) Info {
	t.Helper()
	info, err := New().Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe %s: %v", path, err)
	}
	return info
}

// The path all but a handful of real groups take: identical parts, joined by
// moving packets, with the output exactly as long as its inputs added up to.
func TestJoinCopiesIdenticalParts(t *testing.T) {
	dir := t.TempDir()
	parts := []Part{
		{Path: synth(t, dir, "a.mp4", 2, 320, 240)},
		{Path: synth(t, dir, "b.mp4", 2, 320, 240)},
		{Path: synth(t, dir, "c.mp4", 1, 320, 240)},
	}
	dst := filepath.Join(dir, "joined.mp4")

	res, err := New().Join(context.Background(), parts, dst, JoinOptions{})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !res.Copied {
		t.Errorf("Copied = false (%s); identical parts should not have been re-encoded", res.Why)
	}
	if res.Why != "" {
		t.Errorf("Why = %q on a copy, want empty", res.Why)
	}
	if want := 5.0; math.Abs(res.DurationSeconds-want) > joinDurationSlack {
		t.Errorf("DurationSeconds = %v, want ~%v", res.DurationSeconds, want)
	}

	info := mustProbe(t, dst)
	if math.Abs(info.DurationSeconds-5) > joinDurationSlack {
		t.Errorf("joined file is %vs, want ~5s", info.DurationSeconds)
	}
	if info.Width != 320 || info.Height != 240 {
		t.Errorf("joined size = %dx%d, want 320x240", info.Width, info.Height)
	}
}

// A resolution that changed mid-recording. The copy is impossible, the parts
// are normalised to the largest frame among them, and the result is still one
// file of the right length.
func TestJoinNormalizesPartsThatDisagree(t *testing.T) {
	dir := t.TempDir()
	parts := []Part{
		{Path: synth(t, dir, "small.mp4", 2, 320, 240)},
		{Path: synth(t, dir, "big.mp4", 2, 640, 480)},
	}
	dst := filepath.Join(dir, "joined.mp4")

	res, err := New().Join(context.Background(), parts, dst, JoinOptions{})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if res.Copied {
		t.Fatal("Copied = true; parts of different sizes cannot be stream-copied")
	}
	if res.Why == "" {
		t.Error("Why is empty on a re-encode; the reason is what gets archived")
	}

	info := mustProbe(t, dst)
	if math.Abs(info.DurationSeconds-4) > joinDurationSlack {
		t.Errorf("joined file is %vs, want ~4s", info.DurationSeconds)
	}
	// The larger frame wins: normalising is already the lossy path and there is
	// no reason to throw away detail on top of it.
	if info.Width != 640 || info.Height != 480 {
		t.Errorf("joined size = %dx%d, want 640x480", info.Width, info.Height)
	}
}

// The case a naive concatenation gets wrong in a way nobody notices until they
// play the file: parts where only some have sound. The silent one is given a
// track so the join has one continuous stream rather than a gap a player stops
// at.
func TestJoinGivesASilentPartASoundTrack(t *testing.T) {
	dir := t.TempDir()
	silent := synth(t, dir, "silent.mp4", 2, 320, 240, "-an")
	parts := []Part{
		{Path: synth(t, dir, "loud.mp4", 2, 320, 240)},
		{Path: silent},
	}
	dst := filepath.Join(dir, "joined.mp4")

	res, err := New().Join(context.Background(), parts, dst, JoinOptions{})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if res.Copied {
		t.Fatal("Copied = true; a silent part and a loud one cannot be stream-copied together")
	}

	profile, err := New().profile(context.Background(), dst)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if !profile.HasAudio {
		t.Error("the joined file has no audio stream")
	}
	if math.Abs(profile.Duration-4) > joinDurationSlack {
		t.Errorf("joined file is %vs, want ~4s", profile.Duration)
	}
}

// An overlay is pixels, so it forces the re-encode path even when everything
// else about the parts agrees.
func TestJoinBurnsInACaptionLayer(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "layer.png")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=red@0.5:s=320x240:d=1",
		"-frames:v", "1", layer).CombinedOutput(); err != nil {
		t.Skipf("could not build an overlay fixture: %v: %s", err, out)
	}

	parts := []Part{
		{Path: synth(t, dir, "a.mp4", 1, 320, 240), Overlay: layer},
		{Path: synth(t, dir, "b.mp4", 1, 320, 240)},
	}
	dst := filepath.Join(dir, "joined.mp4")

	res, err := New().Join(context.Background(), parts, dst, JoinOptions{})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if res.Copied {
		t.Fatal("Copied = true; a part with a caption layer has to be re-encoded")
	}
	if math.Abs(res.DurationSeconds-2) > joinDurationSlack {
		t.Errorf("joined file is %vs, want ~2s", res.DurationSeconds)
	}
}

func TestJoinRefusesFewerThanTwoParts(t *testing.T) {
	dir := t.TempDir()
	one := []Part{{Path: synth(t, dir, "a.mp4", 1, 320, 240)}}

	for _, parts := range [][]Part{nil, one} {
		if _, err := New().Join(context.Background(), parts, filepath.Join(dir, "out.mp4"), JoinOptions{}); err == nil {
			t.Errorf("Join accepted %d parts", len(parts))
		}
	}
}

func TestJoinReportsAMissingPart(t *testing.T) {
	dir := t.TempDir()
	parts := []Part{
		{Path: synth(t, dir, "a.mp4", 1, 320, 240)},
		{Path: filepath.Join(dir, "gone.mp4")},
	}
	if _, err := New().Join(context.Background(), parts, filepath.Join(dir, "out.mp4"), JoinOptions{}); err == nil {
		t.Fatal("Join succeeded with a part that does not exist")
	}
}

// Join leaves nothing behind. It stages normalised parts and a list file beside
// its destination, and a merge that runs over a whole library would otherwise
// fill the archive drive with them.
func TestJoinCleansUpAfterItself(t *testing.T) {
	dir := t.TempDir()
	parts := []Part{
		{Path: synth(t, dir, "small.mp4", 1, 320, 240)},
		{Path: synth(t, dir, "big.mp4", 1, 640, 480)},
	}
	dst := filepath.Join(dir, "joined.mp4")
	if _, err := New().Join(context.Background(), parts, dst, JoinOptions{}); err != nil {
		t.Fatalf("Join: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("Join left %s behind", e.Name())
		}
	}
}

// The whole reason SampleFrames resamples by time rather than by frame number:
// the same footage at a different frame rate and a different size still has to
// produce a comparable sequence.
func TestSampleFramesLinesUpAcrossAReEncode(t *testing.T) {
	dir := t.TempDir()
	src := synth(t, dir, "src.mp4", 4, 320, 240)

	alt := filepath.Join(dir, "alt.mp4")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error",
		"-i", src, "-vf", "scale=160:120,fps=30", "-c:v", "libx264", "-preset", "ultrafast",
		"-crf", "34", "-an", alt).CombinedOutput(); err != nil {
		t.Skipf("could not re-encode the fixture: %v: %s", err, out)
	}

	tool := New()
	const count, edge = 8, 32
	a, err := tool.SampleFrames(context.Background(), src, count, edge, mustProbe(t, src))
	if err != nil {
		t.Fatalf("SampleFrames(src): %v", err)
	}
	// Deliberately the *source's* duration, which is what the scan does: two
	// clips are only compared when their lengths already agree, and both
	// sequences have to be spaced identically to be compared at all.
	b, err := tool.SampleFrames(context.Background(), alt, count, edge, mustProbe(t, src))
	if err != nil {
		t.Fatalf("SampleFrames(alt): %v", err)
	}

	if len(a) != count {
		t.Fatalf("got %d frames from the source, want %d", len(a), count)
	}
	if len(b) < count-1 {
		t.Fatalf("got %d frames from the re-encode, want at least %d", len(b), count-1)
	}
	for i, frame := range a {
		if len(frame) != edge*edge {
			t.Fatalf("frame %d is %d bytes, want %d", i, len(frame), edge*edge)
		}
	}

	// The frames themselves are compared in internal/merge, which owns the
	// thresholds. What this test is entitled to say is that both sequences
	// exist, are the right shape, and are not the same frame repeated — a
	// sampler that ignored its spacing would produce exactly that.
	same := 0
	for i := 1; i < len(a); i++ {
		if string(a[i]) == string(a[0]) {
			same++
		}
	}
	if same == len(a)-1 {
		t.Error("every sampled frame is identical; the sampler is not walking the clip")
	}
}

func TestSampleFramesRefusesAClipWithNoDuration(t *testing.T) {
	dir := t.TempDir()
	src := synth(t, dir, "a.mp4", 1, 320, 240)

	_, err := New().SampleFrames(context.Background(), src, 8, 32, Info{Width: 320, Height: 240})
	if err == nil {
		t.Fatal("SampleFrames accepted a clip whose duration is unknown")
	}
}

// lopsided writes a clip whose sound outlasts its picture by two seconds.
//
// It exists to produce the one join failure that leaves a playable file behind.
// profile reads the video stream's duration, so two of these are expected to
// add up to two seconds; the concat demuxer offsets each part by the *format*
// duration of the one before it, so the join comes out at four. Real Snapchat
// exports miss by a tenth of a second rather than by a factor of two, but the
// arithmetic that catches them is the arithmetic being tested here.
func lopsided(t *testing.T, dir, name string) string {
	t.Helper()
	dst := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=3",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-ac", "2", "-ar", "44100", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build a fixture clip (is ffmpeg installed?): %v: %s", err, out)
	}
	return dst
}

// A join whose running time does not add up is refused — and the file it
// refused is left where it is, because it is the only evidence of what went
// wrong that anybody can look at, and the merge worker files it under the group
// for exactly that.
func TestJoinRefusesADurationMismatchAndKeepsTheFile(t *testing.T) {
	dir := t.TempDir()
	parts := []Part{
		{Path: lopsided(t, dir, "a.mp4")},
		{Path: lopsided(t, dir, "b.mp4")},
	}
	dst := filepath.Join(dir, "joined.mp4")

	_, err := New().Join(context.Background(), parts, dst, JoinOptions{})
	var mismatch *DurationMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("Join error = %v, want a *DurationMismatch", err)
	}
	if mismatch.Parts != 2 || math.Abs(mismatch.Expected-2) > 0.2 {
		t.Errorf("mismatch = %+v, want 2 parts expected to add up to ~2s", mismatch)
	}
	if math.Abs(mismatch.Actual-mismatch.Expected) <= joinDurationSlack {
		t.Errorf("mismatch = %+v, but those are within the slack", mismatch)
	}

	info, err := os.Stat(dst)
	if err != nil || info.Size() == 0 {
		t.Fatalf("the refused join is not on disk to be watched: %v", err)
	}
}

// The same join, overruled. Somebody has watched the file above and can see
// that none of the parts is missing, which is a judgement the arithmetic cannot
// make.
func TestJoinArchivesAMismatchWhenToldTo(t *testing.T) {
	dir := t.TempDir()
	parts := []Part{
		{Path: lopsided(t, dir, "a.mp4")},
		{Path: lopsided(t, dir, "b.mp4")},
	}

	res, err := New().Join(context.Background(), parts,
		filepath.Join(dir, "joined.mp4"), JoinOptions{AllowDurationMismatch: true})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !res.Mismatch {
		t.Error("Mismatch = false; the sidecar would not record that this file was doubted")
	}
	if math.Abs(res.Expected-2) > 0.2 {
		t.Errorf("Expected = %v, want the ~2s the parts add up to", res.Expected)
	}
}
