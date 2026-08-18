package snapchat

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseHistory(t *testing.T) {
	raw := []byte(`{"Saved Media":[
		{"Date":"2026-08-14 04:32:03 UTC","Media Type":"Video",
		 "Location":"Latitude, Longitude: 40.72073, -73.979485",
		 "Download Link":"","Media Download Url":""},
		{"Date":"2017-09-11 16:11:41 UTC","Media Type":"Image",
		 "Location":"Latitude, Longitude: 0.0, 0.0",
		 "Download Link":"","Media Download Url":""}
	]}`)

	entries, err := ParseHistory(raw)
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	first := entries[0]
	if want := time.Date(2026, 8, 14, 4, 32, 3, 0, time.UTC); !first.At.Equal(want) {
		t.Errorf("first row At = %s, want %s", first.At, want)
	}
	if !first.IsVideo() {
		t.Error("first row is a Video and did not say so")
	}
	if first.GPSLat == nil || *first.GPSLat != 40.72073 {
		t.Errorf("first row latitude = %v, want 40.72073", first.GPSLat)
	}

	// The whole row survives, not just the fields this package models. It is
	// the only copy once the export is deleted.
	var round map[string]any
	if err := json.Unmarshal(first.Raw, &round); err != nil {
		t.Fatalf("raw row is not valid JSON: %v", err)
	}
	if _, ok := round["Media Download Url"]; !ok {
		t.Error("a field this package does not model was dropped from Raw")
	}

	// Null Island is absence, not a place off the coast of Africa. Most of the
	// export's first two years is this.
	if entries[1].GPSLat != nil || entries[1].GPSLon != nil {
		t.Errorf("zeroed coordinates read as a location: %v, %v",
			entries[1].GPSLat, entries[1].GPSLon)
	}
}

// The document's one key has a space in it, and a file without it is not a
// memories history — most likely somebody pointed the importer at the wrong
// JSON, which is worth saying rather than importing zero rows silently.
func TestParseHistoryRejectsAnotherDocument(t *testing.T) {
	if _, err := ParseHistory([]byte(`{"Chat History":[]}`)); err == nil {
		t.Fatal("a document with no Saved Media key parsed as a memories history")
	}
}

// A row whose date cannot be read is kept. It can never be joined to a file —
// the date is the only thing that could join it — but it is still a record that
// a photograph existed, and dropping it here would leave nothing able to report
// that it was there.
func TestParseHistoryKeepsUndatedRows(t *testing.T) {
	entries, err := ParseHistory([]byte(`{"Saved Media":[{"Date":"","Media Type":"Image","Location":""}]}`))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if !entries[0].At.IsZero() {
		t.Errorf("an unreadable date produced a time: %s", entries[0].At)
	}
}

func TestParseHistoryTime(t *testing.T) {
	at, ok := ParseHistoryTime("2018-01-05 23:14:07 UTC")
	if !ok {
		t.Fatal("a well-formed date did not parse")
	}
	if at.Location() != time.UTC {
		t.Errorf("parsed time is in %s, want UTC", at.Location())
	}
	if want := time.Date(2018, 1, 5, 23, 14, 7, 0, time.UTC); !at.Equal(want) {
		t.Errorf("got %s, want %s", at, want)
	}

	for _, bad := range []string{"", "   ", "2018-01-05", "not a date"} {
		if _, ok := ParseHistoryTime(bad); ok {
			t.Errorf("%q parsed as a date", bad)
		}
	}
}

func TestParseLocation(t *testing.T) {
	lat, lon := ParseLocation("Latitude, Longitude: 39.161533, -86.532104")
	if lat == nil || lon == nil {
		t.Fatal("a well-formed location did not parse")
	}
	if *lat != 39.161533 || *lon != -86.532104 {
		t.Errorf("got %v, %v", *lat, *lon)
	}

	for _, none := range []string{
		"Latitude, Longitude: 0.0, 0.0",
		"Latitude, Longitude: 0, 0",
		"",
		"somewhere",
	} {
		if lat, lon := ParseLocation(none); lat != nil || lon != nil {
			t.Errorf("%q read as a location: %v, %v", none, lat, lon)
		}
	}
}

