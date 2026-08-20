package merge

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/imagehash"
)

// Signature is one asset as the duplicate scan sees it: what it looks like, and
// enough about the file to say which copy of it is the one worth keeping.
type Signature struct {
	ID        string
	MediaKind string

	// Difference and Perceptual are the whole picture, hashed. For a video they
	// are its poster frame, and they are used only to skip pairs cheaply —
	// what decides a video is Frame* below.
	Difference uint64
	Perceptual uint64
	// Aspect is width over height, after orientation.
	Aspect float64

	// FrameDifference and FramePerceptual are a video sampled along its length,
	// one hash per frame, in order. Empty for a still.
	FrameDifference []uint64
	FramePerceptual []uint64

	DurationSeconds float64

	// Everything below decides the ranking rather than the grouping. See
	// Options.Rank.
	Width, Height int
	ByteSize      int64
	SortTime      time.Time
}

// Pixels is the resolution, and the first thing "higher quality" means.
func (s Signature) Pixels() int64 { return int64(s.Width) * int64(s.Height) }

// Options are the thresholds. Every one of them is a guess measured against one
// library; see DefaultOptions for what each was measured to do.
type Options struct {
	// MaxDifference and MaxPerceptual are how many of the 64 bits two stills
	// may disagree about and still be the same photograph. Both must hold.
	MaxDifference int
	MaxPerceptual int

	// MaxAspectRatio is how far apart two shapes may be, as a ratio of the
	// larger to the smaller. It is the cheapest guard there is: two pictures
	// whose grey reductions agree because both were squashed from shapes that
	// share nothing are the most common false positive, and one division
	// removes them.
	MaxAspectRatio float64

	// MaxDurationRatio is the same idea for video, and it does more work: two
	// clips are only compared at all when their lengths already agree, because
	// the frame sequences are sampled across the running time and comparing
	// position seven of a ten-second clip with position seven of a minute is
	// comparing two unrelated instants.
	MaxDurationRatio float64
	// MaxFrameDifference and MaxFramePerceptual are the *mean* distance across
	// the sampled frames. A mean rather than a maximum because one frame of a
	// re-encode can land on the far side of a cut while the other nineteen
	// agree, and a single bad frame is not evidence of anything.
	MaxFrameDifference int
	MaxFramePerceptual int
	// MinFrames is how many frames both clips must have contributed before a
	// verdict is worth anything.
	MinFrames int
}

// DefaultOptions are the thresholds this archive runs on.
//
// The still thresholds are deliberately tight. What is being looked for is the
// same photograph that has been through a re-encode — an export downscaling a
// HEIC to a JPEG, a messaging app stripping it and saving it again — and across
// that kind of round trip both hashes move by single digits.
//
// # What the number was chosen against
//
// Swept from 3 to 12 bits over six thousand real photographs from this archive.
// The striking result is how little it matters: 513 groups at 3 bits, 484 at 9,
// 494 at 12, and the shape of the answer barely moves — around 380 pairs, 60
// triples, and twenty-odd large groups at every setting. What that says is that
// the population being found is genuinely bimodal. Copies of one photograph land
// within a handful of bits of each other, unrelated photographs land past
// thirty, and there is very little in between for a threshold to arbitrate.
//
// Nine, in the middle of the plateau, on the reasoning that the failure modes
// are not symmetric: a false positive costs one click on a page somebody is
// looking at anyway, and a false negative is a duplicate that is never mentioned
// again.
//
// The one thing the sweep does change is how far transitivity runs. A burst of
// ninety-eight frames chains into a single group at every threshold tried, and
// raising the allowance merges more of those chains together — 1,894 photographs
// in groups at 3 bits against 2,393 at 12. Those groups are correct and they are
// also the reason the review page has to draw a hundred thumbnails without
// flinching: a burst really is a hundred near-identical photographs, and
// deciding what to do about it is exactly the judgement this feature refuses to
// make on anybody's behalf.
//
// The video thresholds are looser per frame and much stricter overall. Twenty
// frames all agreeing to within eleven bits is a far stronger statement than one
// frame agreeing to within nine, so the per-frame allowance can afford to absorb
// a re-encode that moved a cut by a frame. They have not had the same sweep: it
// would mean fetching several gigabytes of video, and the duration filter in
// front of them does much less work than it looks like it does — Snapchat caps a
// clip at ten seconds, so this archive holds 229,000 pairs of unrelated videos
// that agree on length and shape to within two percent. The sequence comparison
// is doing all of the discriminating, and if it turns out to be doing too much
// or too little, these are the two numbers to move.
func DefaultOptions() Options {
	return Options{
		MaxDifference:      9,
		MaxPerceptual:      9,
		MaxAspectRatio:     1.05,
		MaxDurationRatio:   1.02,
		MaxFrameDifference: 11,
		MaxFramePerceptual: 11,
		MinFrames:          6,
	}
}

