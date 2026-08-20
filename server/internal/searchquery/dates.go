package searchquery

import (
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Dates are the half of this parser that can be wrong in a way nobody sees.
//
// A misread name produces no results and says so — the chip reads "Phoenix" and
// the grid is empty, and the mistake is one click from fixed. A misread date
// produces *fewer* results, which looks exactly like a library that does not
// contain the photograph. So every rule below leans wide: a window that is a
// fortnight too generous costs a handful of extra tiles at the bottom of a
// ranked page, and a window a fortnight too tight costs the answer.
//
// That is why Christmas is a week, why summer runs to the end of September, and
// why a season that has not finished yet is never what "last" means.

// temporal reports whether a query says anything about time at all.
//
// The gate on the model's date range, and it exists because of an observed
// failure rather than a theoretical one: a small model handed a system prompt
// full of date fields answers "today" to "screenshots about taxes". A range is
// the one thing a parser can get wrong that *removes* photographs, so it has to
// be about something.
//
// Deliberately generous — a digit anywhere, a month, or any of the words below
// — because it is only ever reached when the grammar has already failed to read
// a date. Something temporal is in there; the grammar just had no rule for the
// phrasing, which is exactly the case the model is worth asking about.
func temporal(text string) bool {
	for _, t := range tokenize(text) {
		if strings.ContainsFunc(t.word, unicode.IsDigit) {
			return true
		}
		if _, ok := months[t.word]; ok {
			return true
		}
		if temporalWords[t.word] {
			return true
		}
	}
	return false
}

var temporalWords = map[string]bool{
	"today": true, "yesterday": true, "tomorrow": true, "tonight": true,
	"last": true, "this": true, "next": true, "past": true, "previous": true,
	"recent": true, "recently": true, "ago": true, "since": true, "until": true,
	"before": true, "after": true, "during": true, "between": true,
	"day": true, "days": true, "night": true, "nights": true,
	"week": true, "weeks": true, "weekend": true, "weekends": true,
	"month": true, "months": true, "year": true, "years": true,
	"decade": true, "decades": true, "season": true,
	"summer": true, "winter": true, "spring": true, "fall": true, "autumn": true,
	"christmas": true, "xmas": true, "thanksgiving": true, "halloween": true,
	"easter": true, "birthday": true, "birthdays": true, "anniversary": true,
	"holiday": true, "holidays": true, "vacation": true, "eve": true,
	"early": true, "late": true, "old": true, "older": true, "new": true,
	"newest": true, "oldest": true, "when": true, "back": true, "then": true,
}

// span is an inclusive range of civil days, in UTC.
//
// UTC rather than the viewer's zone, and inclusive rather than half-open,
// because both ends of this are shown to a person: "Jun–Sep 2025" is a chip
// somebody reads and removes. An exclusive end would put a month in the chip
// that was not in the question.
type span struct{ from, to time.Time }

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// endOfMonth is the last day of the month a date falls in.
func endOfMonth(y int, m time.Month) time.Time {
	return day(y, m, 1).AddDate(0, 1, -1)
}

// parseDates reads every date expression in a query and folds them into one
// range.
//
// Several expressions widen rather than conflict: "2019 and 2021" asks about
// both, and answering with the span that covers them is the generous reading of
// a question nobody quite finished asking. The operators — before, since,
// between — are the exception and set one end outright, because they were typed
// precisely.
func parseDates(toks []token, q *Query, now time.Time) {
	utc := now.UTC()
	today := day(utc.Year(), utc.Month(), utc.Day())

	// Seeded from whatever the explicit syntax already settled, so
	// `after:2019-01-01 last summer` narrows twice rather than the second
	// reading discarding the first.
	after, before := q.After, q.Before
	setAfter := func(t time.Time) { v := t; after = &v }
	setBefore := func(t time.Time) { v := t; before = &v }
	widen := func(s span) {
		if after == nil || s.from.Before(*after) {
			setAfter(s.from)
		}
		if before == nil || s.to.After(*before) {
			setBefore(s.to)
		}
	}
	use := func(from, to int) {
		for n := from; n < to; n++ {
			toks[n].used = true
		}
	}

	for i := 0; i < len(toks); {
		if toks[i].used {
			i++
			continue
		}

		// An operator claims the expression that follows it and sets one end of
		// the range outright. Where nothing follows that reads as a date, the
		// word was not an operator — "after school" — and it falls through to
		// the bare match below and then out into the visual phrase.
		if end, ok := readOperator(toks, i, today, setAfter, setBefore); ok {
			use(i, end)
			i = end
			continue
		}

		if s, width, ok := readSpan(toks, i, today); ok {
			widen(s)
			use(i, i+width)
			i += width
			continue
		}
		i++
	}

	// A range that runs backwards is a parse that went wrong, and half of a
	// wrong range is worse than none: it would silently exclude the whole
	// archive while showing a chip that looks reasonable. Dropping both is the
	// honest failure, and the visual half of the query still answers.
	if after != nil && before != nil && after.After(*before) {
		return
	}
	q.After, q.Before = after, before
}

// readOperator handles before / until / since / after / between, and reports
// where the expression it claimed ends.
func readOperator(toks []token, i int, today time.Time, setAfter, setBefore func(time.Time)) (int, bool) {
	switch toks[i].word {
	case "between":
		s1, w1, ok := readSpan(toks, i+1, today)
		if !ok {
			return 0, false
		}
		j := i + 1 + w1
		if j >= len(toks) || (toks[j].word != "and" && toks[j].word != "to") {
			return 0, false
		}
		s2, w2, ok := readSpan(toks, j+1, today)
		if !ok {
			return 0, false
		}
		setAfter(s1.from)
		setBefore(s2.to)
		return j + 1 + w2, true

	case "before":
		// Strictly before, which is what the word means: "before 2020" is not a
		// question about 2020.
		if s, w, ok := readSpan(toks, i+1, today); ok {
			setBefore(s.from.AddDate(0, 0, -1))
			return i + 1 + w, true
		}

	case "until", "till", "til", "through", "thru":
		// Inclusive, which is what these mean instead.
		if s, w, ok := readSpan(toks, i+1, today); ok {
			setBefore(s.to)
			return i + 1 + w, true
		}

	case "since":
		if s, w, ok := readSpan(toks, i+1, today); ok {
			setAfter(s.from)
			return i + 1 + w, true
		}

	case "after":
		if s, w, ok := readSpan(toks, i+1, today); ok {
			setAfter(s.to.AddDate(0, 0, 1))
			return i + 1 + w, true
		}
	}
	return 0, false
}

// readSpan reads one date expression beginning at i, and reports how many
// tokens it took.
//
// The order of the attempts is the disambiguation. A holiday is tried before a
// month so "july fourth" is a day rather than a month; a relative phrase is
// tried before either so "last june" is one expression rather than a stray word
// and a month.
func readSpan(toks []token, i int, today time.Time) (span, int, bool) {
	if i >= len(toks) || toks[i].used {
		return span{}, 0, false
	}

	switch toks[i].word {
	case "today":
		return span{today, today}, 1, true
	case "yesterday":
		y := today.AddDate(0, 0, -1)
		return span{y, y}, 1, true
	}

	if s, w, ok := readRelative(toks, i, today); ok {
		return s, w, true
	}
	if s, w, ok := readHoliday(toks, i, today); ok {
		return s, w, true
	}
	if s, w, ok := readSeason(toks, i, today, ""); ok {
		return s, w, true
	}
	if s, w, ok := readMonth(toks, i, today, ""); ok {
		return s, w, true
	}
	return readNumeric(toks, i)
}

// readRelative handles "last summer", "this year", "past three months".
//
// Two shapes with deliberately different meanings. "last <unit>" is the
// previous *calendar* unit — last month is the month before this one, whole,
// which is what somebody flipping back through a library means. "past N units"
// is a rolling window ending today, which is what somebody asking about the
// recent past means. They agree closely enough that nobody notices, and where
// they differ each is right for the phrasing that produces it.
func readRelative(toks []token, i int, today time.Time) (span, int, bool) {
	qualifier := toks[i].word
	switch qualifier {
	case "last", "this", "past", "previous", "next":
	default:
		return span{}, 0, false
	}
	if i+1 >= len(toks) || toks[i+1].used {
		return span{}, 0, false
	}

	// "last 3 months", "past two weeks".
	if n, ok := numberWord(toks[i+1].word); ok && i+2 < len(toks) {
		if s, ok := rollingWindow(toks[i+2].word, n, today, qualifier == "next"); ok {
			return s, 3, true
		}
		return span{}, 0, false
	}

	unit := toks[i+1].word
	monday := weekStart(today)

	switch unit {
	case "night":
		// "last night" is yesterday evening, and a photograph taken at 1am
		// belongs to it as much as one taken at 11pm. Two days rather than one,
		// for the same reason everything else here is generous.
		if qualifier == "last" {
			return span{today.AddDate(0, 0, -1), today}, 2, true
		}
	case "week":
		switch qualifier {
		case "this":
			return span{monday, monday.AddDate(0, 0, 6)}, 2, true
		case "next":
			return span{monday.AddDate(0, 0, 7), monday.AddDate(0, 0, 13)}, 2, true
		case "past":
			return span{today.AddDate(0, 0, -7), today}, 2, true
		default:
			return span{monday.AddDate(0, 0, -7), monday.AddDate(0, 0, -1)}, 2, true
		}
	case "weekend":
		switch qualifier {
		case "this", "next":
			start := monday.AddDate(0, 0, 5)
			if qualifier == "next" {
				start = start.AddDate(0, 0, 7)
			}
			return span{start, start.AddDate(0, 0, 1)}, 2, true
		default:
			start := monday.AddDate(0, 0, -2)
			return span{start, start.AddDate(0, 0, 1)}, 2, true
		}
	case "month":
		y, m := today.Year(), today.Month()
		switch qualifier {
		case "this":
		case "next":
			y, m = shiftMonth(y, m, 1)
		case "past":
			return span{today.AddDate(0, -1, 0), today}, 2, true
		default:
			y, m = shiftMonth(y, m, -1)
		}
		return span{day(y, m, 1), endOfMonth(y, m)}, 2, true
	case "year":
		y := today.Year()
		switch qualifier {
		case "this":
		case "next":
			y++
		case "past":
			return span{today.AddDate(-1, 0, 0), today}, 2, true
		default:
			y--
		}
		return span{day(y, time.January, 1), day(y, time.December, 31)}, 2, true
	case "decade":
		if qualifier == "last" {
			y := (today.Year()/10)*10 - 10
			return span{day(y, time.January, 1), day(y+9, time.December, 31)}, 2, true
		}
	case "summer", "winter", "spring", "fall", "autumn":
		if s, w, ok := readSeason(toks, i+1, today, qualifier); ok {
			return s, w + 1, true
		}
	}

	if s, w, ok := readMonth(toks, i+1, today, qualifier); ok {
		return s, w + 1, true
	}
	return span{}, 0, false
}

// rollingWindow is "the last N weeks", counted back from today.
func rollingWindow(unit string, n int, today time.Time, forward bool) (span, bool) {
	var end time.Time
	switch strings.TrimSuffix(unit, "s") {
	case "day":
		end = today.AddDate(0, 0, n)
	case "week":
		end = today.AddDate(0, 0, 7*n)
	case "month":
		end = today.AddDate(0, n, 0)
	case "year":
		end = today.AddDate(n, 0, 0)
	default:
		return span{}, false
	}
	if forward {
		return span{today, end}, true
	}
	return span{today.AddDate(0, 0, -int(end.Sub(today).Hours()/24)), today}, true
}

// seasons, as this archive's hemisphere experiences them and wider than an
// almanac would have them.
//
// Summer runs to the end of September because a photograph taken over Labor Day
// weekend is a summer photograph to everyone who was there, and because
// ML_IMAGES.md §7's own worked example says Jun–Sep. Winter is named by the
// year it ends in — "winter 2020" is the December of 2019 through the March of
// 2020 — which is how everybody says it and is why its span crosses a new year.
func seasonSpan(name string, year int) span {
	switch name {
	case "spring":
		return span{day(year, time.March, 1), day(year, time.June, 15)}
	case "summer":
		return span{day(year, time.June, 1), day(year, time.September, 30)}
	case "fall", "autumn":
		return span{day(year, time.September, 1), day(year, time.December, 15)}
	default: // winter
		return span{day(year-1, time.December, 1), day(year, time.March, 31)}
	}
}

// ambiguousSeason is the two that are also ordinary English. "Spring" is a coil
// and a water source, "fall" is what happens on ice — and a vision model writes
// about both. Neither narrows a query on its own; with a year or a "last" in
// front of it, the sentence has said what it meant.
var ambiguousSeason = map[string]bool{"spring": true, "fall": true, "autumn": true}

func readSeason(toks []token, i int, today time.Time, qualifier string) (span, int, bool) {
	if i >= len(toks) || toks[i].used {
		return span{}, 0, false
	}
	name := toks[i].word
	switch name {
	case "summer", "winter", "spring", "fall", "autumn":
	default:
		return span{}, 0, false
	}

	year, extra, explicit := trailingYear(toks, i+1)
	if !explicit && qualifier == "" && ambiguousSeason[name] {
		return span{}, 0, false
	}

	if !explicit {
		year = resolveYear(today, qualifier, func(y int) span { return seasonSpan(name, y) })
	}
	return seasonSpan(name, year), 1 + extra, true
}

func readMonth(toks []token, i int, today time.Time, qualifier string) (span, int, bool) {
	if i >= len(toks) || toks[i].used {
		return span{}, 0, false
	}
	month, ok := months[toks[i].word]
	if !ok {
		return span{}, 0, false
	}

	// A day of the month, if one follows: "june 5", "june 5th".
	width := 1
	dayOfMonth := 0
	if i+1 < len(toks) && !toks[i+1].used {
		if n, ok := ordinal(toks[i+1].word); ok && n >= 1 && n <= 31 {
			dayOfMonth = n
			width++
		}
	}

	year, extra, explicit := trailingYear(toks, i+width)
	width += extra

	// "May" is the month and it is also the commonest modal verb in English.
	// Without a year or a qualifier in front of it, a bare one is far more
	// likely to be "photos that may have..." than a question about a month, and
	// guessing wrong hides eleven months of the archive.
	if !explicit && qualifier == "" && toks[i].word == "may" {
		return span{}, 0, false
	}

	if !explicit {
		year = resolveYear(today, qualifier, func(y int) span {
			return span{day(y, month, 1), endOfMonth(y, month)}
		})
	}

	if dayOfMonth > 0 {
		d := day(year, month, dayOfMonth)
		return span{d, d}, width, true
	}
	return span{day(year, month, 1), endOfMonth(year, month)}, width, true
}

// resolveYear picks which occurrence of a recurring window was meant.
//
// "last summer" is the most recent one that is *over*. In August, this year's
// summer is still happening and is not what "last" means — it is what "this"
// means, and the distinction is the difference between finding the beach
// photographs and finding none. A bare "summer" is the most recent one that has
// begun, because somebody mid-August asking about the summer means this one.
func resolveYear(today time.Time, qualifier string, at func(int) span) int {
	year := today.Year()
	switch qualifier {
	case "this":
		return year
	case "next":
		return year + 1
	case "last", "previous":
		if !at(year).to.Before(today) {
			year--
		}
		return year
	default:
		if at(year).from.After(today) {
			year--
		}
		return year
	}
}

// trailingYear reads an explicit year sitting after an expression, skipping the
// "of" in "summer of 2019".
func trailingYear(toks []token, i int) (year, width int, ok bool) {
	if i < len(toks) && !toks[i].used && toks[i].word == "of" {
		if y, _, found := trailingYear(toks, i+1); found {
			return y, 2, true
		}
		return 0, 0, false
	}
	if i >= len(toks) || toks[i].used {
		return 0, 0, false
	}
	if y, ok := yearNumber(toks[i].word); ok {
		return y, 1, true
	}
	return 0, 0, false
}

// readNumeric handles the spellings that are already dates: 2019, 2019-06,
// 2019-06-01, and the decades.
func readNumeric(toks []token, i int) (span, int, bool) {
	w := toks[i].word

	if t, err := time.Parse("2006-01-02", w); err == nil {
		d := day(t.Year(), t.Month(), t.Day())
		return span{d, d}, 1, true
	}
	if t, err := time.Parse("2006-01", w); err == nil {
		return span{day(t.Year(), t.Month(), 1), endOfMonth(t.Year(), t.Month())}, 1, true
	}

	// "2010s", and "90s" for the decade somebody means rather than the year 90.
	if strings.HasSuffix(w, "s") {
		stem := strings.TrimSuffix(w, "s")
		if y, ok := decade(stem); ok {
			return span{day(y, time.January, 1), day(y+9, time.December, 31)}, 1, true
		}
	}

	if y, ok := yearNumber(w); ok {
		return span{day(y, time.January, 1), day(y, time.December, 31)}, 1, true
	}
	return span{}, 0, false
}

// yearNumber accepts four digits inside the range a photograph could plausibly
// carry. Narrow on purpose: a bare number in a query is far more often a count
// or part of a phrase than a year, and 1826 is not a date this archive holds.
func yearNumber(w string) (int, bool) {
	if len(w) != 4 {
		return 0, false
	}
	n, err := strconv.Atoi(w)
	if err != nil || n < 1900 || n > 2100 {
		return 0, false
	}
	return n, true
}

func decade(stem string) (int, bool) {
	n, err := strconv.Atoi(stem)
	if err != nil {
		return 0, false
	}
	switch {
	case len(stem) == 4 && n >= 1900 && n <= 2090 && n%10 == 0:
		return n, true
	case len(stem) == 2 && n%10 == 0:
		// "90s" is the 1990s and "20s" is the 2020s, which is what everybody
		// means and stops meaning around 2050. Good enough for a photo library.
		if n >= 50 {
			return 1900 + n, true
		}
		return 2000 + n, true
	}
	return 0, false
}

// ordinal reads "5" and "5th" alike.
func ordinal(w string) (int, bool) {
	for _, suffix := range []string{"st", "nd", "rd", "th"} {
		w = strings.TrimSuffix(w, suffix)
	}
	n, err := strconv.Atoi(w)
	if err != nil {
		return 0, false
	}
	return n, true
}

// numberWord reads the small counts people type in front of a unit.
func numberWord(w string) (int, bool) {
	if n, err := strconv.Atoi(w); err == nil && n > 0 && n < 200 {
		return n, true
	}
	spelled := map[string]int{
		"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
		"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
		"couple": 2, "few": 3, "several": 4,
	}
	n, ok := spelled[w]
	return n, ok
}

var months = map[string]time.Month{
	"january": time.January, "jan": time.January,
	"february": time.February, "feb": time.February,
	"march": time.March, "mar": time.March,
	"april": time.April, "apr": time.April,
	"may":  time.May,
	"june": time.June, "jun": time.June,
	"july": time.July, "jul": time.July,
	"august": time.August, "aug": time.August,
	"september": time.September, "sep": time.September, "sept": time.September,
	"october": time.October, "oct": time.October,
	"november": time.November, "nov": time.November,
	"december": time.December, "dec": time.December,
}

// weekStart is the Monday of the week a date falls in.
func weekStart(t time.Time) time.Time {
	return t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
}

func shiftMonth(y int, m time.Month, by int) (int, time.Month) {
	t := day(y, m, 1).AddDate(0, by, 0)
	return t.Year(), t.Month()
}

// holidays are the days a library is actually organised around, each of them a
// window rather than a date.
//
// The window is the point. Nobody photographs Christmas only on the 25th —
// there is the tree going up, the drive, the morning, the day after — and a
// search for "christmas 2019" that returned only the 25th would be a search
// that appeared to have lost most of Christmas. Easter is deliberately absent:
// it moves by a rule this parser has no business containing, and a wrong
// fortnight is worse than sending the words to the embedding.
var holidays = []struct {
	words []string
	at    func(year int) span
}{
	{[]string{"christmas", "eve"}, func(y int) span { return span{day(y, time.December, 23), day(y, time.December, 24)} }},
	{[]string{"christmas"}, func(y int) span { return span{day(y, time.December, 20), day(y, time.December, 26)} }},
	{[]string{"xmas"}, func(y int) span { return span{day(y, time.December, 20), day(y, time.December, 26)} }},
	{[]string{"thanksgiving"}, thanksgiving},
	{[]string{"halloween"}, func(y int) span { return span{day(y, time.October, 28), day(y, time.November, 1)} }},
	// Named by the year it arrives in, so "new year 2020" is the eve of 2019
	// through the first days of 2020 — which is the party everybody means.
	{[]string{"new", "years", "eve"}, func(y int) span { return span{day(y-1, time.December, 30), day(y, time.January, 2)} }},
	{[]string{"new", "years"}, func(y int) span { return span{day(y-1, time.December, 30), day(y, time.January, 2)} }},
	{[]string{"new", "year"}, func(y int) span { return span{day(y-1, time.December, 30), day(y, time.January, 2)} }},
	{[]string{"nye"}, func(y int) span { return span{day(y-1, time.December, 30), day(y, time.January, 2)} }},
	{[]string{"fourth", "of", "july"}, july4},
	{[]string{"4th", "of", "july"}, july4},
	{[]string{"july", "fourth"}, july4},
	{[]string{"july", "4th"}, july4},
	{[]string{"independence", "day"}, july4},
	{[]string{"valentines", "day"}, valentines},
	{[]string{"valentines"}, valentines},
	{[]string{"valentine"}, valentines},
}

func july4(y int) span { return span{day(y, time.July, 2), day(y, time.July, 5)} }

func valentines(y int) span {
	return span{day(y, time.February, 13), day(y, time.February, 15)}
}

// thanksgiving is the fourth Thursday of November, plus the long weekend that
// is the actual reason there are photographs.
func thanksgiving(y int) span {
	first := day(y, time.November, 1)
	offset := (int(time.Thursday) - int(first.Weekday()) + 7) % 7
	fourth := first.AddDate(0, 0, offset+21)
	return span{fourth.AddDate(0, 0, -1), fourth.AddDate(0, 0, 3)}
}

func readHoliday(toks []token, i int, today time.Time) (span, int, bool) {
	for _, h := range holidays {
		if !fits(toks, i, h.words) {
			continue
		}
		width := len(h.words)
		year, extra, explicit := trailingYear(toks, i+width)
		if !explicit {
			year = resolveYear(today, "", h.at)
		}
		return h.at(year), width + extra, true
	}
	return span{}, 0, false
}
