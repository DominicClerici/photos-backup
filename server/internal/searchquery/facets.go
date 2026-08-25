package searchquery

// The facets: media kind, category, favourites. A closed list of English rather
// than a property of this archive, which is why it lives here and the names in
// searchquery.go come from the database.

// matchFacets reads the adjectives out of a query.
func matchFacets(toks []token, q *Query) {
	var phrases []phrase

	// A video is a video. Nobody types "show me videos" meaning "show me
	// items", so this one narrows on sight.
	for _, w := range []string{"video", "videos", "clip", "clips", "movie", "movies", "footage"} {
		phrases = append(phrases, phrase{
			words: []string{w},
			apply: func(q *Query) { q.Kind = kindVideo },
		})
	}

	// "photos" is the opposite case and it is the one worth being careful
	// about. "show me photos of the beach" is a request for the beach — the
	// word is how English says "items in a photo library", and treating it as a
	// filter would silently exclude every video in the archive from a query
	// that never mentioned videos. So the bare word is filler (see the map in
	// searchquery.go) and only these spellings narrow: the ones where somebody
	// has said, in so many words, that they mean stills.
	for _, words := range [][]string{
		{"still"}, {"stills"},
		{"only", "photos"}, {"just", "photos"}, {"photos", "only"},
		{"only", "pictures"}, {"just", "pictures"}, {"pictures", "only"},
		{"only", "images"}, {"just", "images"}, {"images", "only"},
		{"no", "videos"}, {"not", "videos"}, {"without", "videos"},
		{"no", "video"}, {"excluding", "videos"},
	} {
		words := words
		phrases = append(phrases, phrase{
			words: words,
			apply: func(q *Query) { q.Kind = kindImage },
		})
	}

	// And the mirror image, for a query that says what it does not want.
	for _, words := range [][]string{
		{"only", "videos"}, {"just", "videos"}, {"videos", "only"},
		{"no", "photos"}, {"not", "photos"}, {"without", "photos"},
		{"no", "pictures"}, {"no", "stills"},
	} {
		words := words
		phrases = append(phrases, phrase{
			words: words,
			apply: func(q *Query) { q.Kind = kindVideo },
		})
	}

	for _, w := range []string{"favorite", "favorites", "favourite", "favourites", "starred", "hearted"} {
		phrases = append(phrases, phrase{
			words: []string{w},
			apply: func(q *Query) { q.Favorites = true },
		})
	}

	// The categories, spelled the ways people spell them. The keys are
	// internal/db's closed list — an unknown one is refused rather than ignored
	// there, so these have to stay in step with it.
	for _, c := range []struct {
		key       string
		spellings [][]string
	}{
		{"screenshots", [][]string{{"screenshot"}, {"screenshots"}, {"screen", "shot"}, {"screen", "shots"}}},
		{"panoramas", [][]string{{"panorama"}, {"panoramas"}, {"pano"}, {"panos"}}},
		{"timelapse", [][]string{{"timelapse"}, {"timelapses"}, {"time-lapse"}, {"time", "lapse"}}},
		{"live", [][]string{{"live", "photo"}, {"live", "photos"}}},
		{"cinematic", [][]string{{"cinematic"}}},
		{"hdr", [][]string{{"hdr"}}},
	} {
		key := c.key
		for _, words := range c.spellings {
			words := words
			phrases = append(phrases, phrase{
				words: words,
				apply: func(q *Query) {
					if q.Category == "" {
						q.Category = key
					}
				},
			})
		}
	}

	match(toks, phrases, q)
}

// The two media kinds, spelled as internal/db spells them. Repeated here rather
// than imported because this package parses English and knows nothing about a
// database; the API layer is where the two meet.
const (
	kindImage = "image"
	kindVideo = "video"
)
