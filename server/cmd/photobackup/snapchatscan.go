package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/snapchat"
)

// The two halves of a Snapchat export, which are imported separately because
// they are not the same kind of thing.
//
// Memories are photographs somebody chose to keep, and a history document says
// when and where each was taken. Chat media is whatever passed through a
// conversation — a friend's snap, a screenshot, a news clip from Discover —
// and no document describes any of it. Importing them in one run would put a
// single summary over two populations whose quality differs by an order of
// magnitude, and would make "how much of my archive is actually photographs"
// unanswerable.
const (
	halfMemories = "memories"
	halfChat     = "chat"
)

// The directories a delivery of a Snapchat export contains. An export arrives
// as several numbered zips and each one holds some subset of these.
const (
	memoriesDir = "memories"
	chatDir     = "chat_media"
	// historyPath is where the memories history document sits, relative to the
	// top of the delivery that happens to carry it — which is one delivery out
	// of six, and not the one holding most of the memories.
	historyPath = "json/memories_history.json"
)

// snapchatExport is a Snapchat export, read.
type snapchatExport struct {
	half  string
	items []*importItem

	// mains is how many items are photographs or videos in their own right,
	// and overlays how many are the drawn-on layer of one of them.
	mains    int
	overlays int
	// linkedOverlays is how many overlays found the memory they belong to.
	linkedOverlays int
	// orphanOverlays are overlays whose main file is not in the export. They
	// import as ordinary archived items, because the handwriting is somebody's
	// even when the photograph under it is missing.
	orphanOverlays []string

	// historyRows is how many rows memories_history.json held, and matches
	// counts how each file was joined to one — keyed by snapchat.MatchExact and
	// its siblings.
	historyRows int
	matches     map[string]int
	// unmatchedHistory are rows describing a memory whose file is not in the
	// export. Snapchat lists them and ships nothing, so a time and a place is
	// the entirety of what survives of the photograph.
	unmatchedHistory []unmatchedHistoryRow
	// ambiguousRisk is how many of the ambiguous matches could actually have
	// come out differently — the ones where the rows competing for a file
	// disagreed about where it was taken.
	//
	// It is the number that says what the ambiguity costs. An instant with four
	// indistinguishable rows all naming the same coordinates is a match that
	// cannot be got wrong in any way that matters; one where they disagree is a
	// photograph that may end up on the wrong street. In the real export the
	// first is 99 groups and the second is one.
	ambiguousRisk int

	// publishers is how many items were shown to be Discover content by a
	// publisher document found beside them.
	publishers int
	// skipped are files the export contains and this archive will not store:
	// voice notes, which are audio and the schema has no kind for; the
	// encrypted blobs chat_media carries that nothing has the key for; and the
	// JSON documents that describe other files.
	//
	// Itemized rather than counted because the reasons are not
	// interchangeable. "This archive cannot hold audio yet" is a decision
	// somebody can revisit; "nothing on earth can decrypt this" is not. A
	// single number would present the two as the same event.
	skipped []skippedFile
	// duplicatePaths counts files that appear under the same name in more than
	// one delivery. The first read wins.
	duplicatePaths int
	// historyRoots is which deliveries held a history document. More than one
	// is worth reporting; none, in the memories half, is fatal.
	historyRoots []string
}

// skippedFile is a file the export held and the archive is not storing.
type skippedFile struct {
	rel string
	// mimeType is what exiftool made of it, empty when it could make nothing.
	mimeType string
	// reason is why the archive will not hold it, in words, because these are
	// read later by somebody deciding whether to change that.
	reason string
	size   int64
}

// The reasons a file is skipped. They are the sidecar's own vocabulary, so a
// query can find every voice note the day the archive learns to hold one.
const (
	skipAudio       = "audio"
	skipUnreadable  = "unreadable"
	skipDescriptive = "describes-another-file"
)

// unmatchedHistoryRow is a history row with no file, kept whole.
type unmatchedHistoryRow struct {
	// locator is the row's identity inside the export. The document gives its
	// rows no ids, so this is built from the capture instant and the ordinal
	// of the row within that instant — which is stable across re-runs because
	// the document's order is.
	locator string
	raw     json.RawMessage
}

