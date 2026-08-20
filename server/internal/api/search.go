package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
	"github.com/dominicclerici/photos-backup/server/internal/searchquery"
)

// The search endpoint, and the four things that have to happen in order for a
// sentence to become a page of photographs.
//
//	parse    the grammar, then photo-ml's /parse where the grammar was silent
//	embed    the leftover phrase, as a vector in the encoder's space
//	rank     two rankings fused inside the structured WHERE — see db.Search
//	echo     the parse, back out with the results
//
// The last one is not decoration. ML_IMAGES.md §11: a query parser fails
// confidently — it decides "last summer" meant 2024, silently removes the right
// answer, and shows an empty grid. Sending the parse back is what lets the page
// draw "Phoenix ×" and "Jun–Sep 2025 ×" as removable chips, so a wrong parse is
// visible and fixed with one click rather than by retyping the sentence and
// hoping. A search is an editable filter, not an oracle.
//
// Every step but the last is optional. photo-ml being down costs the vector
// ranking and the parse refinement; it does not cost the search box, and the
// full-text half has been sitting in Postgres since the last backfill.

const (
	defaultSearchLimit = 60
	maxSearchLimit     = 200
	// searchEmbedTimeout bounds the one model call a person is waiting on.
	// Shorter than mlclient's default, which is sized for a cold service during
	// a backfill; here a slow answer is worth less than a fast degraded one,
	// because the text ranking is already good enough to be useful.
	searchEmbedTimeout = 10 * time.Second
	// vocabularyTTL is how long the archive's own names are held between
	// searches. They change at the speed of an import.
	vocabularyTTL = 5 * time.Minute
)

// searchResponse is one page of ranked photographs, and what the server thought
// the question was.
type searchResponse struct {
	Query parsedQuery `json:"query"`
	// Items reuses the timeline's item shape with the ranking bolted on, so the
	// grid draws a search result with the component it draws everything else
	// with.
	Items []db.SearchResult `json:"items"`
	// Total is how many candidates the ranking had to choose from, not how many
	// photographs in the archive could conceivably match. For a fused ranking
	// that is the honest number: everything past the fusion's depth was never
	// compared against anything.
	Total int `json:"total"`
	// Degraded says, in a sentence, what this search could not do. Empty when
	// nothing was lost. It exists so that "no results" and "no results, and the
	// GPU service is down" are not the same page.
	Degraded string `json:"degraded,omitempty"`
}

