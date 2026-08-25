package searchquery

import (
	"strings"
	"time"
)

// What photo-ml's /parse says, and how much of it is believed.
//
// ML_IMAGES.md §11 is blunt about the risk: a query parser fails *confidently*.
// It decides "last summer" meant 2024, silently removes the right answer, and
// leaves an empty grid with nothing to argue with. A 0.6B model asked to emit
// JSON will do that several times a day.
//
// So the model is not the parser. The grammar is the parser, and the model is
// allowed to speak only where the grammar was silent — and only in words this
// archive already contains. It cannot move a date the grammar read, cannot
// invent a person, cannot name a place nobody has photographed, and cannot
// propose a visual phrase made of words that were not typed. What is left after
// those four rules is the thing it is genuinely good at: noticing that "the
// snowy one from the ski trip with Chris" mentions a person, when the grammar
// only ever finds one if the name is spelled the way asset_people spells it.
//
// The failure mode this leaves is a parse that is too *narrow* in coverage —
// the model says something true and unverifiable and it gets dropped. That is
// the right direction to fail in. The pills in §7 make what survived visible
// either way.

// ModelQuery is /parse's answer, exactly as the JSON arrives and before
// anything has been checked. Every field is a claim, not a fact.
//
// Three of them, and the list of what is *not* here is the more interesting
// half.
//
// Media kind, category and favourites are absent because the grammar already
// answers them completely: they are a closed list of English — "videos", "only
// photos", "screenshots", "starred" — with nothing archive-specific about them,
// so a model has no information the grammar lacks and can only disagree.
//
// Tags are absent because a tag is a hard filter over a vocabulary of thousands
// of words another model invented, and letting one model guess which of
// another's words to filter by is two guesses stacked. Tags reach a query
// through `tag:` or through the tsvector, where they are ranked rather than
// obeyed. See matchVocabulary.
//
// What is left is the three things a model can genuinely do better than a
// grammar: recognise a name spelled loosely, recognise a place spelled loosely,
// and read a date phrasing nobody thought to write a rule for.
type ModelQuery struct {
	People []string `json:"people"`
	Place  string   `json:"place"`
	After  string   `json:"after"`
	Before string   `json:"before"`
	Visual string   `json:"visual"`
}

// categories is the closed list `category:` is allowed to name, and it is
// internal/db's own. Repeated rather than imported because this package parses
// English and knows nothing about a schema; db.ErrUnknownCategory is the
// backstop if the two ever drift.
var categories = map[string]bool{
	"videos": true, "favorites": true, "live": true, "screenshots": true,
	"panoramas": true, "timelapse": true, "cinematic": true, "hdr": true,
	"archived": true,
}

// maxModelPeople bounds how many names the model may add.
//
// The number exists because of a specific, observed failure. A 0.6B model given
// a list of the archive's people as a hint returns the entire list, on every
// query, whether or not anybody was mentioned — and every name on it passes a
// vocabulary check, because they are all real people in this archive. Six
// ANDed people is a filter that matches nothing, which is §11's silent
// exclusion arriving through the front door.
//
// The mentions() gate below is what actually stops it. This is the second
// bound, for the case where a query genuinely does contain several names and
// the model has decided all of them matter.
const maxModelPeople = 3

// Merge folds a validated model parse into a grammar parse.
//
// The grammar is the floor and the model is an addition. Field by field: if the
// grammar found something, it stands; if it did not, the model's claim is
// checked against the archive and taken if it survives.
func Merge(grammar Query, model *ModelQuery, vocab Vocabulary, now time.Time) Query {
	if model == nil {
		return grammar
	}
	out := grammar
	contributed := false

	if len(out.People) == 0 {
		var people []string
		for _, name := range vocab.matchPeople(model.People) {
			// The gate, and the thing that makes a small model safe to listen
			// to at all: it may only name somebody the query actually mentions.
			// "chris" earns "Chris Morrison" because the word is there; a name
			// parroted back out of the hint list earns nothing.
			if mentions(grammar.Text, name) && len(people) < maxModelPeople {
				people = append(people, name)
			}
		}
		if len(people) > 0 {
			out.People = people
			contributed = true
		}
	}
	if out.Place == nil {
		if place := vocab.matchPlace(model.Place); place != nil && mentions(grammar.Text, place.Label()) {
			out.Place = place
			contributed = true
		}
	}
	// The two ends move together or not at all. Half a range from the model
	// beside half from the grammar is a window neither of them meant, and it is
	// the exact shape of the silent-exclusion failure §11 warns about.
	//
	// And only when the query says something temporal at all. A model with no
	// clock and a system prompt full of date fields will answer "today" to
	// "screenshots about taxes" — and a range is the one field where being
	// wrong removes photographs rather than adding them.
	if out.After == nil && out.Before == nil && temporal(grammar.Text) {
		after, before := parseModelDates(model.After, model.Before)
		if after != nil || before != nil {
			out.After, out.Before = after, before
			contributed = true
		}
	}

	if !contributed {
		return grammar
	}
	out.Source = SourceModel

	// The visual phrase is taken from the model only when the model actually
	// found structure the grammar missed — otherwise the grammar's leftover is
	// already correct — and only when every word of it was typed. That last
	// check is what stops a model that has decided the query is "about a dog"
	// from quietly replacing the phrase that goes to the encoder.
	if visual := strings.TrimSpace(model.Visual); visual != "" && typed(visual, grammar.Text) {
		out.Visual = visual
	}
	return out
}

