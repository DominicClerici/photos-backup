// Package snapchat understands the shape of a Snapchat data export: the
// memories history it ships as one JSON document, the filenames it gives the
// media, and the sidecar this archive composes from the two.
//
// It is pure parsing, for the same reason internal/takeout is. Nothing here
// touches a database, a network, or a media file's bytes — which is what lets
// the awkward parts be tested against the real filenames Snapchat produces.
//
// The awkwardness is worth stating up front, because none of it is guessable:
//
//   - The history JSON names no files. Every row is a Date, a Media Type and a
//     Location, and there is no identifier of any kind linking a row to the
//     file it describes. A Google sidecar sits beside its photo; a Snapchat
//     row sits in a list of three thousand and points at nothing.
//   - What links them is the filesystem. Snapchat sets each exported file's
//     modification time to the memory's capture instant, to the second, in
//     UTC — so the mtime is the join key, and it is the only one. See
//     CaptureKey.
//   - That makes the join fragile in a specific way: a copy that does not
//     preserve mtimes destroys the only link between a photograph and the one
//     record of where and when it was taken. The importer reads the export
//     where it was unzipped, and writes the recovered instant into the sidecar
//     so the fragility is survived once rather than depended on forever.
//   - The JPEGs carry no EXIF at all. Snapchat strips them, so for a still
//     memory the history row is not a supplement to the file's own metadata,
//     it is the entirety of it. The MP4s keep a QuickTime creation date.
//   - A memory's drawings, captions and stickers are a separate transparent
//     PNG — `-overlay.png` beside `-main.jpg` — and the flattened image a
//     person actually saw is in neither file. It has to be composed.
//   - `Latitude, Longitude: 0.0, 0.0` is how a row with no location is
//     written, so Null Island has to be read as absence, exactly as it does in
//     a Takeout's zeroed geoData.
//   - Chat media has no history document at all. A filename date and an mtime
//     is the whole of what an export says about it.
package snapchat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Source is what the archive files these under. One source for both halves of
// an export, because they are one export; which half an item came from is a
// subtype, so a query can ask for either or for both.
const Source = "snapchat"

// The subtypes this importer applies. They are the whole of how the gallery
// tells one kind of Snapchat item from another, so they are part of the
// archive's interface rather than an implementation detail.
//
// Namespaced with a `snapchat:` prefix because subtypes is a shared column:
// PhotoKit writes Apple's own vocabulary into it, and an unprefixed "overlay"
// or "chat" would collide with whatever a future source calls those.
const (
	// SubtypeMemory is a saved Memory — the export's `memories/` half, the
	// items with a history row behind them.
	SubtypeMemory = "snapchat:memory"
	// SubtypeChat is media sent or received in a conversation — the export's
	// `chat_media/` half, which no history document describes.
	SubtypeChat = "snapchat:chat"
	// SubtypeOverlay marks the transparent PNG carrying a memory's drawings,
	// captions and stickers. It is archived and linked from the photo it
	// belongs to rather than shown, so this is what keeps it out of a query
	// for photographs.
	SubtypeOverlay = "snapchat:overlay"
	// SubtypeThumbnail marks chat media Snapchat shipped as a thumbnail of
	// something else. Archived because the export offered it and the thing it
	// is a thumbnail of is usually not in the export at all.
	SubtypeThumbnail = "snapchat:thumbnail"
	// SubtypeJoined marks a recording this archive reassembled from the
	// ten-second pieces Snapchat exported it as. Nothing filters on it today;
	// it is here so that "which of my videos are not the file Snapchat gave
	// me" is a question the library can answer.
	SubtypeJoined = "snapchat:joined"
	// SubtypeDiscover marks publisher content — Daily Mail, VICE, Cosmopolitan
	// — that reached the account through Discover rather than through a person.
	// It is nobody's photograph and it is the first thing anyone trimming this
	// import will want to select.
	SubtypeDiscover = "snapchat:discover"
)

