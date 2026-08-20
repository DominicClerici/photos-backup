package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// The two models that put a photograph into words, named the way VisionModel is
// and for the same three reasons: the name is stored on every row it produces,
// it is what the reconcile compares against to decide whether work is owed, and
// it is ours rather than the checkpoint's — the row records what described a
// photograph and will outlive whatever the weights were called on whichever
// mirror they came from.
//
// Changing either string is a model swap, and a model swap is a data operation:
//
//	delete from asset_descriptions where model = 'qwen3-vl-4b-instruct';
//
// then `photobackup ml backfill`, which finds every asset the new name has said
// nothing about. Never a migration and never a truncate, and the old and the new
// can sit in the tables together while somebody reads both.
//
// rebuild_asset_search takes them as arguments for exactly that reason. The one
// place they are written down as literals is migration 0018's initial fill,
// where both tables were empty and the value could not have mattered.
const (
	CaptionModel = "qwen3-vl-4b-instruct"
	OCRModel     = "rapidocr"
)

// Tag is one word a model wrote about a photograph, and how sure it was.
//
// Free-form on purpose — ML_IMAGES.md §2 chose an open vocabulary over a list
// guessed in advance and then found to be missing the half of the archive that
// is Snapchat. Expect three to six thousand distinct strings, and see
// tags.canonical_id for the cleanup that makes that affordable.
type Tag struct {
	Name string
	// Confidence is what the captioner reported, or 0 where it reported
	// nothing. Stored rather than thresholded here, because the threshold is a
	// ranking decision and this is the write path.
	Confidence float32
}

// PutDescription replaces everything a captioner has said about an asset: the
// sentence, the words, and the search row that is built from both.
//
// One transaction, because the three are one statement about the photograph. A
// run that wrote tags and died before the caption would leave an asset findable
// by "dog" and unable to say why, with a job marked done behind it — the same
// half-published failure clipRenditions commits its frames together to avoid.
//
// Replace rather than upsert, for the reason PutEmbeddings replaces: the *set*
// is part of the answer. A second run that no longer sees a dog has said the dog
// is not there, and an upsert would leave the old tag behind as a claim nothing
// still makes.
//
// The vault check is repeated here even though the worker made it, because the
// two are separated by however long a GPU call took and a photograph can be
// hidden in between. A caption is the most legible description of a photograph
// this server ever produces — considerably more so than a thumbnail, and
// entirely more so than 1152 numbers — so writing one for something in the vault
// would be this server recording in plain English the thing the vault exists to
// stop it knowing. Same guard, same reason, as PutEmbeddings and ApplyPlaces.
func (s *Store) PutDescription(ctx context.Context, assetID, model, caption string, tags []Tag) error {
	names, confidences := normalizeTags(tags)

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The join against assets is the vault guard, and it is also what makes
		// this a no-op rather than a foreign-key error for an asset purged
		// between the GPU call and the write.
		const describe = `
			insert into asset_descriptions (asset_id, model, caption, generated_at)
			select a.id, $2, $3, now()
			from assets a
			where a.id = $1::uuid and a.vault = '' and a.deleted_at is null
			on conflict (asset_id, model)
			do update set caption = excluded.caption, generated_at = excluded.generated_at`
		if _, err := tx.Exec(ctx, describe, assetID, model, caption); err != nil {
			return fmt.Errorf("store caption for %s: %w", assetID, err)
		}

		if err := putTags(ctx, tx, assetID, names, confidences); err != nil {
			return err
		}
		return refresh(ctx, tx, []string{assetID})
	})
}

// PutOCR replaces what the text recogniser read, and rebuilds the search row.
func (s *Store) PutOCR(ctx context.Context, assetID, model, text string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		const store = `
			insert into asset_ocr (asset_id, model, text, generated_at)
			select a.id, $2, $3, now()
			from assets a
			where a.id = $1::uuid and a.vault = '' and a.deleted_at is null
			on conflict (asset_id, model)
			do update set text = excluded.text, generated_at = excluded.generated_at`
		if _, err := tx.Exec(ctx, store, assetID, model, text); err != nil {
			return fmt.Errorf("store recognised text for %s: %w", assetID, err)
		}
		return refresh(ctx, tx, []string{assetID})
	})
}

