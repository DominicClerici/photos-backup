package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// The tag cleanup — ML_IMAGES.md §9, and the schema for it is migration 0019.
//
// §2 bought an open vocabulary by promising a cleanup later, and §11 called
// that "a bet on cleanup happening". This file is the cleanup, and it is two
// passes rather than the one §9 sketched, because running the sketch against
// the real vocabulary showed that the second pass is only worth doing after the
// first:
//
//	triage  is this word worth having at all? The captioner judges its own
//	        output, a person reviews the verdicts, and `tags.junk` is the answer
//	        in force. Roughly a third of a free-form vocabulary is interface
//	        text off screenshots and vague judgements about mood — words that
//	        merge into nothing, match nothing, and sit in the weight-A half of
//	        every tsvector they are attached to.
//	merge   which of the survivors are one word? The tag names are embedded in
//	        the encoder's own space, clustered, and proposed in groups.
//	        Accepting sets canonical_id.
//
// Nothing here destroys a row. asset_tags goes on recording exactly what the
// captioner wrote; `junk` and `canonical_id` are read at every point of use, so
// both passes take effect everywhere at once and are undone the same way.

// DefaultTagSimilarity is where a proposal starts, and it is high on purpose.
//
// Measured over this archive's own 2,936 words: SigLIP-2's text tower puts the
// *median* pair of unrelated tags at 0.73 cosine, so an absolute threshold that
// sounds generous is not. At 0.80 the clustering proposes "man ← woman" and
// "black ← white"; at 0.93 it proposes "mountains ← mountain, mountain range",
// "skiing ← ski, skier, skiers, skis", "outdoor ← outdoors", "sunglasses ← sun
// glasses" and "volkswagen ← vw", and the largest group it produces has ten
// members. That is the number this defaults to, and the review screen makes it
// a control rather than a constant because the right value moves with the
// vocabulary and is cheap to try: the vectors are stored, so re-clustering is
// one kNN query.
const DefaultTagSimilarity = 0.93

// tagNeighbours is how deep the kNN goes per word.
//
// A bound on group size rather than on quality: a proposal with more than two
// dozen members is not a merge anybody is going to read, and at the default
// threshold the largest real group is ten. It is also what keeps the clustering
// a graph walk instead of a self-join — see TagProposals.
const tagNeighbours = 24

// TagWord is one entry of the vocabulary, in the terms the cleanup is decided
// in: what the word is, how much of the archive it is attached to, and who has
// said what about it.
type TagWord struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Uses is how many photographs carry this word. It is the whole of the
	// stakes: junking a word used once costs nothing, and junking one used on
	// two hundred photographs is worth a second look, which is why both review
	// lists are ordered by it.
	Uses int64 `json:"uses"`

	Junk bool `json:"junk,omitempty"`
	// Score is what the captioner thought, 0 to 1. Advisory, and kept because a
	// confident wrong verdict is the one worth catching.
	Score *float32 `json:"score,omitempty"`
	// TriagedAt is when a model last judged this word; JudgedAt is when a
	// person did. The gap between them is ML_IMAGES.md §11's seam, and the
	// review screen draws it: a verdict nobody has confirmed looks different
	// from one somebody chose.
	TriagedAt *time.Time `json:"triaged_at,omitempty"`
	JudgedAt  *time.Time `json:"judged_at,omitempty"`

	// Canonical is the word this one has been folded into, on the merged list.
	Canonical string `json:"canonical,omitempty"`
	// Similarity is how near this word sits to the head of the proposal it is
	// in. Only ever set on a proposal's members, and shown because it is the
	// only evidence on the card that is not a photograph.
	Similarity float32 `json:"similarity,omitempty"`

	// Samples are a few photographs carrying this word, as ids. The evidence:
	// "does doggo mean the same as dog" is not a question about the two strings,
	// and three thumbnails answer it in the time it takes to read them.
	Samples []string `json:"samples,omitempty"`
}

// TagProposal is a group of words that mean one thing, with the one they should
// all be searched as at the head of it.
//
// The head is the most-used member and is chosen rather than voted on, for the
// reason merge.Rank picks a keeper: an archive that says "mountain" a hundred
// and eleven times and "mountain range" nine is telling you which word it
// speaks. It is overridable on the review screen, because that rule is right
// about ninety percent of the time and wrong in the interesting cases.
type TagProposal struct {
	Canonical TagWord   `json:"canonical"`
	Members   []TagWord `json:"members"`
	// Uses is the whole group's claims, which is what the merge is worth: it is
	// how many photographs stop being findable under one of several spellings.
	Uses int64 `json:"uses"`
}

