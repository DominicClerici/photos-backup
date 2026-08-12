package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
)

// A miniature export, built out of the real fixtures so the identifiers being
// matched on are the ones Apple actually wrote.
//
// The layout is Google's: a dated folder for loose items and a named folder for
// an album, sidecars beside the media, and no sidecar at all for the video half
// of a Live Photo — Google exports a pair as two files and one JSON.
func buildExport(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	dated := filepath.Join(root, "Photos from 2025")
	album := filepath.Join(root, "Iceland 2025")
	for _, dir := range []string{dated, album} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	copyFixture(t, "iphone-portrait.heic", filepath.Join(dated, "IMG_5874.HEIC"))
	copyFixture(t, "live-clip.mov", filepath.Join(dated, "IMG_5874.MP4"))
	copyFixture(t, "clip.mov", filepath.Join(album, "IMG_5896.MOV"))
	copyFixture(t, "photo.jpg", filepath.Join(album, "IMG_6147.JPG"))

	// The current sidecar naming, and the truncated form in the same export —
	// which is exactly how a real one arrives, because the truncation depends
	// on how long each filename happens to be.
	write(t, filepath.Join(dated, "IMG_5874.HEIC.supplemental-metadata.json"), `{
		"title": "IMG_5874.HEIC",
		"description": "at the border",
		"photoTakenTime": { "timestamp": "1736125085" },
		"geoData": { "latitude": 41.7844, "longitude": -122.5848 },
		"favorited": true
	}`)
	write(t, filepath.Join(album, "IMG_6147.JPG.supplemental-met.json"), `{
		"title": "IMG_6147.JPG",
		"photoTakenTime": { "timestamp": "1736480878" }
	}`)
	write(t, filepath.Join(album, "IMG_5896.MOV.supplemental-metadata.json"), `{
		"title": "IMG_5896.MOV",
		"photoTakenTime": { "timestamp": "1736480000" },
		"trashed": true
	}`)

	write(t, filepath.Join(album, "metadata.json"), `{"title": "Iceland 2025", "description": "the ring road"}`)
	// The export's own bookkeeping, which describes nothing in the directory.
	write(t, filepath.Join(root, "print-subscriptions.json"), `[]`)
	return root
}

func copyFixture(t *testing.T, fixture, dest string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, root string, includeTrash bool) scanResult {
	t.Helper()
	return scanRoots(t, []string{root}, includeTrash)
}

func scanRoots(t *testing.T, roots []string, includeTrash bool) scanResult {
	t.Helper()
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool is not installed")
	}
	export, err := scanExport(context.Background(), exifdata.New(), roots, includeTrash)
	if err != nil {
		t.Fatalf("scanExport: %v", err)
	}
	return export
}

func find(export scanResult, name string) *importItem {
	for _, item := range export.items {
		if item.filename == name {
			return item
		}
	}
	return nil
}

func TestScanExportReadsAGooglePhotosLayout(t *testing.T) {
	export := scan(t, buildExport(t), false)

	if len(export.items) != 3 {
		var names []string
		for _, item := range export.items {
			names = append(names, item.localID)
		}
		t.Fatalf("items = %v, want the three media files and no JSON", names)
	}
	if export.pairs != 1 {
		t.Errorf("pairs = %d, want the HEIC and MP4 matched by their identifier", export.pairs)
	}
	if export.skippedTrash != 1 {
		t.Errorf("skippedTrash = %d, want the trashed clip held back", export.skippedTrash)
	}
	if len(export.unmatchedSidecars) != 0 {
		t.Errorf("unmatchedSidecars = %v, want every sidecar matched", export.unmatchedSidecars)
	}
	if len(export.albums) != 1 || export.albums[0] != "Iceland 2025" {
		t.Errorf("albums = %v, want only the named folder", export.albums)
	}
}

