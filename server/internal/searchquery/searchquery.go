// Package searchquery turns a typed sentence into a filter.
//
// It is the degraded path from ML_IMAGES.md §7, written first rather than last.
// §11 calls the query parser the weakest link in the whole feature and it is
// right: vector search either works or is boringly mediocre, but a parser fails
// *confidently* — it decides "last summer" meant 2024, silently filters out the
// answer, and shows an empty grid with no evidence of why. A degraded path
// written after the confident one is the one that never gets written, so this
// is the one that exists first and the model is the thing layered on top.
//
// Two properties make it safe to trust, and they are the same two that make the
// model's output safe to accept in merge.go.
//
// **It only recognises what the archive contains.** People come from
// asset_people, places from the geocoded columns, tags from the tags table.
// "phoenix" is a person because there are 1,601 photographs of one; in an
// archive with no Phoenix it is a word for the visual half to deal with. A
// parser that recognised names in general would invent filters that match
// nothing, which is exactly the failure that looks like an empty library.
//
// **Every range it produces is generous.** A window slightly too wide costs a
// few extra results at the bottom of a ranked page. A window slightly too
// narrow hides the photograph that was being looked for, and hides the reason
// too. Where the two are in tension — see the seasons in dates.go — this leans
// wide every time.
//
// What it does not do is understand anything. It matches vocabulary, reads
// dates, and hands whatever is left to the embedding as the visual phrase.
// "phoenix at the beach last summer" comes out as a person, a date range, and
// the four words "at the beach", which is the split ML_IMAGES.md §2 opens with.
package searchquery

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

// Query is a sentence, taken apart.
//
// The structured fields become a db.TimelineFilter — exact answers to exact
// questions, over indexes that already exist. Visual is everything left over
// and becomes a vector, because "at the beach" is not a question a database can
// answer and "June 2025" is not one an embedding can.
//
// Every field is optional and the zero value means "did not narrow by this",
// which is what lets a query that is only a phrase and a query that is only a
// date go down the same path.
type Query struct {
	// Text is what was typed, kept verbatim. It is echoed back with the parse so
	// a client can show what was understood beside what was asked.
	Text string

	// People are names as asset_people stores them, already matched
	// case-insensitively against the archive's own list. More than one is an
	// AND: "phoenix and dominic" means both are in the photograph.
	People []string
	// Place is where, at whichever of the three levels matched. Longest match
	// wins, so "new york city" is a city and "new york" is a state.
	Place *Place
	// Tags are canonical tag names — a merge is already resolved, so a query for
	// "dog" finds what the model called a puppy. Empty until the vision pass has
	// written a vocabulary.
	Tags []string

	// After and Before are civil dates in UTC, both inclusive: "June 2025" is
	// after=2025-06-01, before=2025-06-30. Inclusive because that is what a
	// removable chip has to be able to say — "Jun–Sep 2025" — and an exclusive
	// end would show the reader a month they did not ask about.
	After  *time.Time
	Before *time.Time

	// Kind is db.MediaImage or db.MediaVideo, and empty for both. See kinds.go
	// for why "videos" narrows and "photos" usually does not.
	Kind string
	// Category is one of internal/db's closed list — screenshots, panoramas,
	// live and the rest.
	Category  string
	Favorites bool

	// Visual is the leftover phrase, cleaned of the words that were only there
	// to hold the sentence together. Empty is common and is not a failure: "my
	// videos from 2019" is a question with no visual half at all, and answering
	// it needs no GPU.
	Visual string

	// Source names what produced this parse: SourceGrammar for the code in this
	// package, SourceModel when photo-ml's /parse contributed a field the
	// grammar had nothing to say about. See merge.go.
	Source string
}

const (
	SourceGrammar = "grammar"
	SourceModel   = "model"
)

// Place is where a photograph was taken, at the level the query named it.
//
// Exactly one of the three is set. Which one decides which column the filter
// compares, which is why this is not three loose strings: "California" is an
// admin1 and matching it against place_city would find nothing while looking
// like it worked.
type Place struct {
	City    string `json:"city,omitempty"`
	Admin1  string `json:"admin1,omitempty"`
	Country string `json:"country,omitempty"`
}

// Label renders a place the way a chip would say it.
func (p Place) Label() string {
	switch {
	case p.City != "":
		return p.City
	case p.Admin1 != "":
		return p.Admin1
	default:
		return p.Country
	}
}

