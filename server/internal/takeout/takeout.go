// Package takeout understands the shape of a Google Photos export: the sidecar
// JSON written beside each file, the rules by which a sidecar's name relates to
// the file it describes, and the directory layout that encodes albums.
//
// It is pure parsing. Nothing here touches a database, a network, or a media
// file's bytes — which is what lets the awkward parts, and they are all awkward,
// be tested against the real filenames Google produces rather than reasoned
// about.
//
// The awkwardness is worth stating up front, because none of it is guessable:
//
//   - A sidecar is named `<media>.supplemental-metadata.json`, except that the
//     whole name is capped at 51 characters, so the suffix is truncated to
//     whatever fits — `.supplemental-met.json`, `.supple.json`, `.s.json` — and
//     when even that will not fit, the media name is truncated instead. Older
//     exports use a bare `<media>.json`.
//   - Filename collisions within a folder get a `(1)` counter, and on the
//     sidecar the counter moves to the end of the whole name: `IMG_1.HEIC(1)`
//     describes `IMG_1(1).HEIC`.
//   - A Live Photo's video half has no sidecar at all. Google treats the pair as
//     one item and writes one sidecar, against the still.
//   - `geoData` is present but all zeroes when there are no coordinates, so
//     Null Island has to be read as absence.
package takeout

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sidecar is the subset of Google's per-item JSON worth naming. The rest is
// preserved verbatim by the caller rather than modelled here — see Metadata.Raw.
type Sidecar struct {
	// Title is the filename Google Photos held the item under. It is the most
	// reliable link back to the media file, because it is data rather than an
	// artifact of how the export named things.
	Title       string `json:"title"`
	Description string `json:"description"`

	PhotoTakenTime  *Timestamp `json:"photoTakenTime"`
	CreationTime    *Timestamp `json:"creationTime"`
	LastModifiedRaw *Timestamp `json:"photoLastModifiedTime"`

	// GeoData is Google's copy of the coordinates, which a user may have
	// corrected; GeoDataExif is what the file itself claimed. Both are written
	// as zeroes rather than omitted when there is no location.
	GeoData     *GeoData `json:"geoData"`
	GeoDataExif *GeoData `json:"geoDataExif"`

	Favorited bool `json:"favorited"`
	Archived  bool `json:"archived"`
	Trashed   bool `json:"trashed"`
	InTrash   bool `json:"inTrash"`

	People []Person `json:"people"`

	URL string `json:"url"`
}

type Person struct {
	Name string `json:"name"`
}

// Timestamp is Google's two-field time: seconds since the epoch as a string,
// plus a rendering of it for humans. Only the first is read.
type Timestamp struct {
	Timestamp string `json:"timestamp"`
	Formatted string `json:"formatted"`
}

// Time parses the epoch seconds, returning nil for the absent and the
// unparseable alike.
func (t *Timestamp) Time() *time.Time {
	if t == nil || t.Timestamp == "" {
		return nil
	}
	seconds, err := strconv.ParseInt(t.Timestamp, 10, 64)
	if err != nil || seconds <= 0 {
		return nil
	}
	at := time.Unix(seconds, 0).UTC()
	return &at
}

type GeoData struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Altitude  float64 `json:"altitude"`
}

// Coordinates returns the position, or nils when there is none.
//
// Exactly zero in both axes means "no location" here, not the Gulf of Guinea.
// Google writes the zeroes unconditionally, so reading them literally would put
// a large fraction of any export — every screenshot, everything with location
// services off — on a single point off the coast of Africa.
func (g *GeoData) Coordinates() (lat, lon *float64) {
	if g == nil || (g.Latitude == 0 && g.Longitude == 0) {
		return nil, nil
	}
	return &g.Latitude, &g.Longitude
}

// Metadata is a sidecar reduced to the facts the archive stores, plus the
// sidecar itself.
type Metadata struct {
	Raw json.RawMessage

	Description string
	Favorite    bool
	Archived    bool
	Trashed     bool

	TakenAt *time.Time
	GPSLat  *float64
	GPSLon  *float64

	People []string
}

// Parse reads one sidecar.
func Parse(raw []byte) (Sidecar, error) {
	var s Sidecar
	if err := json.Unmarshal(raw, &s); err != nil {
		return Sidecar{}, fmt.Errorf("parse takeout sidecar: %w", err)
	}
	return s, nil
}