// The dated folder is a bucket, not an album, and the named one is an album
// whose title comes from its metadata.json rather than from its directory name.
func TestScanExportTellsAlbumsFromDatedFolders(t *testing.T) {
	export := scan(t, buildExport(t), false)

	still := find(export, "IMG_5874.HEIC")
	if still == nil {
		t.Fatal("the still is missing from the scan")
	}
	if len(still.albums) != 0 {
		t.Errorf("albums on a dated-folder item = %v, want none", still.albums)
	}

	inAlbum := find(export, "IMG_6147.JPG")
	if inAlbum == nil {
		t.Fatal("the album item is missing from the scan")
	}
	if len(inAlbum.albums) != 1 || inAlbum.albums[0].Title != "Iceland 2025" {
		t.Fatalf("albums = %v, want Iceland 2025", inAlbum.albums)
	}
	if inAlbum.albums[0].Description != "the ring road" {
		t.Errorf("album description = %q, want it from metadata.json", inAlbum.albums[0].Description)
	}
	// And the truncated sidecar name still found its file.
	if inAlbum.sidecar == nil {
		t.Error("a sidecar with a truncated suffix matched nothing")
	}
}

// Google writes one sidecar per item, and a Live Photo is one item. Without the
// inheritance the video half arrives with no capture time and no album, which
// is precisely what it needs when the pairing does not resolve and it has to
// stand on its own.
func TestScanExportGivesAPairedVideoTheStillsSidecar(t *testing.T) {
	export := scan(t, buildExport(t), false)

	video := find(export, "IMG_5874.MP4")
	if video == nil {
		t.Fatal("the paired video is missing from the scan")
	}
	if video.contentID == "" {
		t.Fatal("no content identifier was read off the paired video")
	}
	if video.sidecar == nil {
		t.Fatal("the paired video inherited no sidecar")
	}
	if video.takenAt == nil || video.takenAt.Unix() != 1736125085 {
		t.Errorf("takenAt = %v, want the still's capture time", video.takenAt)
	}

	still := find(export, "IMG_5874.HEIC")
	if still.contentID != video.contentID {
		t.Errorf("identifiers differ: still %q, video %q", still.contentID, video.contentID)
	}
}

// A Takeout strips the extension off every Live Photo's paired video, and 239
// of the 15,689 files in the real export are exactly that. exiftool recursing a
// directory reads only the extensions it knows, so without -ext the scan does
// not see them at all — and a file the scan does not see is one the import
// never uploads. That is the whole library's motion, silently absent.
func TestScanExportReadsAPairedVideoWithNoExtension(t *testing.T) {
	root := t.TempDir()
	dated := filepath.Join(root, "Photos from 2025")
	if err := os.MkdirAll(dated, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, "iphone-portrait.heic", filepath.Join(dated, "IMG_5874.HEIC"))
	copyFixture(t, "live-clip.mov", filepath.Join(dated, "IMG_5874"))

	export := scan(t, root, false)

	video := find(export, "IMG_5874")
	if video == nil {
		t.Fatal("the extensionless paired video was dropped by the scan")
	}
	if !video.isVideo {
		t.Error("the extensionless file was not classified as a video")
	}
	if export.pairs != 1 {
		t.Errorf("pairs = %d, want the still matched to its nameless video", export.pairs)
	}
}

// Every still is uploaded before any video, so that a paired video's row finds
// its still already archived and resolves in the transaction that inserts it.
func TestScanExportOrdersStillsBeforeVideos(t *testing.T) {
	export := scan(t, buildExport(t), true)

	seenVideo := false
	for _, item := range export.items {
		if item.isVideo {
			seenVideo = true
			continue
		}
		if seenVideo {
			t.Fatalf("%s is a photo and comes after a video", item.localID)
		}
	}
}

func TestScanExportKeepsTrashWhenAsked(t *testing.T) {
	export := scan(t, buildExport(t), true)

	if find(export, "IMG_5896.MOV") == nil {
		t.Error("--include-trash did not keep the trashed item")
	}
	if export.skippedTrash != 0 {
		t.Errorf("skippedTrash = %d, want none", export.skippedTrash)
	}
}