// Structured reports whether anything but the visual phrase was recognised.
//
// The question the search path asks to decide whether it is running a filtered
// ranking or a ranking, and the question merge.go asks to decide whether the
// model was any use.
func (q Query) Structured() bool {
	return len(q.People) > 0 || q.Place != nil || len(q.Tags) > 0 ||
		q.After != nil || q.Before != nil ||
		q.Kind != "" || q.Category != "" || q.Favorites
}

// Empty reports a query that narrowed nothing and asked nothing — the empty
// search box, which is the whole library in date order.
func (q Query) Empty() bool { return !q.Structured() && q.Visual == "" }

// Vocabulary is what this archive actually holds, and the only thing the
// grammar is allowed to recognise as a name.
//
// Loaded from the database and cached, because it changes at the speed of an
// import rather than of a keystroke. See db.SearchVocabulary.
type Vocabulary struct {
	// People as asset_people spells them: "Chris Morrison", not "chris".
	People []string
	// Places, one entry per distinct value of each column. Cities carry their
	// state and country so a chip can say "Moraga, California" without a second
	// query.
	Cities    []Place
	Admin1s   []Place
	Countries []Place
	// Tags maps every tag name — including the ones a merge has folded away — to
	// the canonical name search should use. A vocabulary with "puppy" → "dog"
	// resolves the merge before the query is even built, which is what makes
	// tags.canonical_id take effect everywhere at once.
	Tags map[string]string
}

// Parse reads a sentence against a vocabulary.
//
// now is the clock the relative dates are measured from. It is a parameter
// rather than a call to time.Now because "last summer" is the single most
// testable thing in this package and the single most annoying to test against a
// moving present.
func Parse(text string, vocab Vocabulary, now time.Time) Query {
	q := Query{Text: strings.TrimSpace(text), Source: SourceGrammar}
	toks := tokenize(text)
	if len(toks) == 0 {
		return q
	}

	// The explicit syntax first, because it is not English and nothing else
	// should get a look at it. `after:2019-06-01` is a token the date reader
	// would otherwise have to be taught to ignore.
	matchFields(toks, &q, vocab)

	// Dates next, and before the names. A year is four digits that no
	// vocabulary contains, but a month can be a person's name and a season can
	// be a place — "Summer" is a name people have. Reading dates first means
	// "summer 2019" is a date range rather than a person and a number, which is
	// what it says.
	parseDates(toks, &q, now)

	// Then the archive's own words, longest phrase first, so "new york city"
	// beats "new york" and "chris morrison" beats "chris".
	matchVocabulary(toks, &q, vocab)

	// Then the facets, which are a closed list of English rather than a property
	// of this archive.
	matchFacets(toks, &q)

	// And last, the words that were only ever addressed to the search box.
	// Last because "photos" is framing here and part of a filter two passes
	// above — "only photos" narrows and "photos of the beach" does not — and
	// the pass that narrows has to get there first.
	matchFraming(toks)

	q.Visual = leftover(toks)
	return q
}

// token is one word of the query, and whether something has claimed it.
//
// raw is kept beside word because the visual phrase is reassembled from what
// nothing claimed, and "Phoenix's" should come back out of that as it went in
// rather than as the normalised form the matcher compared against.
type token struct {
	word string
	raw  string
	used bool
}

// tokenize splits on whitespace and strips the punctuation that hangs off the
// ends of words, keeping what is inside them.
//
// Inside matters: 2019-06-01 is one token and has to stay one, and so does
// "time-lapse". A possessive is trimmed because "phoenix's birthday" is a
// question about Phoenix, and the apostrophe is the only thing standing between
// the name and the vocabulary.
func tokenize(text string) []token {
	fields := strings.Fields(strings.ToLower(text))
	raws := strings.Fields(text)
	edge := func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }

	toks := make([]token, 0, len(fields))
	for i, f := range fields {
		word := strings.TrimFunc(f, edge)
		word = strings.TrimSuffix(strings.TrimSuffix(word, "'s"), "’s")
		if word == "" {
			continue
		}
		raw := word
		if i < len(raws) {
			raw = strings.TrimFunc(raws[i], edge)
		}
		toks = append(toks, token{word: word, raw: raw})
	}
	return toks
}

// phrase is one entry in the matcher: the words to look for, and what finding
// them means.
type phrase struct {
	words []string
	apply func(*Query)
}

// match walks the tokens once, taking the longest unclaimed phrase at each
// position.
//
// Longest-first is the whole of the disambiguation. "new york city" and "new
// york" are both in the vocabulary of an archive that holds both, and matching
// the shorter one first would file 570 photographs of Manhattan under the
// state. Sorting once per call is cheap against a vocabulary of a few hundred
// entries and a query of a few words.
func match(toks []token, phrases []phrase, q *Query) {
	sort.SliceStable(phrases, func(i, j int) bool {
		return len(phrases[i].words) > len(phrases[j].words)
	})

	for i := range toks {
		if toks[i].used {
			continue
		}
		for _, p := range phrases {
			if !fits(toks, i, p.words) {
				continue
			}
			p.apply(q)
			for n := range p.words {
				toks[i+n].used = true
			}
			break
		}
	}
}