// mentions reports whether any distinctive word of a name appears in the query.
//
// Any rather than all, because "chris" should reach "Chris Morrison" and "new
// york" should reach "New York City". Distinctive meaning three letters or
// more: matching "Bo Lago" on the word "bo" would fire on "boat", and a
// two-letter fragment is not evidence that anybody was mentioned.
func mentions(text, name string) bool {
	haystack := make(map[string]bool)
	for _, t := range tokenize(text) {
		haystack[t.word] = true
	}
	for _, t := range tokenize(name) {
		if len(t.word) >= 3 && haystack[t.word] {
			return true
		}
	}
	return false
}

// typed reports whether every word of a phrase appears in the original query.
func typed(phrase, text string) bool {
	haystack := make(map[string]bool)
	for _, t := range tokenize(text) {
		haystack[t.word] = true
	}
	for _, t := range tokenize(phrase) {
		if !haystack[t.word] {
			return false
		}
	}
	return true
}

func normalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image", "images", "photo", "photos", "still", "stills", "picture", "pictures":
		return kindImage
	case "video", "videos", "clip", "clips", "movie", "movies":
		return kindVideo
	}
	return ""
}

// parseModelDates reads the two date strings and refuses anything it cannot
// make sense of, together.
func parseModelDates(afterText, beforeText string) (*time.Time, *time.Time) {
	after, afterOK := parseModelDate(afterText, false)
	before, beforeOK := parseModelDate(beforeText, true)
	switch {
	case !afterOK && !beforeOK:
		return nil, nil
	case afterOK && beforeOK && after.After(before):
		return nil, nil
	case afterOK && beforeOK:
		return &after, &before
	case afterOK:
		return &after, nil
	default:
		return nil, &before
	}
}

// parseModelDate accepts a day, a month or a year, and widens the last two to
// whichever end of themselves is being asked for.
func parseModelDate(text string, end bool) (time.Time, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse("2006-01-02", text); err == nil {
		return day(t.Year(), t.Month(), t.Day()), plausible(t.Year())
	}
	if t, err := time.Parse("2006-01", text); err == nil {
		if end {
			return endOfMonth(t.Year(), t.Month()), plausible(t.Year())
		}
		return day(t.Year(), t.Month(), 1), plausible(t.Year())
	}
	if y, ok := yearNumber(text); ok {
		if end {
			return day(y, time.December, 31), true
		}
		return day(y, time.January, 1), true
	}
	return time.Time{}, false
}

func plausible(year int) bool { return year >= 1900 && year <= 2100 }

// matchPeople keeps the names this archive holds, spelled as it holds them.
func (v Vocabulary) matchPeople(claimed []string) []string {
	index := make(map[string]string, len(v.People))
	for _, name := range v.People {
		index[strings.ToLower(name)] = name
	}
	var out []string
	for _, name := range claimed {
		if known, ok := index[strings.ToLower(strings.TrimSpace(name))]; ok {
			out = appendUnique(out, known)
		}
	}
	return out
}

// matchPlace resolves a name to the column it belongs in, city first — the same
// precedence the grammar's longest-match gives it.
func (v Vocabulary) matchPlace(claimed string) *Place {
	claimed = strings.ToLower(strings.TrimSpace(claimed))
	if claimed == "" {
		return nil
	}
	for _, group := range [][]Place{v.Cities, v.Admin1s, v.Countries} {
		for _, p := range group {
			if strings.ToLower(p.Label()) == claimed {
				found := p
				return &found
			}
		}
	}
	return nil
}