// snapchatFile is one candidate file and where it sat inside the export.
type snapchatFile struct {
	path string
	// rel is `memories/<name>` or `chat_media/<name>`, deliberately without
	// the delivery directory in front of it.
	//
	// That is the item's identity across runs, and dropping the delivery is
	// what makes it stable: Snapchat's zip numbering is an artifact of one
	// download, a second download of the same account splits the same files
	// differently, and a local id carrying `snapchat_4/` would make every item
	// look new. Filenames carry Snapchat's own identifier and do not collide
	// across deliveries — verified against the real export, in both halves.
	rel      string
	name     string
	delivery string
}

// scanSnapchatExport reads one half of an export into an ordered list of
// uploads.
//
// It takes several roots for the same reason the Takeout importer does, and a
// sharper version of the same reason: Snapchat puts the history document in one
// delivery and the memories it describes in the other five, so a run over a
// single directory either has 3,237 rows and no photographs or 2,791
// photographs and nothing that knows when any of them was taken.
func scanSnapchatExport(ctx context.Context, exif *exifdata.Reader, roots []string, half string) (snapchatExport, error) {
	export := snapchatExport{half: half, matches: map[string]int{}}

	cleaned := make([]string, 0, len(roots))
	for _, root := range roots {
		cleaned = append(cleaned, filepath.Clean(root))
	}

	files, history, historyRoots, duplicates, err := walkSnapchat(cleaned, half)
	if err != nil {
		return export, err
	}
	export.duplicatePaths = duplicates
	export.historyRoots = historyRoots

	if half == halfMemories {
		if len(history) == 0 {
			return export, fmt.Errorf(
				"none of the given directories holds %s;\n"+
					"it is in exactly one delivery of the export, and without it a memory has no\n"+
					"capture time and no location at all — pass every unzipped directory with --from",
				historyPath)
		}
		export.historyRows = len(history)
	}

	// One exiftool per directory that has files in it. It is what says which
	// files are media at all and which are video — and the second question is
	// what separates two memories that share a capture instant, so it has to be
	// answered before any row can be matched.
	identified, err := identifySnapchat(ctx, exif, cleaned, half)
	if err != nil {
		return export, err
	}

	// Publisher documents first: they are not media, so nothing else would read
	// them, and whether an item is a news clip is decided before it is built.
	publishers := readPublishers(files)

	for _, file := range files {
		info, err := os.Stat(file.path)
		if err != nil {
			return export, err
		}

		scanned, ok := identified[file.path]
		if !ok || !isMedia(scanned.MIMEType) {
			if skip, drop := classifySkip(file, scanned.MIMEType, info.Size(), ok); drop {
				export.skipped = append(export.skipped, skip)
			}
			continue
		}

		item := &importItem{
			path:     file.path,
			localID:  file.rel,
			filename: file.name,
			size:     info.Size(),
			modified: info.ModTime().UTC(),
			isVideo:  scanned.IsVideo(),
			source:   snapchat.Source,
		}
		sidecar := snapchat.Sidecar{
			Export:   snapchat.Source,
			Kind:     half,
			Delivery: file.delivery,
			File:     file.name,
			Path:     file.rel,
		}

		switch half {
		case halfMemories:
			name, ok := snapchat.ParseMemoryName(file.name)
			if !ok {
				// Media in the memories directory under a name this parser
				// does not recognise. It cannot be joined to a row or paired
				// with an overlay, so it is reported rather than imported
				// half-described — and it is the signal that the naming rules
				// have changed.
				export.skipped = append(export.skipped, skippedFile{
					rel: file.rel, mimeType: scanned.MIMEType, size: item.size,
					reason: "unrecognised memories filename"})
				continue
			}
			sidecar.MediaID = name.ID
			sidecar.Role = name.Role
			sidecar.Subtypes = []string{snapchat.SubtypeMemory}
			if name.Role == snapchat.RoleOverlay {
				sidecar.Subtypes = append(sidecar.Subtypes, snapchat.SubtypeOverlay)
			}
		case halfChat:
			name, ok := snapchat.ParseChatName(file.name)
			if !ok {
				export.skipped = append(export.skipped, skippedFile{
					rel: file.rel, mimeType: scanned.MIMEType, size: item.size,
					reason: "unrecognised chat media filename"})
				continue
			}
			sidecar.MediaID = name.ID
			sidecar.Role = name.Role
			sidecar.Subtypes = chatSubtypes(name.Role)
			// Chat media has no history document, so the file's own
			// modification time is both the best and the only answer.
			at := item.modified
			sidecar.CapturedAt, sidecar.CapturedAtSource = &at, snapchat.TimeFromModTime
			if raw, ok := publishers[name.ID]; ok {
				sidecar.Publisher = raw
				sidecar.Subtypes = append(sidecar.Subtypes, snapchat.SubtypeDiscover)
				export.publishers++
			}
		}

		item.sidecar = mustSidecar(sidecar)
		item.takenAt = sidecar.CapturedAt
		export.items = append(export.items, item)
	}

	if half == halfMemories {
		if err := matchHistory(&export, history); err != nil {
			return export, err
		}
		linkOverlays(&export)
	}

	for _, item := range export.items {
		if isOverlayItem(item) {
			export.overlays++
		} else {
			export.mains++
		}
	}

	sort.Slice(export.skipped, func(i, j int) bool { return export.skipped[i].rel < export.skipped[j].rel })
	sort.Strings(export.orphanOverlays)
	sortForPairing(export.items)
	return export, nil
}