// putTags interns the words and points the asset at them.
//
// Two statements rather than one because `tags` is the shared vocabulary and
// `asset_tags` is this asset's claim on it. Interning first, with `do nothing`,
// means two describe jobs writing "dog" at the same moment produce one tag row
// and no error — the unique index arbitrates and neither worker has to know the
// other exists.
//
// Nothing here consults canonical_id, and that is the design rather than an
// omission. asset_tags records what the model actually wrote; the merge is
// resolved on the way *out*, in rebuild_asset_search and in the search query.
// Resolving on the way in would bake a merge into rows that then could not be
// unmerged, which is precisely the reversibility canonical_id exists to give.
func putTags(ctx context.Context, tx pgx.Tx, assetID string, names []string, confidences []float32) error {
	const clear = `delete from asset_tags where asset_id = $1::uuid`
	if _, err := tx.Exec(ctx, clear, assetID); err != nil {
		return fmt.Errorf("clear tags for %s: %w", assetID, err)
	}
	if len(names) == 0 {
		return nil
	}

	const intern = `
		insert into tags (name)
		select distinct unnest($1::text[])
		on conflict (name) do nothing`
	if _, err := tx.Exec(ctx, intern, names); err != nil {
		return fmt.Errorf("intern tag names: %w", err)
	}

	const link = `
		insert into asset_tags (asset_id, tag_id, confidence)
		select a.id, t.id, v.confidence
		from assets a
		join unnest($2::text[], $3::real[]) as v (name, confidence) on true
		join tags t on t.name = v.name
		where a.id = $1::uuid and a.vault = '' and a.deleted_at is null
		on conflict (asset_id, tag_id) do update set confidence = excluded.confidence`
	if _, err := tx.Exec(ctx, link, assetID, names, confidences); err != nil {
		return fmt.Errorf("tag %s: %w", assetID, err)
	}
	return nil
}

// RefreshSearch rebuilds the tsvector for specific assets, or for the whole
// library when given nothing.
//
// Called by the two ML jobs after they write, and by `photobackup ml reindex`
// after a merge or a re-geocode. It is the only way anything outside the
// database writes to asset_search: the recipe is a function in migration 0018,
// because what it produces is stored and a changed recipe means every stored row
// is stale.
func (s *Store) RefreshSearch(ctx context.Context, assetIDs ...string) (int64, error) {
	var ids any
	if len(assetIDs) > 0 {
		ids = assetIDs
	}
	var touched int64
	err := s.pool.QueryRow(ctx,
		`select rebuild_asset_search($1::uuid[], $2, $3)`,
		ids, CaptionModel, OCRModel).Scan(&touched)
	if err != nil {
		return 0, fmt.Errorf("rebuild the search index: %w", err)
	}
	return touched, nil
}

func refresh(ctx context.Context, tx pgx.Tx, ids []string) error {
	_, err := tx.Exec(ctx,
		`select rebuild_asset_search($1::uuid[], $2, $3)`,
		ids, CaptionModel, OCRModel)
	if err != nil {
		return fmt.Errorf("rebuild the search index: %w", err)
	}
	return nil
}

// maxTagLength bounds what a model may invent. A captioner asked for tags
// occasionally answers with a sentence, and a sentence in the tag vocabulary is
// a row that will never match anything and will sit in the merge UI forever.
const maxTagLength = 40

// maxTags bounds how many. Twelve is generous for one frame; a model that
// returns eighty has misunderstood the question, and taking all eighty would
// weight that photograph's tsvector as though somebody had described it very
// carefully.
const maxTags = 12