// TagCounts is the cleanup in nine numbers: what the status card says, and what
// the review screen uses to know which stage it is in.
type TagCounts struct {
	Vocabulary int64 `json:"vocabulary"`
	Claims     int64 `json:"claims"`
	// Kept and Junk are the two review lists, and they do not add up to
	// Vocabulary: a word that has been folded into another is on neither.
	Kept int64 `json:"kept"`
	Junk int64 `json:"junk"`
	// Untriaged is words nothing has judged — no model, and no person either:
	// striking a word out before ever running the pass is an answer, and the
	// pass has nothing left to ask about it. Unreviewed is words a model has
	// judged and nobody has confirmed. The first is what the analyse pass has
	// left to do; the second is what approving is for.
	Untriaged  int64 `json:"untriaged"`
	Unreviewed int64 `json:"unreviewed"`
	// Unembedded is kept words with no vector for the current model, which is
	// what the clustering is missing.
	Unembedded int64 `json:"unembedded"`
	// Folded is words merged into another, and Groups is how many words they
	// were merged into. The merged list's two figures.
	Folded int64 `json:"folded"`
	Groups int64 `json:"groups"`
}

// TagCleanupCounts reads all nine. One statement of scalar subqueries, none of
// them touching more than the tags table and one aggregate over asset_tags.
func (s *Store) TagCleanupCounts(ctx context.Context) (TagCounts, error) {
	const query = `
		select
		    (select count(*) from tags),
		    (select count(*) from asset_tags),
		    (select count(*) from tags where not junk and canonical_id is null),
		    (select count(*) from tags where junk and canonical_id is null),
		    (select count(*) from tags where triaged_at is null and judged_at is null),
		    (select count(*) from tags where triaged_at is not null and judged_at is null),
		    (select count(*) from tags t
		      where not t.junk and t.canonical_id is null
		        and not exists (select 1 from tag_embeddings e
		                         where e.tag_id = t.id and e.model = $1)),
		    (select count(*) from tags where canonical_id is not null),
		    (select count(distinct canonical_id) from tags where canonical_id is not null)`

	var c TagCounts
	err := s.pool.QueryRow(ctx, query, VisionModel).Scan(
		&c.Vocabulary, &c.Claims, &c.Kept, &c.Junk,
		&c.Untriaged, &c.Unreviewed, &c.Unembedded, &c.Folded, &c.Groups)
	if err != nil {
		return c, fmt.Errorf("count the tag vocabulary: %w", err)
	}
	return c, nil
}

// UntriagedTags is the next slice of words for the captioner to judge.
//
// Bounded, and the caller is expected to come back for more. A whole vocabulary
// through a 4B model is a couple of minutes, which is too long to hold a browser
// request open and far too short to be worth a job kind, a pool and a reconcile
// — so it is a loop of bounded calls, each of which is a few seconds and none of
// which loses anything if the page is closed halfway. `triaged_at is null` is
// the resume point and it is an index.
//
// `judged_at is null` as well, because a word somebody has already struck out
// or kept is not waiting for anything. Without it such a word is a permanent
// resident of this query: it has no triage stamp, so it is selected on every
// pass; PutTriage refuses to overrule the person who judged it, so it is never
// stamped; and the count the review screen loops on never reaches zero. The
// captioner is asked about it once per pass, forever, to have the answer thrown
// away — and the progress bar stops short of its own total.
//
// Most-used first, so a pass that is interrupted has judged the words that
// matter rather than an arbitrary third of the tail.
func (s *Store) UntriagedTags(ctx context.Context, limit int) ([]TagWord, error) {
	const query = `
		select t.id, t.name, count(at.asset_id)
		from tags t
		left join asset_tags at on at.tag_id = t.id
		where t.triaged_at is null and t.judged_at is null
		group by t.id, t.name
		order by count(at.asset_id) desc, t.name
		limit $1`
	return s.readWords(ctx, "read the words waiting to be judged", query, limit)
}

// TagVerdict is one word, as a model saw it.
type TagVerdict struct {
	ID    int64
	Junk  bool
	Score float32
}