// parsedQuery is the parse on the wire: dates as plain days, because the thing
// reading this draws them in a chip.
type parsedQuery struct {
	Text      string             `json:"text"`
	People    []string           `json:"people,omitempty"`
	Place     *searchquery.Place `json:"place,omitempty"`
	Tags      []string           `json:"tags,omitempty"`
	After     string             `json:"after,omitempty"`
	Before    string             `json:"before,omitempty"`
	Kind      string             `json:"kind,omitempty"`
	Category  string             `json:"category,omitempty"`
	Favorites bool               `json:"favorites,omitempty"`
	// Visual is what went to the encoder, which is not what was typed. Echoed
	// because "why did searching for my dog return the ocean" is answered by
	// seeing that the phrase became "ocean".
	Visual string `json:"visual,omitempty"`
	// Source is "grammar" when the Go parser answered alone, "model" when
	// photo-ml's /parse contributed something the grammar had missed.
	Source string `json:"source"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	limit := defaultSearchLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxSearchLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	offset := 0
	if raw := query.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a row offset")
			return
		}
		offset = n
	}

	parsed, degraded := s.parse(r)

	// The visual half, as a vector. Absent is a supported answer: a query with
	// no leftover phrase — "videos from 2019" — never wanted one, and a query
	// whose phrase could not be embedded falls back to the text ranking, which
	// is §7's degraded path with no second code path behind it.
	var vector []float32
	if parsed.Visual != "" && s.ML != nil {
		ctx, cancel := context.WithTimeout(r.Context(), searchEmbedTimeout)
		embedded, err := s.ML.EmbedTexts(ctx, []string{parsed.Visual})
		cancel()
		switch {
		case err != nil:
			s.logger().Warn("could not embed a search phrase; ranking by text alone",
				"phrase", parsed.Visual, "error", err)
			degraded = degradedNote(degraded, err)
		case len(embedded.Vectors) == 1:
			vector = embedded.Vectors[0]
		}
	} else if parsed.Visual != "" && s.ML == nil {
		degraded = "photo-ml is not configured, so this search ranked by words alone: captions, tags, recognised text, filenames and place names"
	}

	results, total, err := s.Store.Search(r.Context(), db.SearchRequest{
		Filter: filterOf(parsed),
		Vector: vector,
		Model:  db.VisionModel,
		Text:   parsed.Visual,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		s.writeFilterError(w, err, "run search")
		return
	}
	if results == nil {
		results = []db.SearchResult{}
	}

	writeJSON(w, http.StatusOK, searchResponse{
		Query:    echo(parsed),
		Items:    results,
		Total:    total,
		Degraded: degraded,
	})
}

// parse turns the request into a query, by whichever of the two routes the
// caller asked for.
//
// `parse=0` is the escape hatch and it is what the chips are built on. With it,
// nothing is guessed: the filter comes from explicit parameters and the phrase
// from `q`, so removing a chip is omitting a parameter. Removing something a
// parser inferred is not expressible any other way — a merge-on-top of a parse
// can add a filter but has no way to say "and not the date it found".
func (s *Server) parse(r *http.Request) (searchquery.Query, string) {
	query := r.URL.Query()
	text := query.Get("q")

	if !truthyDefault(query.Get("parse"), true) {
		return explicitQuery(query, text), ""
	}

	vocab, err := s.vocabulary(r.Context())
	if err != nil {
		// The parser without a vocabulary would recognise no name and no place
		// in the archive, which is a worse answer than none: it would quietly
		// send "phoenix" to the encoder as a visual phrase. The whole sentence
		// goes to the fuzzy half instead, and the note says so.
		s.logger().Error("read the search vocabulary", "error", err)
		return searchquery.Query{Text: text, Visual: text, Source: searchquery.SourceGrammar},
			"the archive's list of people and places could not be read, so nothing in this query was matched against them"
	}

	grammar := searchquery.Parse(text, vocab, time.Now())
	if s.ML == nil || text == "" {
		return grammar, ""
	}

	// And then the model, on top, where the grammar was silent. Everything it
	// says is checked against the vocabulary above before any of it is
	// believed; see searchquery.Merge for why that asymmetry is the design
	// rather than a lack of confidence in the model.
	claimed, err := s.ML.Parse(r.Context(), text, time.Now(), vocab.People)
	if err != nil {
		// Not worth a note in the response. The grammar has already answered,
		// and what was lost is a refinement nobody can see the absence of.
		s.logger().Debug("photo-ml could not parse a query; the grammar's reading stands",
			"query", text, "error", err)
		return grammar, ""
	}
	return searchquery.Merge(grammar, &claimed, vocab, time.Now()), ""
}

// explicitQuery builds a query from parameters alone, for `parse=0`.
func explicitQuery(query map[string][]string, text string) searchquery.Query {
	get := func(key string) string {
		if v, ok := query[key]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	q := searchquery.Query{
		Text:      text,
		Visual:    text,
		People:    query["person"],
		Tags:      query["tag"],
		Kind:      get("kind"),
		Category:  get("category"),
		Favorites: truthy(get("favorites")),
		Source:    searchquery.SourceGrammar,
	}
	// Present-but-empty is the one spelling that means something here, so this
	// asks whether the parameter was sent rather than whether it says anything.
	// A chip row materialises the whole parse into parameters, and a parse whose
	// visual half is empty — "phoenix", all of it a name — has to be able to say
	// so. Falling back to `q` there would hand the encoder a word the filter has
	// already answered exactly.
	if v, ok := query["visual"]; ok && len(v) > 0 {
		q.Visual = v[0]
	}
	switch {
	case get("city") != "":
		q.Place = &searchquery.Place{City: get("city")}
	case get("admin1") != "":
		q.Place = &searchquery.Place{Admin1: get("admin1")}
	case get("country") != "":
		q.Place = &searchquery.Place{Country: get("country")}
	}
	if t, err := time.Parse(time.DateOnly, get("after")); err == nil {
		q.After = &t
	}
	if t, err := time.Parse(time.DateOnly, get("before")); err == nil {
		q.Before = &t
	}
	return q
}

// filterOf is the structured half, as the timeline's own filter.
//
// No second query engine, which is ML_IMAGES.md §7 step 2: every index that
// already serves the gallery serves this, and a search narrowed to a person is
// the same query the person's page runs.
func filterOf(q searchquery.Query) db.TimelineFilter {
	filter := db.TimelineFilter{
		People:    q.People,
		Tags:      q.Tags,
		After:     q.After,
		Before:    q.Before,
		Kind:      q.Kind,
		Category:  q.Category,
		Favorites: q.Favorites,
	}
	if q.Place != nil {
		filter.Place = &db.Place{City: q.Place.City, Admin1: q.Place.Admin1, Country: q.Place.Country}
	}
	return filter
}

func echo(q searchquery.Query) parsedQuery {
	out := parsedQuery{
		Text:      q.Text,
		People:    q.People,
		Place:     q.Place,
		Tags:      q.Tags,
		Kind:      q.Kind,
		Category:  q.Category,
		Favorites: q.Favorites,
		Visual:    q.Visual,
		Source:    q.Source,
	}
	if q.After != nil {
		out.After = q.After.Format(time.DateOnly)
	}
	if q.Before != nil {
		out.Before = q.Before.Format(time.DateOnly)
	}
	return out
}

func degradedNote(existing string, err error) string {
	if existing != "" {
		return existing
	}
	if errors.Is(err, mlclient.ErrUnavailable) {
		return "photo-ml is not answering, so this search ranked by words alone: captions, tags, recognised text, filenames and place names"
	}
	return "the visual half of this search could not run; it ranked by words alone"
}

// vocabulary is the archive's own names, cached.
//
// Cached because it is read on every keystroke-ish and changes on an import: a
// few hundred place names and a handful of people, re-read every five minutes.
// The stale copy is served while a refresh fails, because a search that
// recognises slightly out-of-date names is better than one that recognises
// none.
func (s *Server) vocabulary(ctx context.Context) (searchquery.Vocabulary, error) {
	s.vocab.mu.Lock()
	defer s.vocab.mu.Unlock()

	if time.Since(s.vocab.at) < vocabularyTTL && s.vocab.loaded {
		return s.vocab.value, nil
	}
	fresh, err := s.Store.SearchVocabulary(ctx)
	if err != nil {
		if s.vocab.loaded {
			return s.vocab.value, nil
		}
		return searchquery.Vocabulary{}, err
	}

	s.vocab.value = searchquery.Vocabulary{
		People:    fresh.People,
		Cities:    places(fresh.Cities),
		Admin1s:   places(fresh.Admin1s),
		Countries: places(fresh.Countries),
		Tags:      fresh.Tags,
	}
	s.vocab.at = time.Now()
	s.vocab.loaded = true
	return s.vocab.value, nil
}

// vocabularyCache holds the archive's names between searches.
type vocabularyCache struct {
	mu     sync.Mutex
	value  searchquery.Vocabulary
	at     time.Time
	loaded bool
}

func places(in []db.Place) []searchquery.Place {
	out := make([]searchquery.Place, len(in))
	for i, p := range in {
		out[i] = searchquery.Place{City: p.City, Admin1: p.Admin1, Country: p.Country}
	}
	return out
}

// truthyDefault reads a flag whose absence means something other than false.
func truthyDefault(raw string, fallback bool) bool {
	if raw == "" {
		return fallback
	}
	return truthy(raw)
}