// walkSnapchat collects the media files of one half, plus the history document
// wherever it turned up.
func walkSnapchat(roots []string, half string) (
	files []snapchatFile,
	history []snapchat.HistoryEntry,
	historyRoots []string,
	duplicates int,
	err error,
) {
	wanted := memoriesDir
	if half == halfChat {
		wanted = chatDir
	}

	seen := make(map[string]bool)
	for _, root := range roots {
		delivery := filepath.Base(root)

		if half == halfMemories {
			at := filepath.Join(root, filepath.FromSlash(historyPath))
			if raw, readErr := os.ReadFile(at); readErr == nil {
				entries, parseErr := snapchat.ParseHistory(raw)
				if parseErr != nil {
					return nil, nil, nil, 0, fmt.Errorf("%s: %w", at, parseErr)
				}
				// Appended rather than replaced. Snapchat has only ever been
				// seen to write one, but two deliveries each carrying a
				// partial document is a shape this cannot rule out, and
				// dropping the second would silently lose the memories only it
				// described.
				history = append(history, entries...)
				historyRoots = append(historyRoots, delivery)
			}
		}

		dir := filepath.Join(root, wanted)
		if _, statErr := os.Stat(dir); statErr != nil {
			continue
		}
		walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() && p != dir {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if isExportBookkeeping(d.Name()) {
				// The export writes one of these per delivery — `memories.html`
				// is in all five that hold memories — so they are neither
				// duplicates nor media, and counting them as either says
				// something untrue about the photographs.
				return nil
			}
			if seen[d.Name()] {
				// The same file in two deliveries. Snapchat's identifiers are
				// unique across an export, so a repeated name is a repeated
				// file rather than two items that happen to share one.
				duplicates++
				return nil
			}
			seen[d.Name()] = true
			files = append(files, snapchatFile{
				path:     p,
				rel:      path.Join(wanted, d.Name()),
				name:     d.Name(),
				delivery: delivery,
			})
			return nil
		})
		if walkErr != nil {
			return nil, nil, nil, 0, fmt.Errorf("walk %s: %w", dir, walkErr)
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, history, historyRoots, duplicates, nil
}

// identifySnapchat runs exiftool over the half being imported, and only over
// that half.
//
// Scoped to the one subdirectory rather than the whole delivery because the
// other half is thousands of files this run will not look at, and exiftool is
// the slowest thing in a scan.
func identifySnapchat(ctx context.Context, exif *exifdata.Reader, roots []string, half string) (map[string]exifdata.Scanned, error) {
	wanted := memoriesDir
	if half == halfChat {
		wanted = chatDir
	}

	identified := make(map[string]exifdata.Scanned)
	for _, root := range roots {
		dir := filepath.Join(root, wanted)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		err := exif.ScanTree(ctx, dir, func(s exifdata.Scanned) error {
			identified[filepath.Clean(s.Path)] = s
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("read metadata under %s: %w", dir, err)
		}
	}
	return identified, nil
}

// matchHistory joins every memory to the row that describes it.
//
// The join is the capture instant and nothing else, because the export offers
// nothing else: the history document names no files and the filenames carry no
// times beyond a date. Snapchat sets each file's modification time to the
// capture instant, so that is the key, truncated to the second on both sides
// because that is the precision the document is written to.
//
// Where several memories share an instant — 100 groups in the real export,
// covering 404 files — Snapchat's own Image/Video classification separates
// them. What it cannot separate is two stills captured in the same second, and
// those are matched in file order and marked snapchat.MatchAmbiguous. The mark
// matters: the duplicate rows have been observed to differ in location, so an
// ambiguous match is a coordinate that may belong to the sibling, and the
// sidecar says so rather than presenting the guess as a fact.
func matchHistory(export *snapchatExport, history []snapchat.HistoryEntry) error {
	// Rows by instant, in document order, so which of two indistinguishable
	// rows is taken does not come out of map iteration.
	rows := make(map[string][]*snapchat.HistoryEntry)
	var undated int
	for i := range history {
		entry := &history[i]
		if entry.At.IsZero() {
			undated++
			continue
		}
		key := snapchat.CaptureKey(entry.At)
		rows[key] = append(rows[key], entry)
	}

	// Only the mains are joined. An overlay is a layer of a memory that is
	// itself in the export and about to be matched, so letting one consume a
	// row would leave the photograph it belongs to unmatched.
	byKey := make(map[string][]*importItem)
	var mains []*importItem
	for _, item := range export.items {
		if isOverlayItem(item) {
			continue
		}
		mains = append(mains, item)
		byKey[snapchat.CaptureKey(item.modified)] = append(byKey[snapchat.CaptureKey(item.modified)], item)
	}
	sort.Slice(mains, func(i, j int) bool { return mains[i].localID < mains[j].localID })

	claimed := make(map[*snapchat.HistoryEntry]bool)
	for key, items := range byKey {
		sort.Slice(items, func(i, j int) bool { return items[i].localID < items[j].localID })
		candidates := rows[key]

		// How many files and how many rows of each kind share this instant.
		// The match is only as good as what actually narrowed it to one, and
		// these two counts are what say which of the three happened.
		countItems := func(video bool) int {
			n := 0
			for _, item := range items {
				if item.isVideo == video {
					n++
				}
			}
			return n
		}
		countRows := func(video bool) int {
			n := 0
			for _, entry := range candidates {
				if entry.IsVideo() == video {
					n++
				}
			}
			return n
		}

		// Snapchat's own classification first, which is what resolves an
		// instant holding one photo and one video.
		for _, item := range items {
			for _, entry := range candidates {
				if claimed[entry] || entry.IsVideo() != item.isVideo {
					continue
				}
				claimed[entry] = true
				applyHistory(item, entry, matchKind(
					len(items), len(candidates),
					countItems(item.isVideo), countRows(item.isVideo)))
				break
			}
		}
		// Then anything left, which is two memories Snapchat called the same
		// thing in the same second.
		for _, item := range items {
			if item.sidecarMatched() {
				continue
			}
			for _, entry := range candidates {
				if claimed[entry] {
					continue
				}
				claimed[entry] = true
				applyHistory(item, entry, snapchat.MatchAmbiguous)
				break
			}
		}
	}

	// Everything that matched nothing falls back to its own modification time,
	// which is the same instant a row would have supplied — Snapchat wrote it
	// there — arrived at without corroboration and marked as such.
	for _, item := range mains {
		if item.sidecarMatched() {
			continue
		}
		applyHistory(item, nil, snapchat.MatchNone)
	}
	for _, item := range mains {
		export.matches[itemMatch(item)]++
	}
	export.ambiguousRisk = countAmbiguousRisk(byKey, rows)

	// The rows nobody claimed. Snapchat lists a memory in the history and ships
	// no file for it — 443 of 3,237 in the real export — and what is left is a
	// time and a place for a photograph that is not here. That is worth
	// keeping: it is a record of where somebody was, and the export is deleted
	// the week after the import.
	ordinal := map[string]int{}
	for i := range history {
		entry := &history[i]
		if claimed[entry] {
			continue
		}
		key := "undated"
		if !entry.At.IsZero() {
			key = snapchat.CaptureKey(entry.At)
		}
		locator := fmt.Sprintf("%s#%s", historyPath, key)
		if n := ordinal[key]; n > 0 {
			locator = fmt.Sprintf("%s.%d", locator, n)
		}
		ordinal[key]++
		export.unmatchedHistory = append(export.unmatchedHistory,
			unmatchedHistoryRow{locator: locator, raw: entry.Raw})
	}
	sort.Slice(export.unmatchedHistory, func(i, j int) bool {
		return export.unmatchedHistory[i].locator < export.unmatchedHistory[j].locator
	})
	return nil
}

// countAmbiguousRisk counts the files whose ambiguous match could have landed
// somewhere else.
//
// An ambiguous match is only a problem if the rows it chose between disagree.
// Where every row at an instant names the same coordinates — which is 99 of the
// 100 contested instants in the real export — taking the wrong one is taking a
// row that says the same thing, and the ambiguity is bookkeeping rather than a
// photograph in the wrong place.
func countAmbiguousRisk(byKey map[string][]*importItem, rows map[string][]*snapchat.HistoryEntry) int {
	risk := 0
	for key, items := range byKey {
		candidates := rows[key]
		if len(items) <= 1 && len(candidates) <= 1 {
			continue
		}
		places := make(map[string]bool)
		for _, entry := range candidates {
			lat, lon := entry.GPSLat, entry.GPSLon
			if lat == nil || lon == nil {
				places["none"] = true
				continue
			}
			places[fmt.Sprintf("%f,%f", *lat, *lon)] = true
		}
		if len(places) <= 1 {
			continue
		}
		for _, item := range items {
			if itemMatch(item) == snapchat.MatchAmbiguous {
				risk++
			}
		}
	}
	return risk
}

// matchKind grades a join by what actually narrowed it to one row.
//
// The grade is the honest part of the whole match, so it is worked out from
// counts rather than from which branch of the loop happened to run. Landing in
// the type-matching pass is not evidence that the type disambiguated anything:
// two stills captured in the same second put both rows in that pass and neither
// is distinguishable from the other.
//
//   - Exact: one file, one row, nothing to choose between.
//   - ByType: several of each, and among files and rows of this media type
//     there was exactly one of each — Snapchat's Image/Video split settled it.
//   - Ambiguous: anything else. A row was taken and it may be the sibling's.
func matchKind(items, rows, sameTypeItems, sameTypeRows int) string {
	switch {
	case items == 1 && rows == 1:
		return snapchat.MatchExact
	case sameTypeItems == 1 && sameTypeRows == 1:
		return snapchat.MatchByType
	default:
		return snapchat.MatchAmbiguous
	}
}

// applyHistory writes a matched row, or the lack of one, into an item's
// sidecar.
func applyHistory(item *importItem, entry *snapchat.HistoryEntry, match string) {
	sidecar := readSidecar(item)
	sidecar.HistoryMatch = match

	if entry != nil {
		at := entry.At
		sidecar.History = entry.Raw
		sidecar.CapturedAt, sidecar.CapturedAtSource = &at, snapchat.TimeFromHistory
	} else {
		at := item.modified
		sidecar.CapturedAt, sidecar.CapturedAtSource = &at, snapchat.TimeFromModTime
	}

	item.sidecar = mustSidecar(sidecar)
	item.takenAt = sidecar.CapturedAt
}

// linkOverlays attaches each overlay to the memory it was drawn on.
//
// They are related by sharing the date-and-identifier stem of their filenames,
// which is the only record of it anywhere: the files carry no metadata linking
// them and the history document does not mention overlays at all.
func linkOverlays(export *snapchatExport) {
	mains := make(map[string]*importItem)
	for _, item := range export.items {
		if isOverlayItem(item) {
			continue
		}
		if name, ok := snapchat.ParseMemoryName(item.filename); ok {
			mains[name.Stem()] = item
		}
	}

	for _, item := range export.items {
		if !isOverlayItem(item) {
			continue
		}
		name, ok := snapchat.ParseMemoryName(item.filename)
		if !ok {
			continue
		}
		main, ok := mains[name.Stem()]
		if !ok {
			// An overlay whose photograph is not in the export. It imports
			// anyway: it is somebody's handwriting, the bytes exist nowhere
			// else, and an archive that dropped it would be deciding that a
			// layer is worthless without the layer under it.
			export.orphanOverlays = append(export.orphanOverlays, item.localID)
			continue
		}

		overlaySidecar := readSidecar(item)
		overlaySidecar.OverlayFor = main.filename
		// The overlay borrows the memory's capture time, and its provenance
		// with it. On its own an overlay has only its own modification time,
		// which Snapchat sets to the same instant — but saying "this is the
		// history row's time" is only true because the file it belongs to
		// matched one, so the mark is copied rather than asserted.
		overlaySidecar.CapturedAt = readSidecar(main).CapturedAt
		overlaySidecar.CapturedAtSource = readSidecar(main).CapturedAtSource
		item.sidecar = mustSidecar(overlaySidecar)
		item.takenAt = overlaySidecar.CapturedAt

		mainSidecar := readSidecar(main)
		mainSidecar.Overlay = item.filename
		main.sidecar = mustSidecar(mainSidecar)

		// The upload phase reads this to decide the order: an overlay has to be
		// archived before the memory that names it can be described.
		main.overlayItem = item
		export.linkedOverlays++
	}
}

// readPublishers finds the Discover metadata documents chat_media carries and
// indexes them by the identifier of the media they describe.
//
// They are the one thing in the chat half that distinguishes a news clip from a
// photograph. There are seven of them against four thousand files, so this
// labels a fraction of the publisher content rather than all of it — what it
// labels, it labels on evidence.
func readPublishers(files []snapchatFile) map[string]json.RawMessage {
	publishers := make(map[string]json.RawMessage)
	for _, file := range files {
		name, ok := snapchat.ParseChatName(file.name)
		if !ok || name.Role != snapchat.RoleMetadata || name.ID == "" {
			continue
		}
		raw, err := os.ReadFile(file.path)
		if err != nil || !json.Valid(raw) {
			continue
		}
		publishers[name.ID] = json.RawMessage(raw)
	}
	return publishers
}

// chatSubtypes labels a chat file by the part it plays.
func chatSubtypes(role string) []string {
	subtypes := []string{snapchat.SubtypeChat}
	switch role {
	case snapchat.RoleOverlay:
		subtypes = append(subtypes, snapchat.SubtypeOverlay)
	case snapchat.RoleThumbnail:
		subtypes = append(subtypes, snapchat.SubtypeThumbnail)
	}
	return subtypes
}

// classifySkip says why a file is not becoming an asset, or that it is not
// worth mentioning.
//
// The export's own index pages are not mentioned: they describe the export
// rather than anything in it, and reporting them as losses would bury the three
// that are real.
func classifySkip(file snapchatFile, mimeType string, size int64, identified bool) (skippedFile, bool) {
	if isExportBookkeeping(file.name) {
		return skippedFile{}, false
	}

	skip := skippedFile{rel: file.rel, mimeType: mimeType, size: size}
	switch {
	case strings.HasPrefix(strings.ToLower(mimeType), "audio/"):
		// Voice notes. The bytes are here and the archive has nowhere to put
		// them: media_kind is checked to be image or video, and every stage
		// after the upload — thumbnails, poster frames, the gallery tile —
		// assumes something can be drawn. Recorded so the decision to hold
		// audio can be made against a list rather than a guess.
		skip.reason = skipAudio
		skip.mimeType = mimeType
		return skip, true

	case !identified || mimeType == "":
		// Snapchat ships some chat media still encrypted, and no key for it is
		// in the export or anywhere else. Nothing can be done with these, which
		// is exactly why the fact that they existed is worth keeping.
		skip.reason = skipUnreadable
		return skip, true

	case strings.Contains(strings.ToLower(mimeType), "json"):
		// A publisher document or a Discover layout. Already read, and already
		// attached to the media it describes where there was any.
		skip.reason = skipDescriptive
		return skip, true
	}

	skip.reason = skipUnreadable
	return skip, true
}

// isExportBookkeeping reports whether a non-media file is one the export writes
// about itself, rather than something that failed to be read.
func isExportBookkeeping(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".html")
}

// readSidecar reads back the document built for an item.
//
// The sidecar is carried as bytes because that is what is sent and what is
// archived, and it is amended several times during a scan — a history row, then
// an overlay, then the overlay's own back-reference. Round-tripping it keeps
// one representation rather than a struct and a copy of it that can disagree.
func readSidecar(item *importItem) snapchat.Sidecar {
	var s snapchat.Sidecar
	if len(item.sidecar) > 0 {
		_ = json.Unmarshal(item.sidecar, &s)
	}
	return s
}

// sidecarMatched reports whether a history row has already been applied.
func (it *importItem) sidecarMatched() bool {
	return readSidecar(it).HistoryMatch != ""
}

func itemMatch(item *importItem) string { return readSidecar(item).HistoryMatch }

// isOverlayItem reports whether an item is a drawn-on layer rather than a
// photograph.
func isOverlayItem(item *importItem) bool {
	for _, subtype := range readSidecar(item).Subtypes {
		if subtype == snapchat.SubtypeOverlay {
			return true
		}
	}
	return false
}

// mustSidecar marshals a sidecar this program built.
//
// A failure here is not an export being malformed, it is this code having put
// something unencodable in a struct of strings and times, so it is turned into
// a document that says so rather than an error every caller would have to
// thread through a scan. It has no way to happen and no way to be silent.
func mustSidecar(s snapchat.Sidecar) json.RawMessage {
	raw, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(fmt.Sprintf(
			`{"export":%q,"error":"this importer could not encode the sidecar: %s"}`,
			snapchat.Source, strings.ReplaceAll(err.Error(), `"`, `'`)))
	}
	return raw
}