// Roles a file plays inside the export, taken from the `-main`/`-overlay`
// suffix on a memory and the `kind~` prefix on chat media.
const (
	RoleMain      = "main"
	RoleOverlay   = "overlay"
	RoleMedia     = "media"
	RoleThumbnail = "thumbnail"
	RoleMetadata  = "metadata"
	// RoleChatSnap is the `b~` prefix, which is what the bulk of chat_media
	// is. Snapchat does not say what the letter stands for; it marks an
	// ordinary snap in a conversation.
	RoleChatSnap = "b"
)

// HistoryEntry is one row of memories_history.json, reduced and kept whole.
//
// Raw travels with the reduction for the reason every sidecar in this archive
// does: the row is four fields today, the export is deleted next week, and a
// field nobody modelled is only recoverable if it was stored.
type HistoryEntry struct {
	Raw json.RawMessage

	// At is the capture instant, in UTC. It is also the join key — see
	// CaptureKey — which is why an unparseable date makes a row useless rather
	// than merely incomplete.
	At time.Time
	// MediaType is Snapchat's own word, "Image" or "Video". It is the
	// tiebreaker when two memories share a capture instant to the second.
	MediaType string
	GPSLat    *float64
	GPSLon    *float64
}

// IsVideo reports whether Snapchat called this a video.
func (e HistoryEntry) IsVideo() bool { return strings.EqualFold(e.MediaType, "Video") }

// historyDocument is the file's top level. The key has a space in it.
type historyDocument struct {
	SavedMedia []json.RawMessage `json:"Saved Media"`
}

// historyRow is one row as Snapchat writes it.
type historyRow struct {
	Date      string `json:"Date"`
	MediaType string `json:"Media Type"`
	Location  string `json:"Location"`
}

// ParseHistory reads memories_history.json into its rows, in file order.
//
// Rows with an unreadable date are returned too, with a zero At. They cannot be
// joined to a file — the date is the only thing that could join them — but they
// are still a record of a photograph, and dropping them here would mean nothing
// downstream could even report that they existed.
func ParseHistory(raw []byte) ([]HistoryEntry, error) {
	var doc historyDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse snapchat memories history: %w", err)
	}
	if doc.SavedMedia == nil {
		return nil, fmt.Errorf("parse snapchat memories history: no %q key; is this memories_history.json?", "Saved Media")
	}

	entries := make([]HistoryEntry, 0, len(doc.SavedMedia))
	for i, raw := range doc.SavedMedia {
		var row historyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("parse snapchat memories history row %d: %w", i, err)
		}
		entry := HistoryEntry{Raw: raw, MediaType: strings.TrimSpace(row.MediaType)}
		if at, ok := ParseHistoryTime(row.Date); ok {
			entry.At = at
		}
		entry.GPSLat, entry.GPSLon = ParseLocation(row.Location)
		entries = append(entries, entry)
	}
	return entries, nil
}

// historyTimeLayout is how Snapchat writes an instant: seconds, a space, and a
// literal zone name that is always UTC.
const historyTimeLayout = "2006-01-02 15:04:05 MST"

// ParseHistoryTime reads a row's Date.
//
// The zone is parsed rather than assumed, and then forced to UTC. Go's
// reference-layout parser accepts a name like "UTC" without knowing what offset
// it stands for, so a value that came back with a zero offset and a name is
// still the right instant; anything else would be a zone Snapchat has never
// been observed to write, and is refused rather than silently shifted.
func ParseHistoryTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(historyTimeLayout, value)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// locationPattern matches `Latitude, Longitude: 40.72073, -73.979485`, which is
// the only form the export has been observed to use.
var locationPattern = regexp.MustCompile(
	`Latitude,\s*Longitude:\s*(-?\d+(?:\.\d+)?)\s*,\s*(-?\d+(?:\.\d+)?)`)

