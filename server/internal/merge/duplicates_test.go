package merge

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// still builds a photograph whose hashes are exactly what is given, so a test
// can say "these two disagree about nine bits" and mean it.
func still(id string, difference, perceptual uint64) Signature {
	return Signature{
		ID:         id,
		MediaKind:  "image",
		Difference: difference,
		Perceptual: perceptual,
		Aspect:     0.75,
		Width:      3024,
		Height:     4032,
		ByteSize:   2_000_000,
		SortTime:   time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// flip returns h with n of its bits inverted, which is how a re-encode is
// spelled in these tests: a picture n bits away from another picture.
func flip(h uint64, n int) uint64 {
	for i := range n {
		h ^= 1 << uint(i)
	}
	return h
}

func groupIDs(groups []Group) [][]string {
	out := make([][]string, len(groups))
	for i, g := range groups {
		out[i] = g.IDs
	}
	return out
}

func TestDuplicatesGroupsCopiesOfOnePhotograph(t *testing.T) {
	opts := DefaultOptions()
	in := []Signature{
		still("a", 0xdead_beef_0000_1111, 0x0f0f_0f0f_1234_5678),
		still("b", flip(0xdead_beef_0000_1111, 4), flip(0x0f0f_0f0f_1234_5678, 3)),
		still("z", 0x1234_5678_9abc_def0, 0x7777_8888_9999_aaaa),
	}

	got := groupIDs(Duplicates(in, nil, opts))
	if len(got) != 1 || !slices.Equal(got[0], []string{"a", "b"}) {
		t.Fatalf("groups = %v, want one group of a and b", got)
	}
}

// Both hashes have to agree. One alone is not enough, and this is the case that
// says why: two flat frames have identical difference hashes and nothing else
// in common.
func TestDuplicatesRequiresBothHashes(t *testing.T) {
	opts := DefaultOptions()
	sameDifference := []Signature{
		still("a", 0, 0x0f0f_0f0f_1234_5678),
		still("b", 0, 0x7777_8888_9999_aaaa),
	}
	if got := Duplicates(sameDifference, nil, opts); len(got) != 0 {
		t.Errorf("an identical difference hash alone grouped: %v", groupIDs(got))
	}

	samePerceptual := []Signature{
		still("a", 0xdead_beef_0000_1111, 0x1111_2222_3333_4444),
		still("b", 0x1234_5678_9abc_def0, 0x1111_2222_3333_4444),
	}
	if got := Duplicates(samePerceptual, nil, opts); len(got) != 0 {
		t.Errorf("an identical perceptual hash alone grouped: %v", groupIDs(got))
	}
}

func TestDuplicatesRespectsTheThreshold(t *testing.T) {
	opts := DefaultOptions()
	const base = uint64(0xdead_beef_0000_1111)

	for _, n := range []int{0, 1, opts.MaxDifference} {
		in := []Signature{still("a", base, 0), still("b", flip(base, n), 0)}
		if got := Duplicates(in, nil, opts); len(got) != 1 {
			t.Errorf("%d bits apart did not group", n)
		}
	}
	for _, n := range []int{opts.MaxDifference + 1, 20, 40} {
		in := []Signature{still("a", base, 0), still("b", flip(base, n), 0)}
		if got := Duplicates(in, nil, opts); len(got) != 0 {
			t.Errorf("%d bits apart grouped: %v", n, groupIDs(got))
		}
	}
}

// The cheapest guard there is. Two pictures squashed into the same square from
// shapes that share nothing are the commonest false positive, and the aspect
// ratio is stored beside the hash precisely so this can be asked.
func TestDuplicatesRefusesDifferentShapes(t *testing.T) {
	opts := DefaultOptions()
	a := still("a", 0xdead_beef_0000_1111, 0x0f0f_0f0f_1234_5678)
	b := a
	b.ID = "b"
	b.Aspect = 1.78 // a panorama against a portrait

	if got := Duplicates([]Signature{a, b}, nil, opts); len(got) != 0 {
		t.Errorf("two different shapes grouped: %v", groupIDs(got))
	}

	// A 4:3 crop of a 4:3 photo at a different resolution is still 4:3, which
	// is the case this must not refuse.
	b.Aspect = a.Aspect * 1.01
	if got := Duplicates([]Signature{a, b}, nil, opts); len(got) != 1 {
		t.Error("two copies of one shape did not group")
	}
}

// A photograph and a video are never the same item however alike a poster frame
// makes them look.
func TestDuplicatesNeverMixesMedia(t *testing.T) {
	a := still("a", 0xdead_beef_0000_1111, 0x0f0f_0f0f_1234_5678)
	b := a
	b.ID, b.MediaKind = "b", "video"
	b.DurationSeconds = 10
	for i := range 20 {
		b.FrameDifference = append(b.FrameDifference, uint64(i))
		b.FramePerceptual = append(b.FramePerceptual, uint64(i))
	}

	if got := Duplicates([]Signature{a, b}, nil, DefaultOptions()); len(got) != 0 {
		t.Errorf("a still and a clip grouped: %v", groupIDs(got))
	}
}

// clip builds a video whose sampled frames are all `apart` bits from base.
func clip(id string, seconds float64, base uint64, apart, frames int) Signature {
	s := Signature{
		ID:              id,
		MediaKind:       "video",
		Difference:      flip(base, apart),
		Perceptual:      flip(base, apart),
		Aspect:          0.5625,
		DurationSeconds: seconds,
		Width:           1080,
		Height:          1920,
		ByteSize:        20_000_000,
		SortTime:        time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for i := range frames {
		h := base ^ (uint64(i) << 40)
		s.FrameDifference = append(s.FrameDifference, flip(h, apart))
		s.FramePerceptual = append(s.FramePerceptual, flip(h, apart))
	}
	return s
}

func TestDuplicatesGroupsTwoCopiesOfOneClip(t *testing.T) {
	in := []Signature{
		clip("a", 12.0, 0xabcd_ef01_2345_6789, 0, 20),
		clip("b", 12.1, 0xabcd_ef01_2345_6789, 5, 20),
	}
	got := groupIDs(Duplicates(in, nil, DefaultOptions()))
	if len(got) != 1 || !slices.Equal(got[0], []string{"a", "b"}) {
		t.Fatalf("groups = %v, want one group of a and b", got)
	}
}

// The frames are sampled across the running time, so position seven of a
// ten-second clip and position seven of a minute are unrelated instants. Two
// clips of different lengths are never compared at all.
func TestDuplicatesRefusesClipsOfDifferentLengths(t *testing.T) {
	in := []Signature{
		clip("a", 10, 0xabcd_ef01_2345_6789, 0, 20),
		clip("b", 60, 0xabcd_ef01_2345_6789, 0, 20),
	}
	if got := Duplicates(in, nil, DefaultOptions()); len(got) != 0 {
		t.Errorf("clips of 10s and 60s grouped: %v", groupIDs(got))
	}
}

// A clip nothing could sample is not comparable to anything. Letting it through
// on its poster frame alone would group every video that opens on a dark room.
func TestDuplicatesIgnoresAnUnsampledClip(t *testing.T) {
	opts := DefaultOptions()
	a := clip("a", 12, 0xabcd_ef01_2345_6789, 0, 20)
	b := clip("b", 12, 0xabcd_ef01_2345_6789, 0, 20)
	b.FrameDifference, b.FramePerceptual = nil, nil

	if got := Duplicates([]Signature{a, b}, nil, opts); len(got) != 0 {
		t.Errorf("a clip with no sampled frames grouped: %v", groupIDs(got))
	}
}

// A mean rather than a maximum: one frame of a re-encode landing on the far
// side of a cut is not evidence that two clips differ.
func TestDuplicatesToleratesOneBadFrame(t *testing.T) {
	a := clip("a", 12, 0xabcd_ef01_2345_6789, 0, 20)
	b := clip("b", 12, 0xabcd_ef01_2345_6789, 2, 20)
	b.FrameDifference[7] = ^b.FrameDifference[7]
	b.FramePerceptual[7] = ^b.FramePerceptual[7]

	if got := Duplicates([]Signature{a, b}, nil, DefaultOptions()); len(got) != 1 {
		t.Error("one wildly different frame in twenty broke the match")
	}
}

// ...but not every frame.
func TestDuplicatesRefusesClipsThatDisagreeThroughout(t *testing.T) {
	a := clip("a", 12, 0xabcd_ef01_2345_6789, 0, 20)
	b := clip("b", 12, 0xabcd_ef01_2345_6789, 20, 20)

	if got := Duplicates([]Signature{a, b}, nil, DefaultOptions()); len(got) != 0 {
		t.Errorf("clips 20 bits apart on every frame grouped: %v", groupIDs(got))
	}
}

// Transitivity, which is what makes this clustering rather than pairing: a is
// near b, b is near c, and a may be nowhere near c. All three are one pile of
// copies and the review page has to be offered all three at once.
func TestDuplicatesClustersTransitively(t *testing.T) {
	const base = uint64(0xdead_beef_0000_1111)
	opts := DefaultOptions()
	in := []Signature{
		still("a", base, base),
		still("b", flip(base, 8), flip(base, 8)),
		still("c", flip(base, 16), flip(base, 16)),
	}
	if d := 16; d <= opts.MaxDifference {
		t.Fatalf("this test assumes a and c are further apart than the threshold")
	}

	got := groupIDs(Duplicates(in, nil, opts))
	if len(got) != 1 || !slices.Equal(got[0], []string{"a", "b", "c"}) {
		t.Fatalf("groups = %v, want a single pile of all three", got)
	}
}

// A dismissal has to stick, or the scan asks the same question every week.
func TestDuplicatesHonoursBlockedPairs(t *testing.T) {
	const base = uint64(0xdead_beef_0000_1111)
	in := []Signature{
		still("a", base, base),
		still("b", flip(base, 3), flip(base, 3)),
	}

	blocked := BlockedPairs{}
	blocked.Add("b", "a") // deliberately the other way round
	if got := Duplicates(in, blocked, DefaultOptions()); len(got) != 0 {
		t.Errorf("a dismissed pair came back: %v", groupIDs(got))
	}
}

// The default keeper, which is what the review page preselects: the biggest
// picture, then the largest file, then the oldest.
func TestRankPrefersResolutionThenBytesThenAge(t *testing.T) {
	old := time.Date(2016, 5, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC)

	group := []Signature{
		{ID: "small", Width: 1024, Height: 768, ByteSize: 900_000, SortTime: old},
		{ID: "big-recent", Width: 4032, Height: 3024, ByteSize: 3_000_000, SortTime: recent},
		{ID: "big-light", Width: 4032, Height: 3024, ByteSize: 1_500_000, SortTime: old},
	}
	Rank(group)

	want := []string{"big-recent", "big-light", "small"}
	for i, s := range group {
		if s.ID != want[i] {
			t.Fatalf("rank = %v, want %v", []string{group[0].ID, group[1].ID, group[2].ID}, want)
		}
	}
}

func TestRankBreaksAnExactTieOnAge(t *testing.T) {
	old := time.Date(2016, 5, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC)

	group := []Signature{
		{ID: "newer", Width: 4032, Height: 3024, ByteSize: 3_000_000, SortTime: recent},
		{ID: "older", Width: 4032, Height: 3024, ByteSize: 3_000_000, SortTime: old},
	}
	Rank(group)
	if group[0].ID != "older" {
		t.Errorf("kept %q; between two identical copies the oldest wins", group[0].ID)
	}
}

// The groups and the order inside them have to be the same on every run over
// the same input, because the fingerprint that stops a rescan re-asking is
// computed from exactly that.
func TestDuplicatesIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	var in []Signature
	for i := range 60 {
		// Twenty piles of three, shuffled into one list.
		base := uint64(i/3) * 0x1111_1111_1111_1111
		s := still(fmt.Sprintf("a%02d", i), flip(base, i%3), flip(base, i%3))
		in = append(in, s)
	}
	first := groupIDs(Duplicates(in, nil, DefaultOptions()))
	if len(first) == 0 {
		t.Fatal("the fixture produced no groups at all")
	}

	for range 10 {
		rng.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
		got := groupIDs(Duplicates(in, nil, DefaultOptions()))
		if !slices.EqualFunc(got, first, slices.Equal) {
			t.Fatalf("a shuffled input gave %v, the first run gave %v", got, first)
		}
	}
}

func TestDuplicatesIgnoresRowsWithNoID(t *testing.T) {
	const base = uint64(0xdead_beef_0000_1111)
	in := []Signature{still("", base, base), still("", base, base), still("a", base, base)}
	if got := Duplicates(in, nil, DefaultOptions()); len(got) != 0 {
		t.Errorf("rows with no id grouped: %v", groupIDs(got))
	}
}

// The sweep is every pair against every other, which is a defensible choice at
// this archive's scale and stops being one somewhere above it. This pins the
// scale where it is still fine, so the day it is not, something says so.
func BenchmarkDuplicatesOverAWholeLibrary(b *testing.B) {
	rng := rand.New(rand.NewPCG(11, 13))
	in := make([]Signature, 25_000)
	for i := range in {
		in[i] = still(fmt.Sprintf("a%06d", i), rng.Uint64(), rng.Uint64())
	}

	b.ResetTimer()
	for b.Loop() {
		Duplicates(in, nil, DefaultOptions())
	}
}