// normalizeTags turns what a model wrote into vocabulary.
//
// Lowercased and collapsed, because "Dog", "dog " and "dog" are one word and
// three rows would be three entries in the merge UI on day one. Punctuation off
// the ends, because a model asked for a list returns "a dog," about a third of
// the time. Anything left that is empty, absurdly long, or not a word at all is
// dropped rather than stored: the vocabulary is going to be read by a person
// eventually, and this is the cheapest moment to keep it readable.
func normalizeTags(tags []Tag) ([]string, []float32) {
	seen := make(map[string]bool, len(tags))
	names := make([]string, 0, len(tags))
	confidences := make([]float32, 0, len(tags))

	for _, tag := range tags {
		name := strings.ToLower(strings.Join(strings.Fields(tag.Name), " "))
		name = strings.TrimFunc(name, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if name == "" || len(name) > maxTagLength || seen[name] {
			continue
		}
		if !strings.ContainsFunc(name, unicode.IsLetter) {
			continue
		}
		seen[name] = true
		names = append(names, name)
		confidences = append(confidences, tag.Confidence)
		if len(names) == maxTags {
			break
		}
	}
	return names, confidences
}

// Vocabulary is every name this archive can be searched by: the people an
// import confirmed, the places the geocoder resolved, and the words the
// captioner has written so far.
//
// It exists because the query parser is only allowed to recognise things that
// are actually here. "Phoenix" is a person because there are 1,601 photographs
// of one; in an archive with no Phoenix it is a word for the visual half to deal
// with. A parser that recognised names in general would invent filters matching
// nothing, which looks from the outside exactly like an empty library.
type Vocabulary struct {
	People    []string
	Cities    []Place
	Admin1s   []Place
	Countries []Place
	// Tags maps every tag name — including the ones a merge has folded away —
	// to the canonical name a search should use. Resolving here rather than in
	// the query is what makes tags.canonical_id take effect everywhere at once.
	Tags map[string]string
}

// SearchVocabulary reads the whole of it. Four small queries against indexed
// columns; the caller caches the result, because this changes at the speed of an
// import rather than of a keystroke.
func (s *Store) SearchVocabulary(ctx context.Context) (Vocabulary, error) {
	v := Vocabulary{Tags: map[string]string{}}

	// The people are the confirmed ones an import carried, and they must stay
	// the confirmed ones: ML_IMAGES.md §11's seam between a name a person
	// approved and a word a model produced runs right through this function.
	// asset_people and tags are read separately and never joined.
	people, err := s.pool.Query(ctx, `
		select distinct p.name
		from asset_people p
		join assets a on a.id = p.asset_id
		where a.vault = '' and a.deleted_at is null and p.name <> ''
		order by 1`)
	if err != nil {
		return v, fmt.Errorf("read the people in the archive: %w", err)
	}
	if err := scanInto(people, &v.People); err != nil {
		return v, err
	}

	// One row per distinct place, at each of the three levels, carrying enough
	// context for a chip to say "Moraga, California" without a second query.
	// min() over the parents rather than a group of all three, because a city
	// name that straddles a state line is one place as far as a search box is
	// concerned.
	places, err := s.pool.Query(ctx, `
		select 'city', place_city, min(place_admin1), min(place_country)
		  from assets where place_city is not null and vault = '' and deleted_at is null
		 group by place_city
		union all
		select 'admin1', place_admin1, '', min(place_country)
		  from assets where place_admin1 is not null and vault = '' and deleted_at is null
		 group by place_admin1
		union all
		select 'country', place_country, '', ''
		  from assets where place_country is not null and vault = '' and deleted_at is null
		 group by place_country`)
	if err != nil {
		return v, fmt.Errorf("read the places in the archive: %w", err)
	}
	defer places.Close()
	for places.Next() {
		var level, name string
		var admin1, country *string
		if err := places.Scan(&level, &name, &admin1, &country); err != nil {
			return v, fmt.Errorf("scan a place: %w", err)
		}
		switch level {
		case "city":
			v.Cities = append(v.Cities, Place{City: name, Admin1: text(admin1), Country: text(country)})
		case "admin1":
			v.Admin1s = append(v.Admin1s, Place{Admin1: name, Country: text(country)})
		default:
			v.Countries = append(v.Countries, Place{Country: name})
		}
	}
	if err := places.Err(); err != nil {
		return v, fmt.Errorf("read the places in the archive: %w", err)
	}

	tags, err := s.pool.Query(ctx, `
		select t.name, coalesce(c.name, t.name)
		from tags t
		left join tags c on c.id = t.canonical_id`)
	if err != nil {
		return v, fmt.Errorf("read the tag vocabulary: %w", err)
	}
	defer tags.Close()
	for tags.Next() {
		var name, canonical string
		if err := tags.Scan(&name, &canonical); err != nil {
			return v, fmt.Errorf("scan a tag: %w", err)
		}
		v.Tags[name] = canonical
	}
	return v, tags.Err()
}

func scanInto(rows pgx.Rows, into *[]string) error {
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return fmt.Errorf("scan vocabulary: %w", err)
		}
		*into = append(*into, value)
	}
	return rows.Err()
}