// ParseLocation reads a row's Location, returning nils when there is none.
//
// Exactly zero in both axes means "no location", not the Gulf of Guinea — the
// same reading takeout applies to Google's zeroed geoData, and for the same
// reason. Snapchat writes the zeroes for every memory captured with location
// services off, which in this export is most of the first two years.
func ParseLocation(value string) (lat, lon *float64) {
	m := locationPattern.FindStringSubmatch(value)
	if m == nil {
		return nil, nil
	}
	parsedLat, errLat := strconv.ParseFloat(m[1], 64)
	parsedLon, errLon := strconv.ParseFloat(m[2], 64)
	if errLat != nil || errLon != nil {
		return nil, nil
	}
	if parsedLat == 0 && parsedLon == 0 {
		return nil, nil
	}
	return &parsedLat, &parsedLon
}

// CaptureKey reduces an instant to the string both sides of the join agree on.
//
// Seconds, because that is the precision the history document is written to. A
// file's modification time can carry more — a zip's extended timestamp field
// preserves nanoseconds — and comparing at that precision would match nothing
// at all. Truncating both sides is the whole of the join, and against the real
// export it places every one of the 2,791 exported memories.
func CaptureKey(at time.Time) string {
	return at.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// MemoryName is a filename in the export's `memories/` directory, taken apart.
//
// The layout is `2017-09-02_4c148b50-7cf5-861f-3b58-3d5822445c1b-main.jpg`: the
// date the memory was captured, Snapchat's own identifier for it, the role the
// file plays, and an extension. The identifier is what pairs a main with its
// overlay, and it is the only thing in the export that does.
type MemoryName struct {
	Date string
	// ID is Snapchat's identifier for the memory. Both halves of a
	// main/overlay pair carry the same one.
	ID string
	// Role is RoleMain or RoleOverlay.
	Role string
	Ext  string
}

// Stem is the identity a main and its overlay share, which is what a scan
// groups on.
func (n MemoryName) Stem() string { return n.Date + "_" + n.ID }

var memoryPattern = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2})_(.+)-(main|overlay)\.([A-Za-z0-9]+)$`)

// ParseMemoryName takes a memories filename apart, reporting whether it is one.
func ParseMemoryName(name string) (MemoryName, bool) {
	m := memoryPattern.FindStringSubmatch(name)
	if m == nil {
		return MemoryName{}, false
	}
	return MemoryName{Date: m[1], ID: m[2], Role: m[3], Ext: strings.ToLower(m[4])}, true
}

// ChatName is a filename in the export's `chat_media/` directory, taken apart.
//
// The layout is `2024-01-12_b~EiQSFVVSOXZVSEplcE9PZnJ4elg2alc1YhoAGg.jpg`: the
// date, the role, a tilde, an identifier, and an extension.
//
// Unlike a memory, the identifier here groups almost nothing. In the real
// export the `media~`, `thumbnail~` and `overlay~` files carry identifiers that
// do not match each other, so a chat overlay cannot be attached to the snap it
// was drawn on and a thumbnail cannot be attached to its original. That is a
// property of the export, not a gap in this parser, and it is why chat media
// imports as loose files where memories import as pairs.
type ChatName struct {
	Date string
	// Role is one of RoleChatSnap, RoleMedia, RoleThumbnail, RoleOverlay or
	// RoleMetadata.
	Role string
	// ID is Snapchat's identifier. It is occasionally empty — the export
	// contains `2018-09-23_media~.mp4` — so nothing may key on it being there.
	ID  string
	Ext string
}

var chatPattern = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2})_([A-Za-z0-9_]+)~(.*)\.([A-Za-z0-9]+)$`)

// ParseChatName takes a chat_media filename apart, reporting whether it is one.
func ParseChatName(name string) (ChatName, bool) {
	m := chatPattern.FindStringSubmatch(name)
	if m == nil {
		return ChatName{}, false
	}
	return ChatName{Date: m[1], Role: m[2], ID: m[3], Ext: strings.ToLower(m[4])}, true
}