// Google splits a large export into numbered zips and does not keep an item
// with the JSON that describes it: in the real export, 5,979 of 11,282 sidecars
// describe a file that is in a different zip. Scanned together the directories
// are one export, and the pairing, the albums and the sidecars all resolve
// across them.
func TestScanExportReadsAnExportSplitAcrossDirectories(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()

	// The still and its album sit in one delivery.
	album := filepath.Join(first, "Iceland 2025")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, "iphone-portrait.heic", filepath.Join(album, "IMG_5874.HEIC"))
	write(t, filepath.Join(album, "metadata.json"), `{"title": "Iceland 2025"}`)

	// Its sidecar and its paired video are in the next one, at the same place
	// inside the export.
	otherHalf := filepath.Join(second, "Iceland 2025")
	if err := os.MkdirAll(otherHalf, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, "live-clip.mov", filepath.Join(otherHalf, "IMG_5874.MP4"))
	write(t, filepath.Join(otherHalf, "IMG_5874.HEIC.supplemental-metadata.json"), `{
		"title": "IMG_5874.HEIC",
		"description": "at the border",
		"photoTakenTime": { "timestamp": "1736125085" }
	}`)

	export := scanRoots(t, []string{first, second}, false)

	if len(export.unmatchedSidecars) != 0 {
		t.Errorf("unmatchedSidecars = %v, want the sidecar matched across directories",
			export.unmatchedSidecars)
	}
	still := find(export, "IMG_5874.HEIC")
	if still == nil {
		t.Fatal("the still is missing from the scan")
	}
	if still.sidecar == nil {
		t.Error("the still found no sidecar, so its caption and capture time are lost")
	}
	if len(still.albums) != 1 || still.albums[0].Title != "Iceland 2025" {
		t.Errorf("albums = %v, want the album named by the other directory's descriptor", still.albums)
	}
	if export.pairs != 1 {
		t.Errorf("pairs = %d, want the still paired with the video in the other directory", export.pairs)
	}
	// Local ids are relative to the export, not to the disk, so importing the
	// six directories and importing one merged tree agree on what each item is.
	if still.localID != "Iceland 2025/IMG_5874.HEIC" {
		t.Errorf("localID = %q, want it relative to the top of the export", still.localID)
	}
}

// The same file at the same place in two deliveries is one item zipped twice.
// Keeping both would give two uploads the same local id, and the second would
// answer "have" for bytes that are not its own.
func TestScanExportKeepsOneCopyOfADuplicatedPath(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, root := range []string{first, second} {
		dated := filepath.Join(root, "Photos from 2025")
		if err := os.MkdirAll(dated, 0o755); err != nil {
			t.Fatal(err)
		}
		copyFixture(t, "photo.jpg", filepath.Join(dated, "IMG_6147.JPG"))
	}

	export := scanRoots(t, []string{first, second}, false)

	if len(export.items) != 1 {
		t.Errorf("items = %d, want the duplicate delivery collapsed to one", len(export.items))
	}
	if export.duplicatePaths != 1 {
		t.Errorf("duplicatePaths = %d, want the duplicate reported rather than hidden", export.duplicatePaths)
	}
}

// A sidecar whose file is not in the export is reported rather than silently
// dropped: it is the signal that Google has changed the naming rules again.
func TestScanExportReportsASidecarThatMatchesNothing(t *testing.T) {
	root := buildExport(t)
	write(t, filepath.Join(root, "Photos from 2025", "IMG_9999.HEIC.supplemental-metadata.json"),
		`{"title": "IMG_9999.HEIC"}`)

	export := scan(t, root, false)
	if len(export.unmatchedSidecars) != 1 {
		t.Errorf("unmatchedSidecars = %v, want the orphan reported", export.unmatchedSidecars)
	}
}

// Reporting the filename was the wrong half to keep. The name says a matching
// rule needs fixing; the body is the only surviving copy of what the export
// knew about a photograph that is in one of the other five zips.
func TestScanExportKeepsAnUnmatchedSidecarsContents(t *testing.T) {
	root := buildExport(t)
	const body = `{"title": "IMG_9999.HEIC", "description": "the last morning", "people": [{"name": "Brody"}]}`
	write(t, filepath.Join(root, "Photos from 2025", "IMG_9999.HEIC.supplemental-metadata.json"), body)

	export := scan(t, root, false)
	if len(export.unmatchedSidecars) != 1 {
		t.Fatalf("unmatchedSidecars = %v, want one", export.unmatchedSidecars)
	}

	orphan := export.unmatchedSidecars[0]
	if orphan.locator != "Photos from 2025/IMG_9999.HEIC.supplemental-metadata.json" {
		t.Errorf("locator = %q, want the path inside the export", orphan.locator)
	}

	var parsed map[string]any
	if err := json.Unmarshal(orphan.raw, &parsed); err != nil {
		t.Fatalf("the kept sidecar is not JSON: %v", err)
	}
	if parsed["description"] != "the last morning" {
		t.Errorf("the sidecar was kept reduced, not verbatim: %v", parsed)
	}
	if len(parsed["people"].([]any)) != 1 {
		t.Errorf("people = %v, want the tag that exists nowhere else", parsed["people"])
	}
}