// Normalize reduces a sidecar to what the archive keeps.
//
// The capture time prefers photoTakenTime over creationTime, and the difference
// matters: creationTime is when the file reached Google, which for anything
// uploaded in a backfill is years after the photo was taken. The sample export
// has a March photo with an April creationTime and a January photo with a
// creationTime three days later — sorting a library by that would scramble it.
func Normalize(raw []byte) (Metadata, error) {
	s, err := Parse(raw)
	if err != nil {
		return Metadata{}, err
	}

	m := Metadata{
		Raw:         json.RawMessage(raw),
		Description: strings.TrimSpace(s.Description),
		Favorite:    s.Favorited,
		Archived:    s.Archived,
		Trashed:     s.Trashed || s.InTrash,
		TakenAt:     s.PhotoTakenTime.Time(),
	}
	if m.TakenAt == nil {
		m.TakenAt = s.CreationTime.Time()
	}

	// Google's copy first: it is the one a correction in the Photos UI lands
	// on, and it falls back to the file's own value when nothing corrected it.
	if m.GPSLat, m.GPSLon = s.GeoData.Coordinates(); m.GPSLat == nil {
		m.GPSLat, m.GPSLon = s.GeoDataExif.Coordinates()
	}

	for _, p := range s.People {
		if name := strings.TrimSpace(p.Name); name != "" {
			m.People = append(m.People, name)
		}
	}
	return m, nil
}

// Album is a directory's album descriptor, read from the metadata.json Google
// writes at the top of each album folder.
type Album struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// albumDescriptors are the names Google gives a directory-level metadata file.
// The bare `metadata.json` is current; older exports name it after the album.
var albumDescriptors = map[string]bool{
	"metadata.json": true,
}

// directoryJSON are the export's own bookkeeping files, which sit beside the
// media and describe nothing in it.
var directoryJSON = map[string]bool{
	"print-subscriptions.json":          true,
	"shared_album_comments.json":        true,
	"user-generated-memory-titles.json": true,
}

// IsDirectoryJSON reports whether a .json file describes the directory rather
// than an item in it.
func IsDirectoryJSON(name string) bool {
	return albumDescriptors[name] || directoryJSON[name]
}

// IsAlbumDescriptor reports whether a .json file is a directory's album header.
func IsAlbumDescriptor(name string) bool { return albumDescriptors[name] }

// datedFolder matches the buckets Google files loose photos into. Anything else
// at that level is a user-made album, which is the whole basis on which album
// membership is recovered — the export records it nowhere else.
//
// Localized exports name these differently ("Fotos von 2025"), so the year is
// what is really being matched and the prefix is only a guard against an album
// literally named "2025".
var datedFolder = regexp.MustCompile(`^(?i)(photos?|fotos?|billeder|foto's) (from|von|van|de|del|da|di|fra|från|fra) \d{4}$`)

// IsDatedFolder reports whether a directory name is one of the export's
// chronological buckets rather than an album.
func IsDatedFolder(name string) bool {
	return datedFolder.MatchString(strings.TrimSpace(name))
}

// counterSuffix matches the `(1)` a collision within a folder appends.
var counterSuffix = regexp.MustCompile(`\((\d+)\)$`)

// MediaNameFor reduces a sidecar's filename to the media filenames it could
// describe, best guess first.
//
// It exists because the obvious answer — trim `.json` — is right only for the
// oldest exports. What a current one produces has been truncated to fit a
// length cap and may have had a collision counter migrated to the end of it, so
// this returns the small set of names those transformations could have come
// from and lets the caller check which one is on disk.
func MediaNameFor(sidecarName string) []string {
	base := strings.TrimSuffix(sidecarName, ".json")
	if base == sidecarName {
		return nil
	}

	var candidates []string
	add := func(name string) {
		if name == "" {
			return
		}
		for _, existing := range candidates {
			if existing == name {
				return
			}
		}
		candidates = append(candidates, name)
	}

	// `IMG_1.HEIC.supplemental-metadata(1).json` describes `IMG_1(1).HEIC`: the
	// counter belongs to the media file and was moved to the end when the
	// suffix was appended. Undone before the suffix is stripped, because the
	// suffix is what it is sitting behind.
	counter := ""
	if m := counterSuffix.FindString(base); m != "" {
		counter = m
		base = strings.TrimSuffix(base, m)
	}

	stripped := trimSupplementalSuffix(base)
	add(withCounter(stripped, counter))
	if stripped != base {
		add(withCounter(base, counter))
	}
	// A sidecar for a file that already had no extension, and the bare
	// `<media>.json` form of older exports, both land here unchanged.
	add(base)
	return candidates
}

// trimSupplementalSuffix removes the `.supplemental-metadata` segment, however
// much of it survived the length cap.
func trimSupplementalSuffix(base string) string {
	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return base
	}
	segment := base[dot+1:]
	if segment == "" || !strings.HasPrefix("supplemental-metadata", segment) {
		return base
	}
	// A real extension can also be a prefix of it — "s" is, and so is "su" —
	// but neither is a media extension this archive would ever see, and a
	// sidecar named `photo.s.json` is not a thing Google writes.
	return base[:dot]
}

// withCounter puts a collision counter back where it belongs: before the
// extension, which is where the file on disk has it.
func withCounter(name, counter string) string {
	if counter == "" {
		return name
	}
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + counter + ext
}