// How a sidecar's capture time was arrived at, recorded beside it because the
// three are worth very different amounts and only the sidecar will survive to
// be asked.
const (
	// TimeFromHistory means a history row was matched and this is its Date:
	// Snapchat's own record of when the memory was captured.
	TimeFromHistory = "history"
	// TimeFromModTime means no row was matched and this is the file's
	// modification time, which Snapchat sets to the capture instant. It is the
	// same number a matched row would have given, arrived at without
	// corroboration.
	TimeFromModTime = "file-modification-time"
	// TimeFromFilename means even the mtime was unusable and this is midnight
	// on the date in the filename. It is a day, not an instant, and it is the
	// only one of the three that is a guess.
	TimeFromFilename = "filename-date"
)

// How a file was matched to a history row, or why it was not.
const (
	// MatchExact means one file and one row shared a capture instant.
	MatchExact = "exact"
	// MatchByType means several files and several rows shared an instant, and
	// Snapchat's own Image/Video classification separated them.
	MatchByType = "media-type"
	// MatchAmbiguous means several rows shared an instant and nothing
	// distinguished them. One was taken. It is recorded because the rows in
	// this position have been observed to differ in location, so an ambiguous
	// match is a coordinate that might belong to the sibling.
	MatchAmbiguous = "ambiguous"
	// MatchNone means no row had this file's capture instant.
	MatchNone = "none"
)

// Sidecar is what this importer writes into the archive for one file.
//
// It is composed rather than copied, which is a departure from how every other
// import source in this archive works and is worth being explicit about.
// Google writes a document per photograph and this archive stores it verbatim.
// Snapchat writes one document for three thousand photographs and nothing that
// says which is which, so a per-file document has to be built — and the honest
// way to build one is to keep Snapchat's own row untouched under History and
// put everything this importer worked out beside it, never inside it.
//
// So: History is Snapchat's, verbatim, byte for byte, or absent. Every other
// field is this importer's reading of the export, and is labelled as such by
// living outside History. Nothing here is ever merged into Snapchat's copy.
type Sidecar struct {
	// Export names the format, matching Source. Present so the document
	// identifies itself when it is read back out of the archive years later
	// with no context around it.
	Export string `json:"export"`
	// Kind is "memory" or "chat", the half of the export this came from.
	Kind string `json:"kind"`
	// Delivery is which of the numbered directories the export was unzipped
	// into held this file. Snapchat splits an export across several and puts
	// the history document in only one of them.
	Delivery string `json:"delivery,omitempty"`
	// File is the name Snapchat gave it, and Path is where it sat inside the
	// export.
	File string `json:"file"`
	Path string `json:"path,omitempty"`
	// MediaID is Snapchat's identifier, from the filename. For a memory it is
	// what pairs a main with its overlay.
	MediaID string `json:"mediaId,omitempty"`
	// Role is the part this file plays: RoleMain, RoleOverlay and the rest.
	Role string `json:"role,omitempty"`

	// Overlay is the filename of the transparent PNG holding this memory's
	// drawings and captions, when there is one. The flattened image a person
	// actually saw exists in neither file and is composed from both.
	Overlay string `json:"overlay,omitempty"`
	// OverlayFor is the reverse, set on the overlay itself.
	OverlayFor string `json:"overlayFor,omitempty"`

	// CapturedAt is when the memory was taken, in UTC, and CapturedAtSource
	// says how that was established — one of TimeFromHistory, TimeFromModTime,
	// TimeFromFilename. The second field is not bookkeeping: for a stripped
	// JPEG this timestamp is the only one that exists anywhere, so how much to
	// trust it is a question somebody will genuinely need answered.
	CapturedAt       *time.Time `json:"capturedAt,omitempty"`
	CapturedAtSource string     `json:"capturedAtSource,omitempty"`

	// History is the row from memories_history.json, verbatim. Absent for chat
	// media, which no history document describes, and for a memory whose row
	// could not be matched.
	History json.RawMessage `json:"history,omitempty"`
	// HistoryMatch is how History was arrived at: MatchExact, MatchByType,
	// MatchAmbiguous, MatchNone.
	HistoryMatch string `json:"historyMatch,omitempty"`

	// Publisher is the Discover metadata document that named this file's
	// publisher, verbatim, when one was found beside it. Its presence is what
	// says an item is a news clip rather than a photograph.
	Publisher json.RawMessage `json:"publisher,omitempty"`

	// Subtypes are the labels applied to this item, and they are what the
	// gallery filters on. Written into the sidecar as well as into the column
	// so that a reindex from the manifest reaches the same conclusion.
	Subtypes []string `json:"subtypes,omitempty"`

	// Joined is set on the one kind of file in this archive that Snapchat never
	// shipped: a recording it cut into ten-second pieces, put back together
	// here. See internal/merge.
	//
	// It is in the sidecar rather than in a column because of what it is for.
	// The pieces go to the trash when the join is archived and the trash empties
	// after a year, so this document eventually becomes the only account of
	// where a minute of video came from — and "which six files, and was it
	// copied or re-encoded" is exactly what somebody will want from it.
	Joined *Joined `json:"joined,omitempty"`
}

