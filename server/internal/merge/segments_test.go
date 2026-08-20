package merge

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

var epoch = time.Date(2018, 10, 7, 5, 42, 5, 0, time.UTC)

// at builds a segment n seconds past the epoch, full-length and phone-shaped
// unless told otherwise.
func at(id string, seconds float64, opts ...func(*VideoSegment)) VideoSegment {
	v := VideoSegment{
		ID:              id,
		At:              epoch.Add(time.Duration(seconds * float64(time.Second))),
		DurationSeconds: 10.03,
		Width:           540,
		Height:          1110,
	}
	for _, o := range opts {
		o(&v)
	}
	return v
}

func lasting(d float64) func(*VideoSegment) {
	return func(v *VideoSegment) { v.DurationSeconds = d }
}

func sized(w, h int) func(*VideoSegment) {
	return func(v *VideoSegment) { v.Width, v.Height = w, h }
}

func ids(groups []Group) [][]string {
	out := make([][]string, len(groups))
	for i, g := range groups {
		if g.Kind != KindSegments {
			panic("segment scan produced a " + g.Kind)
		}
		out[i] = g.IDs
	}
	return out
}

func assertGroups(t *testing.T, got []Group, want [][]string) {
	t.Helper()
	gotIDs := ids(got)
	if !slices.EqualFunc(gotIDs, want, slices.Equal) {
		t.Errorf("groups = %v, want %v", gotIDs, want)
	}
}

// The shape the real export produces: six pieces of one minute, each exactly
// ten seconds after the last, the final one short.
func TestSegmentsChainsAWholeRecording(t *testing.T) {
	in := []VideoSegment{
		at("a", 0), at("b", 10), at("c", 20),
		at("d", 30), at("e", 40), at("f", 50, lasting(9.82)),
	}
	assertGroups(t, Segments(in), [][]string{{"a", "b", "c", "d", "e", "f"}})
}

// Input order must not matter: the scan sorts by capture instant, which is the
// only ordering that means anything here. The EXIF time on these files drifts
// by over a minute across one recording, so a caller handing them over in
// sort_time order is handing them over shuffled.
func TestSegmentsSortsBeforeChaining(t *testing.T) {
	in := []VideoSegment{at("c", 20), at("a", 0), at("f", 50), at("b", 10), at("e", 40), at("d", 30)}
	assertGroups(t, Segments(in), [][]string{{"a", "b", "c", "d", "e", "f"}})
}

func TestSegmentsLeavesALoneClipAlone(t *testing.T) {
	if got := Segments([]VideoSegment{at("a", 0)}); len(got) != 0 {
		t.Errorf("one clip produced %v", ids(got))
	}
}

// Two memories saved a minute apart are two memories.
func TestSegmentsDoesNotChainAcrossAGap(t *testing.T) {
	in := []VideoSegment{at("a", 0), at("b", 60), at("c", 120)}
	if got := Segments(in); len(got) != 0 {
		t.Errorf("unrelated clips produced %v", ids(got))
	}
}

// The spacing is exact. Nine seconds and eleven seconds are both somebody
// filming twice, and this is the tolerance that says so.
func TestSegmentsRefusesTheWrongSpacing(t *testing.T) {
	for _, gap := range []float64{8, 9, 9.5, 10.5, 11, 12, 20} {
		in := []VideoSegment{at("a", 0), at("b", gap)}
		if got := Segments(in); len(got) != 0 {
			t.Errorf("a %.1fs gap linked: %v", gap, ids(got))
		}
	}
	for _, gap := range []float64{9.8, 10, 10.2} {
		in := []VideoSegment{at("a", 0), at("b", gap)}
		if got := Segments(in); len(got) != 1 {
			t.Errorf("a %.1fs gap did not link", gap)
		}
	}
}

