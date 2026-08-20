package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ranking, and the two rankings it is made of.
//
// ML_IMAGES.md §7: a query splits into a part a database can answer exactly and
// a part it cannot. The exact half is a TimelineFilter and is a WHERE clause
// over indexes that already existed. This file is the other half — one ranking
// by what the photographs look like, one by the words attached to them, fused.

// rrfK is the constant in reciprocal-rank fusion, and 60 is the number the
// method was published with.
//
// RRF rather than a weighted sum of the two scores, and that is the decision
// worth defending. Cosine similarity over unit vectors runs about 0.05 to 0.3
// on real queries and clusters tightly; ts_rank_cd runs from 0 to unbounded and
// depends on how many times a word appears in a caption. They are not on
// comparable scales, no monotonic rescaling makes them comparable, and tuning a
// weight between them is a job with no natural end — every query that improves
// makes another worse, and there is no held-out set to say which way is
// forwards.
//
// Fusing the *ranks* throws away the magnitudes on purpose. What survives is
// the only thing both lists agree on: that their first result is better than
// their tenth. k=60 flattens the top so that being second on both lists beats
// being first on one, which is exactly the behaviour wanted from two signals
// that fail in unrelated ways.
const rrfK = 60

// candidateDepth is how far down each ranking is read before fusing.
//
// Deep enough that a photograph the encoder ranks 200th and the text ranks 3rd
// still reaches the fusion, shallow enough that the ts_headline in the final
// select is not run over the archive. Both lists are already narrowed by the
// structured filter, so on a query naming a person this is usually the whole of
// what matches.
const candidateDepth = 400

// SearchRequest is a parsed query, ready to run: the exact half as a filter and
// the fuzzy half as the two things that rank it.
type SearchRequest struct {
	Filter TimelineFilter

	// Vector is the query phrase as the vision encoder sees it, or nil when
	// photo-ml is not there or the query had no visual half. Nil is a supported
	// way to run every search in this archive — it is what §7's degraded path
	// means, and what a purely structural query like "videos from 2019" needs
	// anyway.
	Vector []float32
	// Model must be the one that produced the stored vectors, and it must be
	// repeated literally: migration 0017's HNSW index is partial on
	// `where model = '...'` and is only reachable from a query that says so.
	// Leave it out and Postgres answers the same rows by sequential scan over
	// sixty thousand vectors — correct, and slow enough to look like a bug in
	// the model.
	Model string

	// Text is what goes to websearch_to_tsquery: the visual phrase, which is
	// also the half most likely to appear in a caption. Empty skips the
	// full-text ranking entirely.
	Text string

	Limit  int
	Offset int
}

// SearchResult is one ranked photograph, and why it is here.
//
// It embeds TimelineItem rather than restating it, so the grid draws a search
// result with the component it draws everything else with. The fields after it
// are the evidence — ML_IMAGES.md §8's "each tile can say why it matched", which
// with a free-form vocabulary is not a nicety: being able to see what the model
// called a photograph is what makes the tag cleanup possible at all.
type SearchResult struct {
	TimelineItem
	// Score is the fused rank. Comparable within one result set and meaningless
	// between two, which is why it is not called relevance.
	Score float64 `json:"score"`
	// Similarity is cosine similarity to the query phrase, 0 to 1, or absent
	// when there was no vector half. This one *is* comparable between queries,
	// and is the number to look at when a search feels wrong.
	Similarity *float64 `json:"similarity,omitempty"`
	// Caption is what the captioner wrote, unabridged.
	Caption string `json:"caption,omitempty"`
	// Tags are this asset's words, canonical, in confidence order.
	Tags []string `json:"tags,omitempty"`
	// Snippet is the matching stretch of recognised text with the match marked
	// by [] — the OCR line that explains why a screenshot is in these results.
	Snippet string `json:"snippet,omitempty"`
}

