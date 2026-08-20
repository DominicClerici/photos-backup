// Package merge finds sets of assets that ought to be one asset, and nothing
// else: it does no SQL, opens no files, and runs no ffmpeg. What it is handed
// is rows, and what it hands back is groups.
//
// That boundary is here for the same reason internal/snapchat has one. Every
// interesting decision in this feature is a threshold — ten seconds, nine bits,
// two percent — every one of them is a guess made against one library, and a
// guess is only adjustable if it can be re-run over a table of numbers instead
// of over a hundred gigabytes on an external drive.
package merge

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// The two kinds of group this package finds. They are the `kind` column of
// merge_groups and part of the API the gallery reads, so they are spelled once,
// here.
const (
	// KindSegments is a recording Snapchat exported in ten-second pieces.
	KindSegments = "video-segments"
	// KindDuplicate is several copies of one photograph or clip.
	KindDuplicate = "duplicate"
)

// Group is a set of assets that should become one, in the order they were found
// to belong in.
type Group struct {
	Kind string
	// IDs are the members. For KindSegments the order is chronological and is
	// the order they will be concatenated in; for KindDuplicate it is best
	// first, and it is a suggestion.
	IDs []string
}

// SegmentSeconds is the length Snapchat cuts a memory at.
//
// It is not a duration this package measures — it is the spacing between the
// capture instants of consecutive pieces, and it is exact. A file's own
// duration comes out of the container anywhere between 9.5 and 12 seconds
// depending on how the encoder ended it, but the *history* is written to the
// second and consecutive pieces are always precisely ten apart.
const SegmentSeconds = 10.0

// SegmentSlack is how far a gap may fall from SegmentSeconds and still count.
//
// A quarter of a second, which is to say: essentially none. The history
// document is written to whole seconds, so a genuine link is 10.000 and the
// only thing this tolerance absorbs is a caller that has been through a
// float. It is deliberately far tighter than the file durations, which is the
// whole reason this detector works — see the package comment on evidence.
const SegmentSlack = 0.25

// MinFullSegment is how long a piece must be to have something after it.
//
// A recording is cut into ten-second pieces and a final shorter one, so a piece
// that runs to nine and a half seconds is a piece that filled its slot, and a
// piece that runs to three is the end of a recording. Requiring this of the
// *earlier* member of every link is what separates a continuation from two
// memories that happen to have been saved ten seconds apart, and against this
// archive it removes fourteen of the 287 candidate links.
//
// Slightly below ten because a container rounds: the pieces measured here come
// out between 9.5 and 12.0, and the ones that came out at 9.51 are not endings.
const MinFullSegment = 9.4

// VideoSegment is one clip as the segment scan sees it.
type VideoSegment struct {
	ID string
	// At is the capture instant from the source's own record — for Snapchat,
	// the Date in memories_history.json. Not the EXIF time and not sort_time:
	// the QuickTime creation date on these files is when the piece was written
	// out, it drifts by up to eighty seconds across one recording, and ordering
	// by it puts the pieces of a minute of video in the wrong order.
	At time.Time
	// DurationSeconds is the clip's own running time, from the container.
	DurationSeconds float64
	Width, Height   int
}

// Segments finds the recordings that were exported in pieces.
//
// The rule is a chain: sort every candidate by capture instant, and link one to
// the next when their instants are exactly SegmentSeconds apart, the earlier
// one is a full piece, and the two are the same size. A chain of two or more
// links is a group; everything else is left alone.
//
// The size check is what stops a chain running through a change of camera, and
// it costs almost nothing — two consecutive pieces of one recording are always
// the same size, and two unrelated memories often are not.
//
// # What this deliberately does not do
//
// It does not look at the pixels, and that is a measurement rather than an
// omission. The obvious corroboration is that the last frame of one piece and
// the first frame of the next are three hundredths of a second apart in a
// continuous recording and ought to be nearly identical. Measured across the
// 271 candidate links in this archive against a control set of memories twenty
// to a hundred and twenty seconds apart, they are not: the median difference
// hash distance across a real boundary is 21 bits and across the control 30,
// with the two distributions overlapping from 5 bits to 43. Handheld video of
// one scene stays similar to itself for a whole minute, so "these two frames
// look alike" says almost nothing about whether they are adjacent — a second
// apart *inside* one piece measured further apart than a real boundary did.
//
// A gate on that would have refused a large fraction of genuine merges to
// exclude a couple of false ones. What is used instead is the timestamp
// evidence, which is strong — 287 links at exactly ten seconds against single
// digits at every other spacing — plus the fact that the merge is undoable: the
// pieces go to the trash rather than away, and the joined file can be put back
// where it came from for a year afterwards.
func Segments(in []VideoSegment) []Group {
	candidates := make([]VideoSegment, 0, len(in))
	for _, v := range in {
		if v.ID == "" || v.At.IsZero() || v.DurationSeconds <= 0 {
			continue
		}
		candidates = append(candidates, v)
	}
	slices.SortFunc(candidates, func(a, b VideoSegment) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	// Instants that more than one candidate claims. A memory whose history row
	// could not be told apart from its neighbours' shares a Date with them, and
	// then "the clip ten seconds after this one" has several answers and no way
	// to choose between them — chaining would pick whichever sorted first,
	// which is to say at random.
	//
	// Refusing to link at an ambiguous instant costs nothing measurable: across
	// this archive 375 of 1,294 memory videos share a capture instant with
	// another, and not one of the 266 links is between two of them. The
	// ambiguity is real and it is simply somewhere else — several separate
	// memories saved in the same second, none of them a continuation.
	crowded := map[time.Time]bool{}
	seen := map[time.Time]bool{}
	for _, v := range candidates {
		if seen[v.At] {
			crowded[v.At] = true
		}
		seen[v.At] = true
	}

	var groups []Group
	var run []VideoSegment
	flush := func() {
		if len(run) > 1 {
			ids := make([]string, len(run))
			for i, v := range run {
				ids[i] = v.ID
			}
			groups = append(groups, Group{Kind: KindSegments, IDs: ids})
		}
		run = nil
	}

	for _, v := range candidates {
		if crowded[v.At] {
			// Neither the end of the run before it nor the start of one after.
			flush()
			continue
		}
		if len(run) > 0 && continues(run[len(run)-1], v) {
			run = append(run, v)
			continue
		}
		flush()
		run = []VideoSegment{v}
	}
	flush()
	return groups
}

// continues reports whether b is the piece that follows a.
func continues(a, b VideoSegment) bool {
	if a.DurationSeconds < MinFullSegment {
		return false
	}
	if a.Width != b.Width || a.Height != b.Height {
		return false
	}
	// Zero dimensions mean the metadata job has not run or could not read the
	// file. Two of those would compare equal and link on no evidence at all.
	if a.Width <= 0 || a.Height <= 0 {
		return false
	}
	gap := b.At.Sub(a.At).Seconds()
	return math.Abs(gap-SegmentSeconds) <= SegmentSlack
}