// The rule that removes the last of the coincidences: a piece that ran to three
// seconds is the end of a recording, so whatever comes ten seconds later is the
// start of a different one.
func TestSegmentsRefusesToFollowAShortPiece(t *testing.T) {
	in := []VideoSegment{at("a", 0, lasting(3.2)), at("b", 10)}
	if got := Segments(in); len(got) != 0 {
		t.Errorf("a short piece was chained onward: %v", ids(got))
	}

	// And the boundary itself, which is set just under ten because a container
	// rounds a ten-second piece to anywhere from 9.5 upwards.
	for _, d := range []float64{MinFullSegment, MinFullSegment + 0.1, 10.03} {
		if got := Segments([]VideoSegment{at("a", 0, lasting(d)), at("b", 10)}); len(got) != 1 {
			t.Errorf("a %.2fs piece did not chain onward", d)
		}
	}
	if got := Segments([]VideoSegment{at("a", 0, lasting(MinFullSegment-0.01)), at("b", 10)}); len(got) != 0 {
		t.Errorf("a piece just under the floor chained onward: %v", ids(got))
	}
}

// A chain must not run through a change of camera.
func TestSegmentsRefusesAChangeOfSize(t *testing.T) {
	in := []VideoSegment{at("a", 0), at("b", 10, sized(1080, 1920)), at("c", 20, sized(1080, 1920))}
	assertGroups(t, Segments(in), [][]string{{"b", "c"}})
}

// Two chains in one evening, and nothing joining them.
func TestSegmentsFindsSeveralRuns(t *testing.T) {
	in := []VideoSegment{
		at("a", 0), at("b", 10), at("c", 20, lasting(6.8)),
		at("d", 300), at("e", 310, lasting(7.5)),
	}
	assertGroups(t, Segments(in), [][]string{{"a", "b", "c"}, {"d", "e"}})
}

// Rows the metadata job has not reached yet, or could not read. Two of them
// would compare equal on every field and chain on no evidence at all.
func TestSegmentsIgnoresIncompleteRows(t *testing.T) {
	cases := map[string][]VideoSegment{
		"no id":       {at("", 0), at("b", 10)},
		"no instant":  {{ID: "a", DurationSeconds: 10, Width: 540, Height: 1110}, at("b", 10)},
		"no duration": {at("a", 0, lasting(0)), at("b", 10)},
		"no size":     {at("a", 0, sized(0, 0)), at("b", 10, sized(0, 0))},
	}
	for name, in := range cases {
		if got := Segments(in); len(got) != 0 {
			t.Errorf("%s: chained anyway: %v", name, ids(got))
		}
	}
}

// The same input twice has to produce the same groups in the same order, or the
// fingerprint that makes a rescan idempotent changes every run.
func TestSegmentsIsDeterministic(t *testing.T) {
	in := []VideoSegment{at("d", 30), at("b", 10), at("a", 0), at("c", 20)}
	first := ids(Segments(in))
	for range 20 {
		if got := ids(Segments(in)); !slices.EqualFunc(got, first, slices.Equal) {
			t.Fatalf("second run gave %v, first gave %v", got, first)
		}
	}
}

// Two clips claiming the same instant make "the clip ten seconds after this
// one" a question with two answers, so neither of them chains to anything. The
// alternative is picking whichever sorted first, which is picking at random.
func TestSegmentsRefusesToChainFromASharedInstant(t *testing.T) {
	if got := Segments([]VideoSegment{at("z", 0), at("a", 0), at("m", 10)}); len(got) != 0 {
		t.Errorf("chained out of an ambiguous instant: %v", ids(got))
	}
	// The same the other way round: the ambiguity is at the far end.
	if got := Segments([]VideoSegment{at("a", 0), at("m", 10), at("n", 10)}); len(got) != 0 {
		t.Errorf("chained into an ambiguous instant: %v", ids(got))
	}
}

// And a crowded instant cuts a run rather than being skipped over: the pieces
// on either side of it are ten seconds from the ambiguity, not from each other.
func TestSegmentsCutsARunAtASharedInstant(t *testing.T) {
	in := []VideoSegment{
		at("a", 0), at("b", 10), at("c", 20),
		at("d", 30), at("d2", 30),
		at("e", 40), at("f", 50),
	}
	assertGroups(t, Segments(in), [][]string{{"a", "b", "c"}, {"e", "f"}})
}

func TestSegmentsHandlesALongRun(t *testing.T) {
	var in []VideoSegment
	for i := range 12 {
		in = append(in, at(fmt.Sprintf("s%02d", i), float64(i)*10))
	}
	got := Segments(in)
	if len(got) != 1 || len(got[0].IDs) != 12 {
		t.Fatalf("got %v, want one run of 12", ids(got))
	}
}