// Blocked reports pairs that must never be linked, however alike they look. It
// is how a dismissal sticks: somebody has already looked at these two and said
// they are different photographs, and a scan that proposed them again every
// week would be a scan nobody runs twice.
type Blocked interface {
	Blocked(a, b string) bool
}

// BlockedPairs is the obvious implementation, keyed on the two ids in sorted
// order so a caller need not care which way round it asks.
type BlockedPairs map[[2]string]struct{}

func (p BlockedPairs) Add(a, b string) {
	p[pairKey(a, b)] = struct{}{}
}

func (p BlockedPairs) Blocked(a, b string) bool {
	_, ok := p[pairKey(a, b)]
	return ok
}

func pairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// Duplicates clusters signatures into groups of copies of the same thing.
//
// # Why this is written as every pair against every other
//
// It is O(n²), which for the twenty-three thousand signatures in this archive
// is two hundred and sixty million comparisons of two integers — a couple of
// seconds, measured, and the scan runs when somebody asks or after an import
// rather than on a request path. The alternative is the standard trick of
// splitting each hash into k+1 bands and bucketing on them, which turns the
// sweep into something close to linear and is exactly right at a scale this
// archive is not at. It also only works below a threshold set by the number of
// bands, so adopting it early would quietly couple the tuning knob above to the
// indexing strategy.
//
// The honest note for later: this measures around 3 seconds at 25,000
// signatures and the cost is quadratic, so at a million assets it is well over
// an hour and the bands become worth their complexity. Nothing else changes when
// that day comes, because the thresholds and the pair test are already separate
// from the sweep — see BenchmarkDuplicatesOverAWholeLibrary, which is pinned at
// the scale where this is still the right answer.
func Duplicates(sigs []Signature, blocked Blocked, opts Options) []Group {
	usable := make([]Signature, 0, len(sigs))
	for _, s := range sigs {
		if s.ID == "" {
			continue
		}
		if s.MediaKind == "video" && len(s.FrameDifference) < opts.MinFrames {
			// A clip nothing could sample. It is not comparable to anything,
			// and letting it through on its poster frame alone would group
			// every video that opens on a dark room.
			continue
		}
		usable = append(usable, s)
	}
	// Sorted so the sweep, the union-find and therefore the output are the same
	// on every run over the same input. A scan whose groups came out in map
	// order would write a different fingerprint each time and propose the same
	// question forever.
	slices.SortFunc(usable, func(a, b Signature) int { return cmp.Compare(a.ID, b.ID) })

	sets := newUnionFind(len(usable))
	for i := range usable {
		for j := i + 1; j < len(usable); j++ {
			// Same first, and the set lookup only for the pairs that pass
			// it. In a tidy library almost every pair fails on its first hash
			// comparison, so this ordering keeps two union-find walks off the
			// hot path three hundred million times.
			if !Same(usable[i], usable[j], opts) {
				continue
			}
			if blocked != nil && blocked.Blocked(usable[i].ID, usable[j].ID) {
				continue
			}
			sets.union(i, j)
		}
	}

	members := map[int][]Signature{}
	for i, s := range usable {
		root := sets.find(i)
		members[root] = append(members[root], s)
	}

	groups := make([]Group, 0, len(members))
	for _, group := range members {
		if len(group) < 2 {
			continue
		}
		Rank(group)
		ids := make([]string, len(group))
		for i, s := range group {
			ids[i] = s.ID
		}
		groups = append(groups, Group{Kind: KindDuplicate, IDs: ids})
	}
	// By first member, which is by id, which is stable.
	slices.SortFunc(groups, func(a, b Group) int { return cmp.Compare(a.IDs[0], b.IDs[0]) })
	return groups
}

