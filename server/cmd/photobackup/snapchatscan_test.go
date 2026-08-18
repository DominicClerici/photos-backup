package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/snapchat"
)

// A one-pixel JPEG and a one-pixel PNG, so exiftool has real containers to
// classify. The scan's decisions turn on what it says a file is.
var (
	tinyJPEG = []byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01,
		0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xDB, 0x00, 0x43,
		0x00, 0x03, 0x02, 0x02, 0x02, 0x02, 0x02, 0x03, 0x02, 0x02, 0x02, 0x03,
		0x03, 0x03, 0x03, 0x04, 0x06, 0x04, 0x04, 0x04, 0x04, 0x04, 0x08, 0x06,
		0x06, 0x05, 0x06, 0x09, 0x08, 0x0A, 0x0A, 0x09, 0x08, 0x09, 0x09, 0x0A,
		0x0C, 0x0F, 0x0C, 0x0A, 0x0B, 0x0E, 0x0B, 0x09, 0x09, 0x0D, 0x11, 0x0D,
		0x0E, 0x0F, 0x10, 0x10, 0x11, 0x10, 0x0A, 0x0C, 0x12, 0x13, 0x12, 0x10,
		0x13, 0x0F, 0x10, 0x10, 0x10, 0xFF, 0xC9, 0x00, 0x0B, 0x08, 0x00, 0x01,
		0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xFF, 0xCC, 0x00, 0x06, 0x00, 0x10,
		0x10, 0x05, 0xFF, 0xDA, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3F, 0x00,
		0xD2, 0xCF, 0x20, 0xFF, 0xD9,
	}
	tinyPNG = []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
		0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}
)