// PutTriage records a pass, and refuses to overrule a person.
//
// `where judged_at is null` is the whole of that refusal, and it is what makes
// re-running the triage safe on a vocabulary that has grown: a second pass over
// six thousand words re-judges the three thousand nobody has looked at and
// leaves every answer somebody gave. ML_IMAGES.md §11's seam, in a where clause.
//
// It does not rebuild asset_search, and that is the one asymmetry in this file
// worth stating plainly. A single word being judged rebuilds the photographs
// carrying it — see JudgeTags — because that is a handful of rows. A bulk pass
// would rebuild most of the library once per chunk, fifteen times over, to
// arrive at the state one rebuild produces; so the bulk operation rebuilds
// once, at the end, when the pass is approved. See ApproveTriage.
func (s *Store) PutTriage(ctx context.Context, verdicts []TagVerdict) (int64, error) {
	if len(verdicts) == 0 {
		return 0, nil
	}
	ids := make([]int64, len(verdicts))
	junk := make([]bool, len(verdicts))
	scores := make([]float32, len(verdicts))
	for i, v := range verdicts {
		ids[i], junk[i], scores[i] = v.ID, v.Junk, v.Score
	}

	var applied int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		const update = `
			update tags t
			set junk = v.junk, junk_score = v.score, triaged_at = now()
			from unnest($1::bigint[], $2::bool[], $3::real[]) as v (id, junk, score)
			where t.id = v.id and t.judged_at is null`
		tag, err := tx.Exec(ctx, update, ids, junk, scores)
		if err != nil {
			return fmt.Errorf("record the triage verdicts: %w", err)
		}
		applied = tag.RowsAffected()
		// A word just called junk has no business in the clustering, and its
		// vector has no business in the kNN graph the clustering walks. See
		// dropEmbeddings.
		return dropEmbeddings(ctx, tx, ids)
	})
	return applied, err
}

// JudgeTags is a person disagreeing, one word at a time — or agreeing, which is
// the same write.
//
// This is the only thing in the feature that sets judged_at on a word somebody
// actually looked at, and from here no triage pass will touch it again.
func (s *Store) JudgeTags(ctx context.Context, ids []int64, junk bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var changed int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		const update = `
			update tags set junk = $2, judged_at = now()
			where id = any ($1::bigint[])`
		tag, err := tx.Exec(ctx, update, ids, junk)
		if err != nil {
			return fmt.Errorf("record a judgement on %d words: %w", len(ids), err)
		}
		changed = tag.RowsAffected()

		if junk {
			if err := dropEmbeddings(ctx, tx, ids); err != nil {
				return err
			}
		}
		// A handful of words, so a handful of photographs: cheap enough to keep
		// the index true at the moment the button is pressed rather than at the
		// next time somebody remembers to run `photobackup ml reindex`, which
		// is the bet ML_IMAGES.md §11 names and this is how it stops being one.
		return refreshForTags(ctx, tx, ids)
	})
	return changed, err
}

// ApproveTriage is somebody saying they have read the two lists.
//
// It changes no verdict. What it changes is whose verdict each one is: every
// word a model judged and nobody has contradicted becomes a word this archive's
// owner stands behind, and no later pass will revisit it. That is a real claim
// and it is deliberately one button — the alternative is a vocabulary where the
// distinction between "confirmed" and "nobody has looked" decays into noise
// after the first pass, which is the same distinction asset_people exists to
// protect for names.
//
// It is also the moment the search index is made true, over the whole library
// in one call. Seconds over this archive, and the bulk counterpart to the
// per-word rebuild in JudgeTags.
func (s *Store) ApproveTriage(ctx context.Context) (approved int64, reindexed int64, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		const stamp = `
			update tags set judged_at = now()
			where triaged_at is not null and judged_at is null`
		tag, err := tx.Exec(ctx, stamp)
		if err != nil {
			return fmt.Errorf("approve the triage: %w", err)
		}
		approved = tag.RowsAffected()

		if err := tx.QueryRow(ctx,
			`select rebuild_asset_search(null, $1, $2)`,
			CaptionModel, OCRModel).Scan(&reindexed); err != nil {
			return fmt.Errorf("rebuild the search index: %w", err)
		}
		return nil
	})
	return approved, reindexed, err
}