// Same reports whether two signatures are copies of one thing.
//
// Exported because it is the interesting half: a threshold is only defensible
// if it can be pointed at two real signatures and asked.
func Same(a, b Signature, opts Options) bool {
	if a.MediaKind != b.MediaKind {
		return false
	}
	if !ratioWithin(a.Aspect, b.Aspect, opts.MaxAspectRatio) {
		return false
	}

	if a.MediaKind != "video" {
		return imagehash.Distance(a.Difference, b.Difference) <= opts.MaxDifference &&
			imagehash.Distance(a.Perceptual, b.Perceptual) <= opts.MaxPerceptual
	}

	if !ratioWithin(a.DurationSeconds, b.DurationSeconds, opts.MaxDurationRatio) {
		return false
	}
	// The poster frames first, because it is two integers against forty. Loose,
	// deliberately: it is here to skip obviously unrelated clips cheaply, not
	// to decide anything. A clip that opens on a title card shared with another
	// clip should still reach the sequence comparison below.
	if imagehash.Distance(a.Difference, b.Difference) > 3*opts.MaxFrameDifference {
		return false
	}

	n := min(len(a.FrameDifference), len(b.FrameDifference))
	if n < opts.MinFrames {
		return false
	}
	var sumDiff, sumPerc int
	for i := range n {
		sumDiff += imagehash.Distance(a.FrameDifference[i], b.FrameDifference[i])
	}
	if sumDiff > opts.MaxFrameDifference*n {
		return false
	}
	m := min(len(a.FramePerceptual), len(b.FramePerceptual), n)
	for i := range m {
		sumPerc += imagehash.Distance(a.FramePerceptual[i], b.FramePerceptual[i])
	}
	return m == 0 || sumPerc <= opts.MaxFramePerceptual*m
}

// ratioWithin reports whether two positive numbers are within a ratio of each
// other. Zero on either side means "unknown", which is allowed through: an
// asset whose dimensions the metadata job could not read should be decided by
// its pixels rather than refused on a missing column.
func ratioWithin(a, b, maxRatio float64) bool {
	if a <= 0 || b <= 0 || maxRatio <= 0 {
		return true
	}
	return math.Max(a, b)/math.Min(a, b) <= maxRatio
}

// Rank orders a group best first — the copy that should be kept, and then the
// ones offered below it.
//
// Resolution, then bytes, then age. Which is to say: the biggest picture wins,
// because that is what "higher quality" means about two copies of one
// photograph and it is the only part of it a machine can see. Between two
// copies of the same size the larger file has been through less compression.
// Between two that are the same in both respects, the older one wins — it is
// the copy that has been in the archive longest, its neighbours in the timeline
// are already arranged around it, and it is the one whose capture time came
// from a source that had not yet had a chance to lose it.
//
// The id breaks the last tie so the order is total, and so a group offered
// today is offered the same way tomorrow.
func Rank(group []Signature) {
	slices.SortFunc(group, func(a, b Signature) int {
		if c := cmp.Compare(b.Pixels(), a.Pixels()); c != 0 {
			return c
		}
		if c := cmp.Compare(b.ByteSize, a.ByteSize); c != 0 {
			return c
		}
		if c := a.SortTime.Compare(b.SortTime); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

// unionFind is the standard disjoint-set forest, with both of the standard
// optimisations, because a group of forty duplicates is forty unions and the
// naive version is quadratic in exactly the case that motivated the feature.
type unionFind struct {
	parent []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	u := &unionFind{parent: make([]int, n), rank: make([]int, n)}
	for i := range u.parent {
		u.parent[i] = i
	}
	return u
}

func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]] // halving
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if u.rank[ra] < u.rank[rb] {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	if u.rank[ra] == u.rank[rb] {
		u.rank[ra]++
	}
}