func TestParseMemoryName(t *testing.T) {
	// Both halves of a pair, from the real export. They share a stem, and that
	// is the only thing anywhere that relates them.
	main, ok := ParseMemoryName("2017-09-02_4c148b50-7cf5-861f-3b58-3d5822445c1b-main.jpg")
	if !ok {
		t.Fatal("a real memories filename did not parse")
	}
	overlay, ok := ParseMemoryName("2017-09-02_4c148b50-7cf5-861f-3b58-3d5822445c1b-overlay.png")
	if !ok {
		t.Fatal("a real overlay filename did not parse")
	}

	if main.Role != RoleMain || overlay.Role != RoleOverlay {
		t.Errorf("roles = %q, %q", main.Role, overlay.Role)
	}
	if main.Ext != "jpg" || overlay.Ext != "png" {
		t.Errorf("extensions = %q, %q", main.Ext, overlay.Ext)
	}
	if main.Stem() != overlay.Stem() {
		t.Errorf("a main and its overlay have different stems: %q and %q", main.Stem(), overlay.Stem())
	}
	if main.ID != "4c148b50-7cf5-861f-3b58-3d5822445c1b" {
		t.Errorf("id = %q", main.ID)
	}

	// Uppercase identifiers appear in the later years of the export.
	if _, ok := ParseMemoryName("2024-08-05_39484843-B114-4072-AA62-A1EE7454BD30-overlay.png"); !ok {
		t.Error("an uppercase identifier did not parse")
	}
	for _, bad := range []string{
		"memories.html",
		"2017-09-02_4c148b50-main",
		"4c148b50-main.jpg",
	} {
		if _, ok := ParseMemoryName(bad); ok {
			t.Errorf("%q parsed as a memories filename", bad)
		}
	}
}

func TestParseChatName(t *testing.T) {
	cases := []struct {
		name string
		role string
		id   string
		ext  string
	}{
		{"2024-01-12_b~EiQSFVVSOXZVSEplcE9PZnJ4elg2alc1YhoAGgAyAQNIAlAEYAE.jpg",
			RoleChatSnap, "EiQSFVVSOXZVSEplcE9PZnJ4elg2alc1YhoAGgAyAQNIAlAEYAE", "jpg"},
		{"2024-01-12_media~zip-48E9429E-9BA3-4B7D-ADD2-6C7213BD7D9F.mp4",
			RoleMedia, "zip-48E9429E-9BA3-4B7D-ADD2-6C7213BD7D9F", "mp4"},
		{"2024-01-12_thumbnail~zip-CEE7DCE1-A710-4636-A215-4122640921C1.jpg",
			RoleThumbnail, "zip-CEE7DCE1-A710-4636-A215-4122640921C1", "jpg"},
		{"2018-09-14_metadata~zip-3B15689D-14D0-4F16-A07B-DA551F061AB6.unknown",
			RoleMetadata, "zip-3B15689D-14D0-4F16-A07B-DA551F061AB6", "unknown"},
		// The export really does contain these: a role, a tilde, and no
		// identifier at all. Nothing may key on the identifier being there.
		{"2018-09-23_media~.mp4", RoleMedia, "", "mp4"},
	}

	for _, tc := range cases {
		got, ok := ParseChatName(tc.name)
		if !ok {
			t.Errorf("%q did not parse", tc.name)
			continue
		}
		if got.Role != tc.role || got.ID != tc.id || got.Ext != tc.ext {
			t.Errorf("%q parsed as %+v, want role %q id %q ext %q",
				tc.name, got, tc.role, tc.id, tc.ext)
		}
	}
}