func text(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// DescribeCoverage is how far the two word-writing passes have got.
//
// The same two-numbers-not-one shape as EmbeddingCoverage, and for the same
// reason: during a backfill the interesting question is not how many rows exist
// but how many of the things that could have one do. Eligible is what the
// timeline shows and mlprep has written renditions for.
type DescribeCoverage struct {
	Eligible  int64 `json:"eligible"`
	Described int64 `json:"described"`
	Recognised int64 `json:"recognised"`
	Tags      int64 `json:"tags"`
	Vocabulary int64 `json:"vocabulary"`
}

func (s *Store) DescribeCoverage(ctx context.Context) (DescribeCoverage, error) {
	const query = `
		select
		    (select count(*) from assets a
		       join jobs prep on prep.asset_id = a.id
		            and prep.kind = 'mlprep' and prep.state = 'done'
		      where a.vault = '' and a.deleted_at is null),
		    (select count(*) from asset_descriptions where model = $1),
		    (select count(*) from asset_ocr where model = $2 and text <> ''),
		    (select count(*) from asset_tags),
		    (select count(*) from tags)`

	var c DescribeCoverage
	err := s.pool.QueryRow(ctx, query, CaptionModel, OCRModel).
		Scan(&c.Eligible, &c.Described, &c.Recognised, &c.Tags, &c.Vocabulary)
	if err != nil {
		return c, fmt.Errorf("count description coverage: %w", err)
	}
	return c, nil
}

// AnalysisTag is one word, as it will be searched and as it was written.
//
// Both, because they are two different facts and the panel that draws this is
// the one place either matters. Name is what a search resolves to; Raw is what
// the captioner actually put on this photograph. They differ exactly when a
// merge has folded one into the other, and seeing that a photograph was called
// "puppy" and is findable as "dog" is the whole of what makes ML_IMAGES.md §9's
// cleanup reviewable rather than a leap of faith.
type AnalysisTag struct {
	Name       string  `json:"name"`
	Raw        string  `json:"raw,omitempty"`
	Confidence float32 `json:"confidence,omitempty"`
}

// Analysis is everything the ML passes have said about one photograph, read
// back for a person rather than for a ranking.
//
// The same four tables db.Search fuses, in the opposite direction: search asks
// "which photographs match this sentence", and this asks "what does the archive
// think this photograph is". Which is the question somebody has when a search
// returned something surprising, and until now had no way to ask.
//
// Every field is allowed to be empty and empty means different things, which is
// why Jobs is here. A photograph with no caption may be one the captioner has
// not reached — hours of GPU are queued ahead of it — or one it looked at and
// failed on, and a panel that drew both as blank would be the silent-exclusion
// failure §11 warns about, one surface further out.
type Analysis struct {
	Caption      string     `json:"caption,omitempty"`
	CaptionModel string     `json:"caption_model,omitempty"`
	CaptionedAt  *time.Time `json:"captioned_at,omitempty"`

	// Tags in confidence order, which is the order the captioner was surest in
	// rather than any order a person chose.
	Tags []AnalysisTag `json:"tags,omitempty"`

	// Text is what the recogniser read, verbatim and unabridged. Search sees a
	// headline of the matching stretch; this is the whole of it, because the
	// receipt somebody is looking at is the reason they opened the panel.
	Text      string     `json:"text,omitempty"`
	TextModel string     `json:"text_model,omitempty"`
	ReadAt    *time.Time `json:"read_at,omitempty"`

	// Frames is how many vectors the encoder wrote: 1 for a still, one per
	// sampled frame for a video, 0 for anything the vision pass has not reached.
	// It is not shown as a number so much as an answer to "is this findable by
	// what it looks like at all".
	Frames      int    `json:"frames,omitempty"`
	VisionModel string `json:"vision_model,omitempty"`

	// Jobs maps each of the four ML kinds to its state, for the kinds that have
	// a row. Absent means never queued, which for `describe` is the ordinary
	// case on a library the backfill has not been run over.
	Jobs map[string]string `json:"jobs,omitempty"`
}

// AssetAnalysis reads it: three queries, none of them on any hot path.
//
// Three rather than one because the shapes do not fit together — the scalars
// are one row, the tags are many, and the job states are many — and because a
// panel that draws a caption is worth more than one that refuses to draw
// anything, so the caller is expected to log a failure here and carry on.
//
// Nothing is read for an asset in the vault, and nothing needs to be: the write
// paths all refuse it, so the tables are empty for those rows by construction
// rather than by this query remembering to say so. The guard is repeated
// anyway, because "the tables happen to be empty" is a fact about the past and
// the caller is asking about the present.
func (s *Store) AssetAnalysis(ctx context.Context, assetID string) (Analysis, error) {
	var a Analysis

	const scalars = `
		select d.caption, d.generated_at, o.text, o.generated_at,
		       (select count(*) from asset_embeddings e
		         where e.asset_id = a.id and e.model = $4)
		from assets a
		left join asset_descriptions d on d.asset_id = a.id and d.model = $2
		left join asset_ocr          o on o.asset_id = a.id and o.model = $3
		where a.id = $1::uuid and a.vault = '' and a.deleted_at is null`

	var caption, ocrText *string
	var captionedAt, readAt *time.Time
	var frames int64
	err := s.pool.QueryRow(ctx, scalars, assetID, CaptionModel, OCRModel, VisionModel).
		Scan(&caption, &captionedAt, &ocrText, &readAt, &frames)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Hidden, trashed, or gone between the viewer opening and the panel
		// being asked. An empty analysis is the truthful answer to all three.
		return a, nil
	case err != nil:
		return a, fmt.Errorf("read the analysis of %s: %w", assetID, err)
	}

	a.Caption = text(caption)
	a.CaptionedAt = captionedAt
	if a.Caption != "" {
		a.CaptionModel = CaptionModel
	}
	a.Text = text(ocrText)
	a.ReadAt = readAt
	if a.Text != "" {
		a.TextModel = OCRModel
	}
	a.Frames = int(frames)
	if a.Frames > 0 {
		a.VisionModel = VisionModel
	}

	// Resolved through the merge on the way out, the way rebuild_asset_search
	// and db.Search resolve it: asset_tags holds what the model wrote and
	// canonical_id is read at every point of use, which is what keeps a
	// mistaken merge undoable.
	const tags = `
		select coalesce(canonical.name, tag.name), tag.name, at.confidence
		from asset_tags at
		join tags tag on tag.id = at.tag_id
		left join tags canonical on canonical.id = tag.canonical_id
		where at.asset_id = $1::uuid
		order by at.confidence desc nulls last, tag.name`
	rows, err := s.pool.Query(ctx, tags, assetID)
	if err != nil {
		return a, fmt.Errorf("read the tags of %s: %w", assetID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var t AnalysisTag
		var confidence *float32
		if err := rows.Scan(&t.Name, &t.Raw, &confidence); err != nil {
			return a, fmt.Errorf("scan a tag of %s: %w", assetID, err)
		}
		if confidence != nil {
			t.Confidence = *confidence
		}
		// Only worth sending when it is news. Equal is the overwhelming
		// majority, and a panel does not need to be told a word is itself.
		if t.Raw == t.Name {
			t.Raw = ""
		}
		a.Tags = append(a.Tags, t)
	}
	if err := rows.Err(); err != nil {
		return a, fmt.Errorf("read the tags of %s: %w", assetID, err)
	}

	const states = `
		select kind, state from jobs
		where asset_id = $1::uuid and kind in ('mlprep', 'vision', 'ocr', 'describe')`
	jobRows, err := s.pool.Query(ctx, states, assetID)
	if err != nil {
		return a, fmt.Errorf("read the ML job states of %s: %w", assetID, err)
	}
	defer jobRows.Close()
	for jobRows.Next() {
		var kind, state string
		if err := jobRows.Scan(&kind, &state); err != nil {
			return a, fmt.Errorf("scan an ML job state of %s: %w", assetID, err)
		}
		if a.Jobs == nil {
			a.Jobs = map[string]string{}
		}
		a.Jobs[kind] = state
	}
	return a, jobRows.Err()
}