// writeAt writes a file and stamps it with the modification time Snapchat would
// have given it. The stamp is not decoration: it is the entire join between a
// photograph and the record of where and when it was taken.
func writeAt(t *testing.T, path string, body []byte, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func writeHistory(t *testing.T, root string, rows ...string) {
	t.Helper()
	body := `{"Saved Media":[` + joinRows(rows) + `]}`
	path := filepath.Join(root, filepath.FromSlash(historyPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func joinRows(rows []string) string {
	out := ""
	for i, row := range rows {
		if i > 0 {
			out += ","
		}
		out += row
	}
	return out
}

func row(date, mediaType, location string) string {
	return `{"Date":"` + date + `","Media Type":"` + mediaType +
		`","Location":"` + location + `","Download Link":"","Media Download Url":""}`
}

func exifReader(t *testing.T) *exifdata.Reader {
	t.Helper()
	if _, err := os.Stat("/usr/bin/exiftool"); err != nil {
		t.Skip("exiftool is not installed")
	}
	return &exifdata.Reader{}
}

func sidecarOf(t *testing.T, item *importItem) snapchat.Sidecar {
	t.Helper()
	var s snapchat.Sidecar
	if err := json.Unmarshal(item.sidecar, &s); err != nil {
		t.Fatalf("sidecar of %s is not valid JSON: %v", item.localID, err)
	}
	return s
}

func itemNamed(items []*importItem, name string) *importItem {
	for _, item := range items {
		if item.filename == name {
			return item
		}
	}
	return nil
}

// The whole point of the memories importer, end to end: the history document
// is in one delivery, the media in another, and the modification time is what
// puts them back together.
func TestScanSnapchatMemoriesJoinsAcrossDeliveries(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()
	first, second := filepath.Join(dir, "snapchat_1"), filepath.Join(dir, "snapchat_2")

	at := time.Date(2017, 9, 2, 6, 55, 44, 0, time.UTC)
	writeHistory(t, first,
		row("2017-09-02 06:55:44 UTC", "Image", "Latitude, Longitude: 39.161533, -86.532104"))
	writeAt(t, filepath.Join(second, memoriesDir, "2017-09-02_abc-main.jpg"), tinyJPEG, at)

	export, err := scanSnapchatExport(context.Background(), exif,
		[]string{first, second}, halfMemories)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(export.items) != 1 {
		t.Fatalf("got %d items, want 1", len(export.items))
	}
	item := export.items[0]
	if item.localID != "memories/2017-09-02_abc-main.jpg" {
		// The delivery directory is deliberately not part of the id: a second
		// download of the same account splits the files differently.
		t.Errorf("localID = %q", item.localID)
	}
	if item.source != snapchat.Source {
		t.Errorf("source = %q, want %q", item.source, snapchat.Source)
	}

	s := sidecarOf(t, item)
	if s.HistoryMatch != snapchat.MatchExact {
		t.Errorf("historyMatch = %q, want %q", s.HistoryMatch, snapchat.MatchExact)
	}
	if s.CapturedAtSource != snapchat.TimeFromHistory {
		t.Errorf("capturedAtSource = %q, want %q", s.CapturedAtSource, snapchat.TimeFromHistory)
	}
	if s.CapturedAt == nil || !s.CapturedAt.Equal(at) {
		t.Errorf("capturedAt = %v, want %s", s.CapturedAt, at)
	}
	if len(s.History) == 0 {
		t.Fatal("the matched row was not kept in the sidecar")
	}
	if item.takenAt == nil || !item.takenAt.Equal(at) {
		t.Errorf("takenAt = %v, want %s", item.takenAt, at)
	}
	if export.matches[snapchat.MatchExact] != 1 {
		t.Errorf("match counts = %v", export.matches)
	}
	if len(export.unmatchedHistory) != 0 {
		t.Errorf("a matched row was also reported as an orphan: %v", export.unmatchedHistory)
	}
}

// Without the history document there is no capture time and no location for
// anything, so pointing the importer at the wrong subset of the deliveries has
// to fail loudly rather than import three thousand undated files.
func TestScanSnapchatMemoriesRefusesWithoutHistory(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()
	writeAt(t, filepath.Join(dir, memoriesDir, "2017-09-02_abc-main.jpg"), tinyJPEG,
		time.Date(2017, 9, 2, 6, 55, 44, 0, time.UTC))

	_, err := scanSnapchatExport(context.Background(), exif, []string{dir}, halfMemories)
	if err == nil {
		t.Fatal("a scan with no history document succeeded")
	}
}

// Two memories in the same second, one a photo and one a video: Snapchat's own
// classification is what tells the rows apart.
func TestScanSnapchatSeparatesSameSecondByMediaType(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()

	at := time.Date(2019, 1, 5, 12, 0, 0, 0, time.UTC)
	writeHistory(t, dir,
		row("2019-01-05 12:00:00 UTC", "Video", "Latitude, Longitude: 1.5, 2.5"),
		row("2019-01-05 12:00:00 UTC", "Image", "Latitude, Longitude: 3.5, 4.5"))
	writeAt(t, filepath.Join(dir, memoriesDir, "2019-01-05_aaa-main.jpg"), tinyJPEG, at)

	export, err := scanSnapchatExport(context.Background(), exif, []string{dir}, halfMemories)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(export.items) != 1 {
		t.Fatalf("got %d items, want 1", len(export.items))
	}

	s := sidecarOf(t, export.items[0])
	if s.HistoryMatch != snapchat.MatchByType {
		t.Fatalf("historyMatch = %q, want %q", s.HistoryMatch, snapchat.MatchByType)
	}

	// The still must have taken the Image row. Taking the Video row would put
	// this photograph 200 miles from where it was taken, silently.
	var got map[string]string
	if err := json.Unmarshal(s.History, &got); err != nil {
		t.Fatal(err)
	}
	if got["Media Type"] != "Image" {
		t.Errorf("a still claimed the %q row", got["Media Type"])
	}

	// The video's row had no file, so it survives as an orphan rather than
	// being dropped.
	if len(export.unmatchedHistory) != 1 {
		t.Fatalf("got %d orphan rows, want 1", len(export.unmatchedHistory))
	}
}

// An overlay is linked to the memory it was drawn on, borrows its capture time,
// and is labelled so the timeline knows not to draw it.
func TestScanSnapchatLinksOverlays(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()

	at := time.Date(2018, 2, 7, 3, 4, 5, 0, time.UTC)
	writeHistory(t, dir, row("2018-02-07 03:04:05 UTC", "Image", "Latitude, Longitude: 0.0, 0.0"))
	writeAt(t, filepath.Join(dir, memoriesDir, "2018-02-07_xyz-main.jpg"), tinyJPEG, at)
	// The overlay's own mtime is deliberately different, to prove the capture
	// time is inherited from the memory rather than read off the layer.
	writeAt(t, filepath.Join(dir, memoriesDir, "2018-02-07_xyz-overlay.png"), tinyPNG,
		at.Add(90*time.Second))

	export, err := scanSnapchatExport(context.Background(), exif, []string{dir}, halfMemories)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if export.linkedOverlays != 1 {
		t.Fatalf("linkedOverlays = %d, want 1", export.linkedOverlays)
	}
	if export.mains != 1 || export.overlays != 1 {
		t.Errorf("mains = %d, overlays = %d, want 1 and 1", export.mains, export.overlays)
	}

	main := itemNamed(export.items, "2018-02-07_xyz-main.jpg")
	overlay := itemNamed(export.items, "2018-02-07_xyz-overlay.png")
	if main == nil || overlay == nil {
		t.Fatal("a half of the pair is missing from the scan")
	}

	if main.overlayItem != overlay {
		t.Error("the memory does not point at its overlay")
	}
	mainSidecar, overlaySidecar := sidecarOf(t, main), sidecarOf(t, overlay)
	if mainSidecar.Overlay != overlay.filename {
		t.Errorf("main sidecar overlay = %q, want %q", mainSidecar.Overlay, overlay.filename)
	}
	if overlaySidecar.OverlayFor != main.filename {
		t.Errorf("overlay sidecar overlayFor = %q, want %q", overlaySidecar.OverlayFor, main.filename)
	}
	if overlaySidecar.CapturedAt == nil || !overlaySidecar.CapturedAt.Equal(at) {
		t.Errorf("overlay capturedAt = %v, want the memory's %s", overlaySidecar.CapturedAt, at)
	}
	if !isOverlayItem(overlay) || isOverlayItem(main) {
		t.Error("the overlay label is on the wrong file")
	}
}

// An overlay whose memory is not in the export still imports. The bytes exist
// nowhere else, and an archive that dropped a layer for want of the layer under
// it would be deciding somebody's handwriting is worthless.
func TestScanSnapchatKeepsOrphanOverlays(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()

	at := time.Date(2024, 8, 5, 1, 2, 3, 0, time.UTC)
	writeHistory(t, dir, row("2024-08-05 01:02:03 UTC", "Image", ""))
	writeAt(t, filepath.Join(dir, memoriesDir, "2024-08-05_lonely-overlay.png"), tinyPNG, at)

	export, err := scanSnapchatExport(context.Background(), exif, []string{dir}, halfMemories)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(export.orphanOverlays) != 1 {
		t.Fatalf("orphanOverlays = %v, want one", export.orphanOverlays)
	}
	if len(export.items) != 1 {
		t.Fatalf("the orphan overlay was dropped: %d items", len(export.items))
	}
	if export.linkedOverlays != 0 {
		t.Errorf("linkedOverlays = %d, want 0", export.linkedOverlays)
	}
}

// A row Snapchat listed and shipped no file for. 443 of the real export's 3,237
// are these, and a time and a place is all that survives of each.
func TestScanSnapchatKeepsRowsWithNoFile(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()

	writeHistory(t, dir,
		row("2020-05-05 05:05:05 UTC", "Image", "Latitude, Longitude: 9.5, 8.5"),
		row("2020-05-05 05:05:05 UTC", "Image", "Latitude, Longitude: 7.5, 6.5"))
	writeAt(t, filepath.Join(dir, memoriesDir, "2020-05-05_one-main.jpg"), tinyJPEG,
		time.Date(2020, 5, 5, 5, 5, 5, 0, time.UTC))

	export, err := scanSnapchatExport(context.Background(), exif, []string{dir}, halfMemories)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Two rows, one file, nothing to tell them apart: one row is taken and the
	// match is marked ambiguous rather than presented as a fact.
	if export.matches[snapchat.MatchAmbiguous] != 1 {
		t.Errorf("match counts = %v, want one ambiguous", export.matches)
	}
	if len(export.unmatchedHistory) != 1 {
		t.Fatalf("got %d orphan rows, want 1", len(export.unmatchedHistory))
	}
	orphan := export.unmatchedHistory[0]
	if orphan.locator == "" {
		t.Error("an orphan row has no locator, so a re-import would add a second copy")
	}
	if len(orphan.raw) == 0 {
		t.Error("an orphan row was recorded without the row")
	}
}

// A memory with no row at all falls back to its own modification time, which is
// the same instant Snapchat would have reported, and says so.
func TestScanSnapchatFallsBackToModTime(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()

	at := time.Date(2021, 3, 3, 3, 3, 3, 0, time.UTC)
	writeHistory(t, dir, row("1999-01-01 00:00:00 UTC", "Image", ""))
	writeAt(t, filepath.Join(dir, memoriesDir, "2021-03-03_nomatch-main.jpg"), tinyJPEG, at)

	export, err := scanSnapchatExport(context.Background(), exif, []string{dir}, halfMemories)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if export.matches[snapchat.MatchNone] != 1 {
		t.Fatalf("match counts = %v, want one unmatched", export.matches)
	}

	s := sidecarOf(t, export.items[0])
	if s.CapturedAtSource != snapchat.TimeFromModTime {
		t.Errorf("capturedAtSource = %q, want %q", s.CapturedAtSource, snapchat.TimeFromModTime)
	}
	if s.CapturedAt == nil || !s.CapturedAt.Equal(at) {
		t.Errorf("capturedAt = %v, want %s", s.CapturedAt, at)
	}
	if len(s.History) != 0 {
		t.Error("an unmatched memory was given a history row")
	}
}

// The chat half: no history document is needed or read, every capture time is a
// modification time, and a publisher document is what proves an item is a news
// clip rather than a photograph.
func TestScanSnapchatChat(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()

	at := time.Date(2019, 11, 7, 8, 54, 58, 0, time.UTC)
	writeAt(t, filepath.Join(dir, chatDir, "2019-11-07_b~EiQSFVJhTmhDbmx.jpg"), tinyJPEG, at)
	writeAt(t, filepath.Join(dir, chatDir, "2019-11-07_media~zip-0c4b3f87.mp4"), tinyJPEG, at)
	writeAt(t, filepath.Join(dir, chatDir, "2019-11-07_thumbnail~zip-CEE7DCE1.jpg"), tinyJPEG, at)
	writeAt(t, filepath.Join(dir, chatDir, "2019-11-07_metadata~zip-0c4b3f87.unknown"),
		[]byte(`{"publisher_name":"DAILY MAIL","edition_id":"328808277501952"}`), at)

	export, err := scanSnapchatExport(context.Background(), exif, []string{dir}, halfChat)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	snap := itemNamed(export.items, "2019-11-07_b~EiQSFVJhTmhDbmx.jpg")
	if snap == nil {
		t.Fatal("the chat snap is missing from the scan")
	}
	s := sidecarOf(t, snap)
	if s.Kind != halfChat || s.Role != snapchat.RoleChatSnap {
		t.Errorf("sidecar kind/role = %q/%q", s.Kind, s.Role)
	}
	if s.CapturedAtSource != snapchat.TimeFromModTime {
		t.Errorf("capturedAtSource = %q, want %q", s.CapturedAtSource, snapchat.TimeFromModTime)
	}
	if len(s.History) != 0 {
		t.Error("chat media was given a history row; no document describes any of it")
	}

	// The publisher document names the media it describes by identifier, and
	// that is the only evidence in the export that an item is Discover content.
	clip := itemNamed(export.items, "2019-11-07_media~zip-0c4b3f87.mp4")
	if clip == nil {
		t.Fatal("the publisher clip is missing from the scan")
	}
	clipSidecar := sidecarOf(t, clip)
	if len(clipSidecar.Publisher) == 0 {
		t.Error("the publisher document was not attached to the media it names")
	}
	if !hasSubtype(clipSidecar.Subtypes, snapchat.SubtypeDiscover) {
		t.Errorf("subtypes = %v, want %s", clipSidecar.Subtypes, snapchat.SubtypeDiscover)
	}
	if export.publishers != 1 {
		t.Errorf("publishers = %d, want 1", export.publishers)
	}

	// A thumbnail whose original is not in the export is labelled rather than
	// dropped, and never mistaken for the thing it is a thumbnail of.
	thumb := itemNamed(export.items, "2019-11-07_thumbnail~zip-CEE7DCE1.jpg")
	if thumb == nil {
		t.Fatal("the thumbnail is missing from the scan")
	}
	if !hasSubtype(sidecarOf(t, thumb).Subtypes, snapchat.SubtypeThumbnail) {
		t.Error("a thumbnail was not labelled as one")
	}

	// The metadata document itself is not media and must not become an asset.
	if itemNamed(export.items, "2019-11-07_metadata~zip-0c4b3f87.unknown") != nil {
		t.Error("a publisher metadata document was imported as a photograph")
	}
}

// The same file delivered in two zips is one item, not two. Giving both the
// same local id would make the second look like a different photograph that
// happens to collide.
func TestScanSnapchatCountsDuplicateDeliveries(t *testing.T) {
	exif := exifReader(t)
	dir := t.TempDir()
	first, second := filepath.Join(dir, "snapchat_1"), filepath.Join(dir, "snapchat_2")

	at := time.Date(2022, 6, 6, 6, 6, 6, 0, time.UTC)
	writeHistory(t, first, row("2022-06-06 06:06:06 UTC", "Image", ""))
	writeAt(t, filepath.Join(first, memoriesDir, "2022-06-06_dup-main.jpg"), tinyJPEG, at)
	writeAt(t, filepath.Join(second, memoriesDir, "2022-06-06_dup-main.jpg"), tinyJPEG, at)

	export, err := scanSnapchatExport(context.Background(), exif, []string{first, second}, halfMemories)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(export.items) != 1 {
		t.Fatalf("got %d items, want 1", len(export.items))
	}
	if export.duplicatePaths != 1 {
		t.Errorf("duplicatePaths = %d, want 1", export.duplicatePaths)
	}
}

func hasSubtype(subtypes []string, want string) bool {
	for _, subtype := range subtypes {
		if subtype == want {
			return true
		}
	}
	return false
}