// UnembeddedTags is the next slice of words for the encoder.
//
// Only words in play: junk is excluded because it will never be clustered, and
// a word already folded into another is excluded because it has already been
// answered. Bounded and resumable for the reason UntriagedTags is, though this
// pass is far cheaper — seven seconds for a whole vocabulary against a warm
// encoder, against a couple of minutes for the triage.
func (s *Store) UnembeddedTags(ctx context.Context, limit int) ([]TagWord, error) {
	const query = `
		select t.id, t.name, count(at.asset_id)
		from tags t
		left join asset_tags at on at.tag_id = t.id
		where not t.junk and t.canonical_id is null
		  and not exists (select 1 from tag_embeddings e
		                   where e.tag_id = t.id and e.model = $2)
		group by t.id, t.name
		order by count(at.asset_id) desc, t.name
		limit $1`

	rows, err := s.pool.Query(ctx, query, limit, VisionModel)
	if err != nil {
		return nil, fmt.Errorf("read the words waiting to be embedded: %w", err)
	}
	return scanWords(rows, "read the words waiting to be embedded")
}

// PutTagEmbeddings stores what the text tower made of a batch of words.
//
// Upsert rather than the replace-the-set that PutEmbeddings does, because a word
// is one row and there is no set: re-embedding "dog" under the same model
// replaces one vector, and under a different model adds one beside it.
func (s *Store) PutTagEmbeddings(ctx context.Context, model string, ids []int64, vectors [][]float32) error {
	if len(ids) != len(vectors) {
		return fmt.Errorf("%d words and %d vectors", len(ids), len(vectors))
	}
	if len(ids) == 0 {
		return nil
	}
	literals := make([]string, len(vectors))
	for i, v := range vectors {
		if len(v) != VisionDim {
			return fmt.Errorf("the vector for tag %d has %d dimensions, want %d", ids[i], len(v), VisionDim)
		}
		literals[i] = VectorLiteral(v)
	}

	const insert = `
		insert into tag_embeddings (tag_id, model, embedding, embedded_at)
		select v.id, $2, v.vec::halfvec, now()
		from unnest($1::bigint[], $3::text[]) as v (id, vec)
		join tags t on t.id = v.id
		on conflict (tag_id, model)
		do update set embedding = excluded.embedding, embedded_at = excluded.embedded_at`
	if _, err := s.pool.Exec(ctx, insert, ids, model, literals); err != nil {
		return fmt.Errorf("store %d tag vectors: %w", len(ids), err)
	}
	return nil
}

// dropEmbeddings takes words out of the kNN graph.
//
// Called whenever a word stops being a candidate — junked, or folded into
// another — and it is what keeps TagProposals' query honest. The alternative is
// filtering junk out after the neighbour search, which sounds equivalent and is
// not: a kNN asked for the twenty-four nearest words returns twenty-four, and
// if eight of them are junk then eight of a group's slots were spent on words
// that could never be proposed. Deleting means tag_embeddings holds exactly the
// words that are still a question, and re-embedding one is a call to the
// encoder that costs milliseconds if it ever becomes one again.
func dropEmbeddings(ctx context.Context, tx pgx.Tx, ids []int64) error {
	const clear = `
		delete from tag_embeddings e
		using tags t
		where t.id = e.tag_id and e.tag_id = any ($1::bigint[])
		  and (t.junk or t.canonical_id is not null)`
	if _, err := tx.Exec(ctx, clear, ids); err != nil {
		return fmt.Errorf("drop tag vectors: %w", err)
	}
	return nil
}

// refreshForTags rebuilds the search rows of every photograph carrying one of
// these words.
//
// The obligation ML_IMAGES.md §11 warned would have to be remembered — "a tag
// merge leaves every row already written out of date" — discharged inside the
// transaction that creates it. One statement: the affected assets are an index
// lookup on asset_tags, and coalescing to an empty array rather than null is
// deliberate, since null means "the whole library" to the function.
func refreshForTags(ctx context.Context, tx pgx.Tx, tagIDs []int64) error {
	const refresh = `
		select rebuild_asset_search(
		    (select coalesce(array_agg(distinct asset_id), '{}'::uuid[])
		       from asset_tags where tag_id = any ($1::bigint[])),
		    $2, $3)`
	if _, err := tx.Exec(ctx, refresh, tagIDs, CaptionModel, OCRModel); err != nil {
		return fmt.Errorf("rebuild the search index: %w", err)
	}
	return nil
}

func (s *Store) readWords(ctx context.Context, what, query string, args ...any) ([]TagWord, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return scanWords(rows, what)
}