// The join between a file and the row describing it is the capture second and
// nothing else, so both sides have to reduce to the same string. A file's
// modification time can carry sub-second precision that the history document,
// written to the second, never will.
func TestCaptureKeyIgnoresSubSecondPrecision(t *testing.T) {
	fromHistory := time.Date(2017, 8, 27, 5, 42, 30, 0, time.UTC)
	fromFile := time.Date(2017, 8, 27, 5, 42, 30, 837_000_000, time.UTC)

	if CaptureKey(fromHistory) != CaptureKey(fromFile) {
		t.Errorf("a file and its row did not join: %q vs %q",
			CaptureKey(fromHistory), CaptureKey(fromFile))
	}

	// Different zones, same instant, same key: the file's mtime arrives in
	// whatever zone the reader is in and the document is always UTC.
	east := time.FixedZone("UTC+2", 2*60*60)
	if got := CaptureKey(fromHistory.In(east)); got != CaptureKey(fromHistory) {
		t.Errorf("the same instant in another zone keyed differently: %q", got)
	}
}

func TestNormalize(t *testing.T) {
	at := time.Date(2017, 9, 2, 6, 55, 44, 0, time.UTC)
	sidecar := Sidecar{
		Export:           Source,
		Kind:             "memory",
		File:             "2017-09-02_4c148b50-main.jpg",
		Role:             RoleMain,
		Overlay:          "2017-09-02_4c148b50-overlay.png",
		CapturedAt:       &at,
		CapturedAtSource: TimeFromHistory,
		HistoryMatch:     MatchExact,
		History: json.RawMessage(`{"Date":"2017-09-02 06:55:44 UTC","Media Type":"Image",` +
			`"Location":"Latitude, Longitude: 39.161533, -86.532104"}`),
		Subtypes: []string{SubtypeMemory},
	}
	raw, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}

	meta, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if meta.TakenAt == nil || !meta.TakenAt.Equal(at) {
		t.Errorf("TakenAt = %v, want %s", meta.TakenAt, at)
	}
	// The coordinates come from the history row and from nowhere else. Snapchat
	// strips EXIF from the stills, so for a still memory this is the only
	// record that the photograph has a place at all.
	if meta.GPSLat == nil || *meta.GPSLat != 39.161533 {
		t.Errorf("GPSLat = %v, want 39.161533", meta.GPSLat)
	}
	if meta.Archived {
		t.Error("a memory was archived; only its overlay should be")
	}
}

// An overlay is archived on the strength of its subtype alone. It is a
// transparent PNG of somebody's handwriting, and the archive holds it without
// the gallery drawing it as though it were a photograph.
func TestNormalizeArchivesOverlays(t *testing.T) {
	raw, err := json.Marshal(Sidecar{
		Export:   Source,
		Kind:     "memory",
		Role:     RoleOverlay,
		Subtypes: []string{SubtypeMemory, SubtypeOverlay},
	})
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}

	meta, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !meta.Archived {
		t.Error("an overlay was not archived")
	}
	if len(meta.Subtypes) != 2 {
		t.Errorf("subtypes = %v, want both the memory and the overlay label", meta.Subtypes)
	}
}

// Chat media has no history row, so the composed capture time is all there is
// and it has to survive normalization intact.
func TestNormalizeChatWithoutHistory(t *testing.T) {
	at := time.Date(2024, 1, 12, 11, 29, 2, 0, time.UTC)
	raw, err := json.Marshal(Sidecar{
		Export:           Source,
		Kind:             "chat",
		Role:             RoleChatSnap,
		CapturedAt:       &at,
		CapturedAtSource: TimeFromModTime,
		Subtypes:         []string{SubtypeChat},
	})
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}

	meta, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if meta.TakenAt == nil || !meta.TakenAt.Equal(at) {
		t.Errorf("TakenAt = %v, want %s", meta.TakenAt, at)
	}
	if meta.GPSLat != nil || meta.GPSLon != nil {
		t.Error("chat media produced coordinates; no export contains any")
	}
}