// Joined is the record of a recording reassembled from its pieces.
type Joined struct {
	// Method is "stream-copy" or "re-encode", and Reason says why when it is the
	// second. A stream copy holds the camera's own frames; a re-encode is a
	// generation further from them, and that is a fact about an archived
	// original rather than a detail of how it was made.
	Method string `json:"method"`
	Reason string `json:"reason,omitempty"`

	DurationSeconds float64   `json:"durationSeconds"`
	JoinedAt        time.Time `json:"joinedAt"`

	// ExpectedSeconds is what the parts added up to, written down only when it
	// disagrees with the line above — which is to say only when somebody
	// watched a join this archive had refused to make and said to make it
	// anyway. The disagreement is the reason the file exists in the form it
	// does, and it belongs beside the file rather than in a log nobody keeps.
	ExpectedSeconds float64 `json:"expectedSeconds,omitempty"`

	// Parts are the pieces, in the order they were joined. By digest as well as
	// by name, because the digest is what finds the blob again — and because if
	// somebody ever wants to take this apart, the parts are still exactly those
	// bytes.
	Parts []JoinedPart `json:"parts"`
}

// JoinedPart is one piece of a joined recording.
type JoinedPart struct {
	File            string    `json:"file"`
	SHA256          string    `json:"sha256"`
	CapturedAt      time.Time `json:"capturedAt"`
	DurationSeconds float64   `json:"durationSeconds"`
}

// Metadata is a sidecar reduced to the facts the archive stores.
type Metadata struct {
	Raw json.RawMessage

	TakenAt  *time.Time
	GPSLat   *float64
	GPSLon   *float64
	Subtypes []string
	// Archived keeps an item out of the timeline while leaving it in the
	// archive. It is set on an overlay, whose bytes are wanted and whose lone
	// transparent PNG in a grid of photographs is not.
	Archived bool
}

// Normalize reads one sidecar.
//
// The coordinates come from the history row and nowhere else, because nowhere
// else has them: Snapchat strips EXIF from the stills, so for a still memory
// the row is the only record that the photograph has a place at all.
func Normalize(raw []byte) (Metadata, error) {
	var s Sidecar
	if err := json.Unmarshal(raw, &s); err != nil {
		return Metadata{}, fmt.Errorf("parse snapchat sidecar: %w", err)
	}

	m := Metadata{
		Raw:      json.RawMessage(raw),
		TakenAt:  s.CapturedAt,
		Subtypes: s.Subtypes,
	}
	for _, subtype := range s.Subtypes {
		if subtype == SubtypeOverlay {
			m.Archived = true
		}
	}

	if len(s.History) > 0 {
		var row historyRow
		if err := json.Unmarshal(s.History, &row); err != nil {
			return Metadata{}, fmt.Errorf("parse snapchat sidecar history row: %w", err)
		}
		m.GPSLat, m.GPSLon = ParseLocation(row.Location)
		// The row's own Date outranks the composed CapturedAt when both are
		// there, since CapturedAt may have come from an mtime and this is
		// Snapchat saying so directly. They agree everywhere they have both
		// been observed; this settles which wins if they ever stop.
		if at, ok := ParseHistoryTime(row.Date); ok {
			m.TakenAt = &at
		}
	}
	return m, nil
}