// Search runs the fused ranking and returns one page of it.
//
// Paged by offset rather than by cursor, unlike everything else here. A cursor
// is the sort key of the last row, and this ordering's sort key is a fused rank
// computed from the query — it does not exist on any row, cannot be compared
// against one, and would be invalidated by the next caption written. Relevance
// is also read differently from a timeline: nobody flings into page 400 of a
// search, they retype it.
//
// Offset asks one thing of an ordering that a cursor never has to: that it be
// the same ordering the next time it is computed. Every page re-runs the whole
// statement, so any two rows this query does not deliberately separate may come
// back in either order — and a pair that swaps across a page boundary is one
// photograph returned on both pages and another returned on neither. Every sort
// here therefore ends in a tiebreaker no two rows can share: `id` in the final
// order, `asset_id` inside both rankings' row_number. What that cannot make
// deterministic is which candidates the approximate vector scan found; see
// useSearch's accept for the guard on the other side of the wire.
func (s *Store) Search(ctx context.Context, req SearchRequest) ([]SearchResult, int, error) {
	if req.Limit <= 0 || req.Limit > 200 {
		req.Limit = 60
	}
	if req.Offset < 0 {
		req.Offset = 0
	}

	// $1..$6 are the two rankings' inputs and the page; the filter's own
	// arguments start after them.
	narrow, args, err := req.Filter.where(7)
	if err != nil {
		return nil, 0, err
	}
	var vector any
	if len(req.Vector) > 0 {
		vector = VectorLiteral(req.Vector)
	}
	var text any
	if req.Text != "" {
		text = req.Text
	}
	args = append([]any{vector, req.Model, text, candidateDepth, req.Limit, req.Offset}, args...)

	// One transaction, for one statement, for one setting.
	//
	// pgvector's HNSW index answers a nearest-neighbour query by walking a
	// graph, and it walks a fixed number of candidates before it stops. A
	// filtered search post-filters that walk — so "Phoenix at the beach",
	// against a person who is 9% of the library, can walk forty neighbours and
	// find that none of them are of Phoenix, and return nothing at all while
	// the photographs sit right there. iterative_scan is pgvector 0.8's answer:
	// keep resuming the walk until enough rows survive the filter. relaxed
	// rather than strict because the rows come back out of distance order and
	// this query re-sorts them anyway — and max_scan_tuples is set to more than
	// this archive contains, so the worst case is an exhaustive scan of 31,000
	// vectors, which is correct and takes tens of milliseconds.
	var results []SearchResult
	var total int
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if vector != nil {
			if _, err := tx.Exec(ctx, `set local hnsw.iterative_scan = relaxed_order`); err != nil {
				return fmt.Errorf("configure the vector index: %w", err)
			}
			if _, err := tx.Exec(ctx, `set local hnsw.max_scan_tuples = 100000`); err != nil {
				return fmt.Errorf("configure the vector index: %w", err)
			}
		}
		rows, err := tx.Query(ctx, searchSQL(req.Filter, narrow), args...)
		if err != nil {
			return fmt.Errorf("run search: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var r SearchResult
			var sortTime time.Time
			var caption, snippet *string
			if err := rows.Scan(
				&r.ID, &r.MediaKind, &sortTime, &r.OffsetMinutes,
				&r.State, &r.PlaybackState, &r.DurationSeconds, &r.LiveState,
				&r.Score, &r.Similarity, &caption, &r.Tags, &snippet, &total,
			); err != nil {
				return fmt.Errorf("scan search result: %w", err)
			}
			r.TakenAt = sortTime
			r.hideEmptyLiveState()
			r.Caption = text2(caption)
			r.Snippet = text2(snippet)
			results = append(results, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return results, total, nil
}

func text2(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// searchSQL builds the fused query around a filter fragment.
//
// The shape, from the inside out:
//
//	filtered   the structured half — one WHERE over the timeline's own scope
//	           and indexes, and the thing both rankings are cut to.
//	vec        nearest frames by cosine distance, collapsed to their asset by
//	           min(). A clip is as relevant as its best frame; averaging six
//	           frames of a video that goes from a beach to a restaurant into one
//	           number would make it neither.
//	fts        websearch_to_tsquery over the tsvector, ranked by ts_rank_cd.
//	fused      1/(k+rank) summed over whichever of the two produced a row.
//
// A query with no vector half runs the same statement with $1 null and the vec
// CTE empty, which is the degraded path from §7 with no second code path to
// maintain: photo-ml being down subtracts a ranking rather than a feature.
func searchSQL(filter TimelineFilter, narrow string) string {
	k := fmt.Sprint(rrfK)
	// Whether the structured half narrowed anything, baked in as a literal
	// rather than bound: it is part of the shape of the question, not a value
	// in it, and the two branches of `ranked` plan differently.
	fallback := "false"
	if narrow != "" {
		fallback = "true"
	}
	return `
with filtered as (
	select a.id, a.media_kind, a.sort_time, a.exif_offset_minutes,
	       a.derived_state, a.playback_state, a.duration_seconds
	from assets a
	where ` + filter.scope() + narrow + `
),
vec as (
	-- The tiebreaker is what makes this rank the same rank on the next page.
	-- Photographs at identical distance are not a corner case in an archive —
	-- a burst, a screenshot saved twice, one file imported from two phones —
	-- and without it their order is whatever the aggregate happened to emit,
	-- which moves everything below them by a place. See Search.
	select h.asset_id, min(h.distance) as distance,
	       row_number() over (order by min(h.distance), h.asset_id) as rank
	from (
		select e.asset_id, e.embedding <=> $1::halfvec as distance
		from asset_embeddings e
		join filtered f on f.id = e.asset_id
		where $1 is not null and e.model = $2
		order by e.embedding <=> $1::halfvec
		limit $4
	) h
	group by h.asset_id
),
fts as (
	select s.asset_id, q.query,
	       row_number() over (order by ts_rank_cd(s.tsv, q.query) desc, s.asset_id) as rank
	from asset_search s
	join filtered f on f.id = s.asset_id
	cross join lateral (select websearch_to_tsquery('english', $3::text) as query) q
	where $3 is not null and s.tsv @@ q.query
	order by ts_rank_cd(s.tsv, q.query) desc, s.asset_id
	limit $4
),
fused as (
	select coalesce(vec.asset_id, fts.asset_id) as asset_id,
	       (coalesce(1.0 / (` + k + ` + vec.rank), 0)
	      + coalesce(1.0 / (` + k + ` + fts.rank), 0))::float8 as score,
	       vec.distance,
	       fts.query
	from vec
	full outer join fts on fts.asset_id = vec.asset_id
),
-- The second branch is what a fused ranking of nothing falls back to, and it
-- covers two different situations that want the same answer.
--
-- "videos from 2019" has no fuzzy half at all: it is a timeline, and answering
-- it here rather than in Go keeps one statement and one page shape for both
-- kinds of question.
--
-- "phoenix at the beach last summer", with photo-ml down and no caption in the
-- library mentioning a beach, is the more interesting one. The exact half of
-- that query answered perfectly well — a person, a date range — and letting the
-- fuzzy half annihilate it would produce an empty grid for a question the
-- archive can partly answer. That is ML_IMAGES.md §11's failure exactly:
-- filtered out, silently, with nothing to argue with. So when the fusion is
-- empty and the filter is not, the filter's own answer stands, in date order.
--
-- Only when the filter is not empty. A search for a word nothing matches, over
-- the whole library, is genuinely no results, and answering it with 17,788
-- photographs in date order would be worse than saying so.
ranked as (
	select f.id, f.media_kind, f.sort_time, f.exif_offset_minutes,
	       f.derived_state, f.playback_state, f.duration_seconds,
	       fused.score,
	       fused.distance,
	       fused.query
	from filtered f
	join fused on fused.asset_id = f.id
	union all
	select f.id, f.media_kind, f.sort_time, f.exif_offset_minutes,
	       f.derived_state, f.playback_state, f.duration_seconds,
	       0::float8, null::float8, null::tsquery
	from filtered f
	where ` + fallback + ` and not exists (select 1 from fused)
),
counted as (select count(*)::int as total from ranked)
select r.id::text, r.media_kind, r.sort_time, r.exif_offset_minutes,
       r.derived_state, r.playback_state, r.duration_seconds,
       coalesce(live.live_state, 'none'),
       r.score,
       case when r.distance is null then null else 1 - r.distance end,
       d.caption,
       coalesce(t.names, '{}'::text[]),
       case when r.query is null then null
            else ts_headline('english', coalesce(o.text, ''), r.query,
                             'MaxFragments=1,MaxWords=14,MinWords=4,StartSel=[,StopSel=]') end,
       counted.total
from ranked r
cross join counted
left join asset_descriptions d on d.asset_id = r.id and d.model = '` + CaptionModel + `'
left join asset_ocr          o on o.asset_id = r.id and o.model = '` + OCRModel + `'
left join lateral (
	select array_agg(coalesce(canonical.name, tag.name) order by at.confidence desc nulls last) as names
	from asset_tags at
	join tags tag on tag.id = at.tag_id
	left join tags canonical on canonical.id = tag.canonical_id
	where at.asset_id = r.id
) t on true` + liveJoin("r") + `
order by r.score desc, r.sort_time desc, r.id desc
limit $5 offset $6`
}