// fits reports whether a phrase begins at position i over tokens nothing has
// claimed.
func fits(toks []token, i int, words []string) bool {
	if i+len(words) > len(toks) {
		return false
	}
	for n, w := range words {
		if toks[i+n].used || toks[i+n].word != w {
			return false
		}
	}
	return true
}

// matchVocabulary recognises the names this archive actually contains.
func matchVocabulary(toks []token, q *Query, vocab Vocabulary) {
	var phrases []phrase

	for _, name := range vocab.People {
		name := name
		phrases = append(phrases, phrase{
			words: strings.Fields(strings.ToLower(name)),
			apply: func(q *Query) { q.People = appendUnique(q.People, name) },
		})
	}

	// Cities before states before countries, which the length sort mostly
	// handles and this makes deterministic for the ties. A query naming both a
	// city and its state — "san francisco california" — keeps the city, because
	// the first match claims the tokens and the state has nothing left to match
	// against.
	for _, group := range [][]Place{vocab.Cities, vocab.Admin1s, vocab.Countries} {
		for _, p := range group {
			p := p
			phrases = append(phrases, phrase{
				words: strings.Fields(strings.ToLower(p.Label())),
				apply: func(q *Query) {
					if q.Place == nil {
						q.Place = &p
					}
				},
			})
		}
	}

	// Tags are deliberately absent from this pass, and it is the one omission in
	// the file worth arguing about.
	//
	// A tag is a word a model wrote, and the vocabulary will run to thousands of
	// them. Matching them here would turn "dog" into a hard filter — every
	// photograph the captioner happened to write "dog" about, in upload order,
	// with the visual phrase now empty and no ranking left to do. That is
	// strictly worse than what happens when the word falls through: it becomes
	// the visual phrase, the encoder finds dogs it was never told about, and the
	// tsvector — where tags sit at weight A — ranks the ones that were.
	//
	// The hard filter still exists, for the tag browser and for anybody who
	// wants it: `tag:dog`, in matchFields. Asking for it explicitly is the
	// difference between a filter and a guess.
	match(toks, phrases, q)
}