func scanWords(rows pgx.Rows, what string) ([]TagWord, error) {
	defer rows.Close()
	out := []TagWord{}
	for rows.Next() {
		var w TagWord
		if err := rows.Scan(&w.ID, &w.Name, &w.Uses); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// TagWordQuery is one page of one of the two review lists.
type TagWordQuery struct {
	// Junk picks the list: the words in force as junk, or the words kept. They
	// do not add up to the vocabulary — a word already folded into another is
	// on neither, because it has been answered.
	Junk bool
	// Search narrows by substring, which is how somebody looks for a word they
	// remember seeing rather than scrolls to it.
	Search string
	Limit  int
	Offset int
	// Samples is how many photographs to attach per word, 0 for none.
	Samples int
}

// TagWords reads one page of a review list, most-used first, with the total
// behind it.
//
// Most-used first because that is the order the mistakes are worth catching in.
// A model that junks "screenshot" — 132 photographs — has done something worth
// undoing; a model that junks a word used once has probably done something
// right, and if it has not, nothing much happened.
func (s *Store) TagWords(ctx context.Context, q TagWordQuery) ([]TagWord, int64, error) {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	const query = `
		select t.id, t.name, count(at.asset_id), t.junk, t.junk_score,
		       t.triaged_at, t.judged_at,
		       count(*) over () as matched
		from tags t
		left join asset_tags at on at.tag_id = t.id
		where t.canonical_id is null and t.junk = $1
		  and ($2 = '' or t.name like '%' || $2 || '%')
		group by t.id, t.name
		order by count(at.asset_id) desc, t.name
		limit $3 offset $4`

	rows, err := s.pool.Query(ctx, query, q.Junk, q.Search, q.Limit, q.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("read the tag vocabulary: %w", err)
	}
	defer rows.Close()

	out := []TagWord{}
	var matched int64
	for rows.Next() {
		var w TagWord
		if err := rows.Scan(&w.ID, &w.Name, &w.Uses, &w.Junk, &w.Score,
			&w.TriagedAt, &w.JudgedAt, &matched); err != nil {
			return nil, 0, fmt.Errorf("scan a tag: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read the tag vocabulary: %w", err)
	}
	if err := s.attachSamples(ctx, out, q.Samples); err != nil {
		return nil, 0, err
	}
	return out, matched, nil
}

// attachSamples hangs a few photographs off each word.
//
// One query for the whole page rather than one per word, and only for the
// photographs that actually have a thumbnail to draw: a sample the grid renders
// as a grey square is worse than one fewer sample.
func (s *Store) attachSamples(ctx context.Context, words []TagWord, per int) error {
	if per <= 0 || len(words) == 0 {
		return nil
	}
	ids := make([]int64, len(words))
	for i, w := range words {
		ids[i] = w.ID
	}

	const query = `
		select w.id, s.asset_id
		from unnest($1::bigint[]) as w (id)
		cross join lateral (
		    select at.asset_id
		    from asset_tags at
		    join assets a on a.id = at.asset_id
		    where at.tag_id = w.id
		      and a.vault = '' and a.deleted_at is null and a.derived_state = 'ready'
		    order by at.confidence desc nulls last, a.sort_time desc
		    limit $2
		) s`

	rows, err := s.pool.Query(ctx, query, ids, per)
	if err != nil {
		return fmt.Errorf("read sample photographs for %d words: %w", len(words), err)
	}
	defer rows.Close()

	byTag := map[int64][]string{}
	for rows.Next() {
		var tag int64
		var asset string
		if err := rows.Scan(&tag, &asset); err != nil {
			return fmt.Errorf("scan a sample photograph: %w", err)
		}
		byTag[tag] = append(byTag[tag], asset)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read sample photographs: %w", err)
	}
	for i := range words {
		words[i].Samples = byTag[words[i].ID]
	}
	return nil
}

// TagProposalQuery is one clustering run.
type TagProposalQuery struct {
	// Similarity is the cosine floor for two words to be proposed as one. See
	// DefaultTagSimilarity for why the useful range is narrow and high.
	Similarity float64
	Limit      int
	Samples    int
}

// TagProposals clusters the vocabulary and returns the groups it found.
//
// Three steps, and the division of labour between them is the point.
//
// Postgres finds the neighbours. The vectors are already there, halfvec
// distance is SIMD in C, and migration 0019's HNSW index turns what would be a
// self-join into a graph walk: measured over 3,000 words, 7.7 seconds becomes
// 240ms. That difference is what makes the similarity a control on the review
// screen rather than a constant in this file.
//
// Go does the grouping, because the rule is not a distance. Words are taken
// most-used first and each one becomes the head of a group of its unclaimed
// neighbours — leader clustering, and it is chosen over the obvious union-find
// for one reason: single linkage *chains*. With "dog" near "puppy", "puppy"
// near "kitten" and "kitten" near "cat", union-find produces one group
// containing both a dog and a cat, each link individually defensible. Requiring
// every member to be near the head is what stops that, and taking heads in
// order of use is what makes the head the word the archive actually speaks.
//
// A word is claimed the moment it is *considered* as a head, whether or not it
// found anybody, which is what guarantees the direction of every merge: a word
// can only ever be folded into one used at least as much as itself.
func (s *Store) TagProposals(ctx context.Context, q TagProposalQuery) ([]TagProposal, error) {
	if q.Similarity <= 0 {
		q.Similarity = DefaultTagSimilarity
	}

	words, err := s.candidateWords(ctx)
	if err != nil {
		return nil, err
	}
	edges, err := s.tagNeighbours(ctx, q.Similarity)
	if err != nil {
		return nil, err
	}

	byID := make(map[int64]int, len(words))
	for i, w := range words {
		byID[w.ID] = i
	}

	claimed := make(map[int64]bool, len(words))
	out := []TagProposal{}
	for _, head := range words {
		if claimed[head.ID] {
			continue
		}
		claimed[head.ID] = true

		var members []TagWord
		for _, edge := range edges[head.ID] {
			if claimed[edge.other] {
				continue
			}
			i, ok := byID[edge.other]
			if !ok {
				continue
			}
			member := words[i]
			member.Similarity = edge.similarity
			members = append(members, member)
			claimed[edge.other] = true
		}
		if len(members) == 0 {
			continue
		}
		// Nearest first: the obvious merges are read and accepted, and the one
		// that made somebody pause is at the bottom where it can be unticked.
		sort.SliceStable(members, func(a, b int) bool {
			return members[a].Similarity > members[b].Similarity
		})

		group := TagProposal{Canonical: head, Members: members, Uses: head.Uses}
		for _, m := range members {
			group.Uses += m.Uses
		}
		out = append(out, group)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, s.attachProposalSamples(ctx, out, q.Samples)
}

// candidateWords is every word still in play, most-used first.
//
// "Still in play" is exactly "has a vector", because dropEmbeddings takes the
// vector away the moment a word is junked or folded. That is why this joins
// tag_embeddings rather than filtering tags: one definition of the candidate
// set, held in one place, and the kNN below walks the same rows.
func (s *Store) candidateWords(ctx context.Context) ([]TagWord, error) {
	const query = `
		select t.id, t.name, count(at.asset_id)
		from tags t
		join tag_embeddings e on e.tag_id = t.id and e.model = $1
		left join asset_tags at on at.tag_id = t.id
		group by t.id, t.name
		order by count(at.asset_id) desc, t.name`

	rows, err := s.pool.Query(ctx, query, VisionModel)
	if err != nil {
		return nil, fmt.Errorf("read the words to cluster: %w", err)
	}
	return scanWords(rows, "read the words to cluster")
}

type tagEdge struct {
	other      int64
	similarity float32
}

// tagNeighbours is the kNN, as an adjacency list.
//
// The model is spelled into the SQL rather than bound as a parameter, and it has
// to be: migration 0019's index is partial on `where model = '...'`, and a
// partial index is only reachable from a query that repeats its predicate
// literally. Bound as $1 it would still be correct and would fall back to a
// sequential scan per word, which over a few thousand words is the seven-second
// self-join this index exists to avoid. Same rule, same constant, as
// db.VisionModel in Search.
//
// Symmetric by construction: cosine is, and the pair is recorded from both ends
// so a group is not lost to one side's kNN being full. Anything two people have
// already said is not one word is left out here rather than filtered later, so
// a rejection actually removes the proposal instead of hiding it.
func (s *Store) tagNeighbours(ctx context.Context, similarity float64) (map[int64][]tagEdge, error) {
	query := `
		select a.tag_id, n.tag_id, (1 - (a.embedding <=> n.embedding))::real as similarity
		from tag_embeddings a
		cross join lateral (
		    select e.tag_id, e.embedding
		    from tag_embeddings e
		    where e.model = '` + VisionModel + `'
		    order by e.embedding <=> a.embedding
		    limit $1
		) n
		where a.model = '` + VisionModel + `'
		  and n.tag_id <> a.tag_id
		  and (1 - (a.embedding <=> n.embedding)) >= $2
		  and not exists (
		      select 1 from tag_merge_blocks b
		      where b.tag_id   = least(a.tag_id, n.tag_id)
		        and b.other_id = greatest(a.tag_id, n.tag_id))`

	// One more than the group bound, because the nearest neighbour of every
	// word is itself and the row is dropped above.
	rows, err := s.pool.Query(ctx, query, tagNeighbours+1, similarity)
	if err != nil {
		return nil, fmt.Errorf("find the words near each other: %w", err)
	}
	defer rows.Close()

	edges := map[int64][]tagEdge{}
	seen := map[[2]int64]bool{}
	for rows.Next() {
		var from, to int64
		var similarity float32
		if err := rows.Scan(&from, &to, &similarity); err != nil {
			return nil, fmt.Errorf("scan a pair of words: %w", err)
		}
		for _, pair := range [][2]int64{{from, to}, {to, from}} {
			if seen[pair] {
				continue
			}
			seen[pair] = true
			edges[pair[0]] = append(edges[pair[0]], tagEdge{other: pair[1], similarity: similarity})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find the words near each other: %w", err)
	}
	// Nearest first, so a head claims the words it is surest about before a
	// later head can take them.
	for id := range edges {
		list := edges[id]
		sort.SliceStable(list, func(a, b int) bool { return list[a].similarity > list[b].similarity })
	}
	return edges, nil
}

// attachProposalSamples fetches thumbnails for every word on the page at once.
func (s *Store) attachProposalSamples(ctx context.Context, groups []TagProposal, per int) error {
	if per <= 0 || len(groups) == 0 {
		return nil
	}
	flat := make([]TagWord, 0, len(groups)*3)
	for _, g := range groups {
		flat = append(flat, g.Canonical)
		flat = append(flat, g.Members...)
	}
	if err := s.attachSamples(ctx, flat, per); err != nil {
		return err
	}
	i := 0
	for g := range groups {
		groups[g].Canonical.Samples = flat[i].Samples
		i++
		for m := range groups[g].Members {
			groups[g].Members[m].Samples = flat[i].Samples
			i++
		}
	}
	return nil
}

// ErrNoSuchTag means a word named in a request is not in the vocabulary — a
// stale review page, usually, asking about a word a rebuild has since replaced.
var ErrNoSuchTag = errors.New("db: no such word")

// ErrTagFolded means a word offered as the head of a merge has itself already
// been folded into another. Refused rather than followed, because canonical_id
// is resolved one hop everywhere it is read — see rebuild_asset_search — and a
// chain would silently stop resolving at the first link.
var ErrTagFolded = errors.New("db: that word has already been merged into another")

// TagMerge is what a merge did.
type TagMerge struct {
	Canonical string `json:"canonical"`
	Merged    int64  `json:"merged"`
	// Rejected is how many pairs this merge also recorded a "no" for: the
	// members somebody unticked before accepting the rest. Without them the
	// next clustering run proposes exactly the group that was just corrected.
	Rejected int64 `json:"rejected,omitempty"`
	// Reindexed is how many photographs had their search row rewritten, which
	// is the number ML_IMAGES.md §11 says somebody would otherwise have had to
	// remember to produce by hand.
	Reindexed int64 `json:"reindexed"`
}

// MergeTags folds a group of words into one, and records the ones somebody
// took out of it.
//
// One column, one transaction, and no row destroyed: asset_tags goes on saying
// this photograph was called "puppy", and every read resolves that to "dog"
// through canonical_id. Undone by clearing the same column — see UnmergeTags.
//
// Three things happen besides the obvious one.
//
// A member that was itself the head of an earlier merge brings its children
// with it. canonical_id is resolved exactly one hop wherever it is read, so
// leaving "doggo → puppy" alone while pointing "puppy → dog" would leave
// "doggo" resolving to a word that is no longer canonical: findable as neither.
//
// The rejected members are written down as pairs that are not the same word,
// which is the only thing that makes unticking one durable.
//
// And the merged words lose their vectors, because they have stopped being a
// question. See dropEmbeddings.
func (s *Store) MergeTags(ctx context.Context, canonical int64, members, rejected []int64) (TagMerge, error) {
	var out TagMerge
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var folded bool
		err := tx.QueryRow(ctx,
			`select name, canonical_id is not null from tags where id = $1`, canonical).
			Scan(&out.Canonical, &folded)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNoSuchTag
		case err != nil:
			return fmt.Errorf("read the word being merged into: %w", err)
		case folded:
			return ErrTagFolded
		}

		const fold = `
			update tags
			set canonical_id = $1
			where id <> $1
			  and (id = any ($2::bigint[]) or canonical_id = any ($2::bigint[]))`
		tag, err := tx.Exec(ctx, fold, canonical, members)
		if err != nil {
			return fmt.Errorf("merge %d words into %q: %w", len(members), out.Canonical, err)
		}
		out.Merged = tag.RowsAffected()

		const block = `
			insert into tag_merge_blocks (tag_id, other_id)
			select least($1, r.id), greatest($1, r.id)
			from unnest($2::bigint[]) as r (id)
			where r.id <> $1
			on conflict do nothing`
		tag, err = tx.Exec(ctx, block, canonical, rejected)
		if err != nil {
			return fmt.Errorf("record the words left out of a merge: %w", err)
		}
		out.Rejected = tag.RowsAffected()

		if err := dropEmbeddings(ctx, tx, members); err != nil {
			return err
		}

		// Both ends: the photographs carrying a folded word are now findable as
		// the canonical one, and the head's own rows are rewritten too because
		// string_agg(distinct ...) over a group that gained a spelling is a
		// different vector.
		touched := append(append([]int64{canonical}, members...), rejected...)
		if err := refreshForTags(ctx, tx, touched); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			select count(distinct asset_id) from asset_tags
			where tag_id = any ($1::bigint[])`, touched).Scan(&out.Reindexed)
	})
	return out, err
}

// UnmergeTags puts words back, which is the whole reason the merge is a column.
//
// The vector is not restored here and does not need to be: the word becomes a
// candidate again the moment canonical_id is null, and the next scan embeds it.
// Restoring it would mean keeping vectors for words that are not candidates,
// which is exactly what dropEmbeddings exists to avoid.
func (s *Store) UnmergeTags(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var restored int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		const clear = `
			update tags set canonical_id = null
			where id = any ($1::bigint[]) and canonical_id is not null`
		tag, err := tx.Exec(ctx, clear, ids)
		if err != nil {
			return fmt.Errorf("undo a tag merge: %w", err)
		}
		restored = tag.RowsAffected()
		return refreshForTags(ctx, tx, ids)
	})
	return restored, err
}

// DismissTagProposal records that a group of words are not one word.
//
// Every pair in the group, so that the next clustering run cannot re-propose
// any part of it — including the sub-group it would find after the most-used
// member had been taken out. A rejection nobody wrote down is a rejection that
// arrives again on the next scan, which is the whole reason merge_groups has a
// `dismissed` state and this table exists.
func (s *Store) DismissTagProposal(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) < 2 {
		return 0, nil
	}
	const block = `
		insert into tag_merge_blocks (tag_id, other_id)
		select least(a.id, b.id), greatest(a.id, b.id)
		from unnest($1::bigint[]) as a (id), unnest($1::bigint[]) as b (id)
		where a.id < b.id
		on conflict do nothing`
	tag, err := s.pool.Exec(ctx, block, ids)
	if err != nil {
		return 0, fmt.Errorf("record that %d words are not one word: %w", len(ids), err)
	}
	return tag.RowsAffected(), nil
}

// MergedTags is the log: every word that has been folded, under the word it was
// folded into.
//
// The same shape as a proposal on purpose, so the review screen draws one card
// component for both. The difference is what the card offers — a proposal has
// "merge" and a merged group has "undo" — which is exactly the difference
// between the duplicates tab and the joined-recordings tab on the merge review,
// and for the same reason: one is a question and the other is a log of answers.
func (s *Store) MergedTags(ctx context.Context, limit int) ([]TagProposal, error) {
	if limit <= 0 {
		limit = 200
	}
	const query = `
		select head.id, head.name,
		       (select count(*) from asset_tags where tag_id = head.id),
		       t.id, t.name,
		       (select count(*) from asset_tags where tag_id = t.id)
		from tags t
		join tags head on head.id = t.canonical_id
		where t.canonical_id is not null
		order by head.name, t.name`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read the merged words: %w", err)
	}
	defer rows.Close()

	out := []TagProposal{}
	at := map[int64]int{}
	for rows.Next() {
		var head, member TagWord
		if err := rows.Scan(&head.ID, &head.Name, &head.Uses,
			&member.ID, &member.Name, &member.Uses); err != nil {
			return nil, fmt.Errorf("scan a merged word: %w", err)
		}
		member.Canonical = head.Name
		i, ok := at[head.ID]
		if !ok {
			i = len(out)
			at[head.ID] = i
			out = append(out, TagProposal{Canonical: head, Uses: head.Uses})
		}
		out[i].Members = append(out[i].Members, member)
		out[i].Uses += member.Uses
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the merged words: %w", err)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