// matchFields reads the explicit syntax: `person:phoenix`, `tag:dog`,
// `after:2019-06-01`.
//
// It exists because everything else in this package is a guess with good
// manners, and sometimes a guess is not what is wanted. It is also the only way
// to reach a tag as a filter, and the only way to name a date the grammar has
// no phrasing for. Underscores stand in for spaces, so `person:chris_morrison`
// is one token.
//
// A field whose value this archive does not contain is left alone rather than
// silently dropped. `tag:banana` in a library with no bananas then reaches the
// visual phrase and returns something fuzzy, which is a more useful answer than
// an empty grid — and the phrase is echoed back, so what happened is visible.
func matchFields(toks []token, q *Query, vocab Vocabulary) {
	for i := range toks {
		if toks[i].used {
			continue
		}
		field, value, ok := strings.Cut(toks[i].word, ":")
		if !ok || value == "" {
			continue
		}
		value = strings.ReplaceAll(value, "_", " ")

		claimed := true
		switch field {
		case "tag", "tags":
			canonical, known := vocab.Tags[value]
			if known {
				q.Tags = appendUnique(q.Tags, canonical)
			}
			claimed = known
		case "person", "people", "who":
			if people := vocab.matchPeople([]string{value}); len(people) > 0 {
				q.People = appendUnique(q.People, people[0])
			} else {
				claimed = false
			}
		case "place", "where":
			if place := vocab.matchPlace(value); place != nil && q.Place == nil {
				q.Place = place
			} else {
				claimed = false
			}
		case "kind", "type":
			if kind := normalizeKind(value); kind != "" {
				q.Kind = kind
			} else {
				claimed = false
			}
		case "category":
			if categories[value] {
				q.Category = value
			} else {
				claimed = false
			}
		case "after", "since":
			if t, ok := parseModelDate(value, false); ok {
				q.After = &t
			} else {
				claimed = false
			}
		case "before", "until":
			if t, ok := parseModelDate(value, true); ok {
				q.Before = &t
			} else {
				claimed = false
			}
		case "favorite", "favourite", "favorites", "favourites":
			q.Favorites = value == "true" || value == "yes" || value == "1"
		default:
			claimed = false
		}
		toks[i].used = claimed
	}
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// matchFraming removes the words that are addressed to the search box rather
// than to the archive.
//
// Framing is "show me", "photos of", "find" — the scaffolding of asking. It is
// deliberately not the same thing as English structure: prepositions and
// articles stay, because they are what a text encoder was trained on. SigLIP
// learned from alt text, where "at the beach" and "in front of the tree" are
// how captions are written, and stripping them down to "beach" and "front tree"
// would hand the encoder something nobody ever wrote under a photograph.
//
// The phrases are matched longest-first like everything else, which is what
// makes "photos of me at the beach" come out as "at the beach": "photos of"
// goes as a unit and the stranded "me" goes on its own, rather than "of" being
// left dangling at the front.
func matchFraming(toks []token) {
	var phrases []phrase
	for _, words := range [][]string{
		{"show", "me"}, {"find", "me"}, {"get", "me"}, {"give", "me"},
		{"search", "for"}, {"look", "for"}, {"looking", "for"},
		{"photos", "of"}, {"photo", "of"}, {"pictures", "of"}, {"picture", "of"},
		{"images", "of"}, {"image", "of"}, {"pics", "of"}, {"pic", "of"},
		{"show"}, {"find"}, {"search"}, {"get"}, {"please"},
		{"photos"}, {"photo"}, {"pictures"}, {"picture"}, {"pics"}, {"pic"},
		{"images"}, {"image"},
		{"me"}, {"my"}, {"our"}, {"us"}, {"i"}, {"we"},
		{"all"}, {"any"}, {"some"}, {"taken"}, {"shot"},
	} {
		phrases = append(phrases, phrase{words: words, apply: func(*Query) {}})
	}
	match(toks, phrases, nil)
}

// leftover reassembles what nothing claimed into the phrase the embedding gets.
//
// Joined in the order it was typed, because word order carries meaning to a
// text tower: "dog on a surfboard" is not "surfboard on a dog". The raw
// spelling rather than the normalised one, so what goes to the encoder is what
// was typed.
//
// An empty result is the ordinary outcome for a fully structured query and is
// not a failure. It is what tells the search path there is nothing to embed.
func leftover(toks []token) string {
	var kept []token
	for _, t := range toks {
		if !t.used {
			kept = append(kept, t)
		}
	}

	// A filter that claimed the middle of a phrase leaves its glue behind:
	// "videos from 2019" is fully understood and what is left over is the word
	// "from". Nothing is gained by embedding that, and a phrase made only of
	// glue is indistinguishable from no phrase at all.
	onlyGlue := true
	for _, t := range kept {
		if !connective[t.word] {
			onlyGlue = false
			break
		}
	}
	if onlyGlue {
		return ""
	}

	// A dangling word at either end is the same damage, one word smaller. Only
	// at the ends, and only the words that cannot begin or finish a caption:
	// "of the beach" is what is left when "only photos" claims the noun out of
	// "photos of", while "at the beach" is a phrase somebody wrote under a
	// photograph and has to survive intact.
	for len(kept) > 0 && danglingHead[kept[0].word] {
		kept = kept[1:]
	}
	for len(kept) > 0 && danglingTail[kept[len(kept)-1].word] {
		kept = kept[:len(kept)-1]
	}

	words := make([]string, len(kept))
	for i, t := range kept {
		words[i] = t.raw
	}
	return strings.Join(words, " ")
}

// connective is the glue of an English sentence: everything that carries no
// picture on its own.
var connective = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "in": true, "at": true,
	"on": true, "with": true, "and": true, "or": true, "from": true, "to": true,
	"for": true, "by": true, "about": true, "that": true, "which": true,
	"this": true, "these": true, "those": true, "is": true, "are": true,
	"was": true, "were": true, "it": true, "its": true, "there": true,
	"then": true, "than": true, "as": true, "but": true,
}

// danglingHead is the words no caption begins with. "at the beach" and "in the
// snow" are captions; "of the beach" is the wreckage of one.
var danglingHead = map[string]bool{
	"of": true, "and": true, "or": true, "that": true, "which": true,
	"is": true, "are": true, "was": true, "were": true, "but": true,
	"than": true, "then": true, "as": true, "about": true, "for": true,
}

// danglingTail is the words no caption ends with, which is nearly all of them.
var danglingTail = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "in": true, "at": true,
	"on": true, "with": true, "and": true, "or": true, "from": true, "to": true,
	"for": true, "by": true, "about": true, "that": true, "which": true,
	"is": true, "are": true, "was": true, "were": true, "as": true, "but": true,
}
