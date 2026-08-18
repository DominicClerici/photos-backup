package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A folder holding two items that Google Photos held under the same title.
//
// This is the shape that matters and it is not guessable from the sidecars'
// contents: both of them say `"title": "20170817_014420.jpg"`, because that is
// what Photos held, and only the `(1)` the export appended says which document
// belongs to which file. The urls are what tell them apart here, for the same
// reason they tell them apart in a real export — they are per-item.
func buildCollisionExport(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	copyFixture(t, "photo.jpg", filepath.Join(dir, "20170817_014420.jpg"))
	copyFixture(t, "photo.jpg", filepath.Join(dir, "20170817_014420(1).jpg"))

	write(t, filepath.Join(dir, "20170817_014420.jpg.supplemental-metadata.json"), `{
		"title": "20170817_014420.jpg",
		"photoTakenTime": { "timestamp": "1502959460" },
		"url": "https://photos.google.com/photo/BASE"
	}`)
	write(t, filepath.Join(dir, "20170817_014420.jpg.supplemental-metadata(1).json"), `{
		"title": "20170817_014420.jpg",
		"photoTakenTime": { "timestamp": "1502959999" },
		"url": "https://photos.google.com/photo/COUNTER"
	}`)
	return root
}

func urlOfSidecar(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	return s.URL
}

// Both siblings must keep their own document. Believing the title instead of the
// counter used to give them both to the base file, which silently dropped one
// document and left `(1)` with no capture time, no coordinates and no album.
func TestCollisionSiblingsEachKeepTheirOwnSidecar(t *testing.T) {
	root := buildCollisionExport(t)

	_, sidecars, _, _, err := walkExport([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	got := sidecars["Archive"]

	base, ok := got["20170817_014420.jpg"]
	if !ok {
		t.Fatal("the base file got no sidecar")
	}
	counter, ok := got["20170817_014420(1).jpg"]
	if !ok {
		t.Fatal("the (1) file got no sidecar — the counter sidecar was discarded")
	}

	if u := urlOfSidecar(t, base); u != "https://photos.google.com/photo/BASE" {
		t.Errorf("base file holds %s, want the base document", u)
	}
	if u := urlOfSidecar(t, counter); u != "https://photos.google.com/photo/COUNTER" {
		t.Errorf("(1) file holds %s, want the counter document", u)
	}
}

// The matching runs over a map, so a bug here would show up as a flake rather
// than a failure. Reading the same export repeatedly is what catches that.
func TestCollisionMatchingIsDeterministic(t *testing.T) {
	root := buildCollisionExport(t)

	for i := 0; i < 25; i++ {
		_, sidecars, _, _, err := walkExport([]string{root})
		if err != nil {
			t.Fatal(err)
		}
		got := sidecars["Archive"]
		if len(got) != 2 {
			t.Fatalf("run %d: %d sidecars placed, want 2", i, len(got))
		}
		if u := urlOfSidecar(t, got["20170817_014420.jpg"]); u != "https://photos.google.com/photo/BASE" {
			t.Fatalf("run %d: base file holds %s", i, u)
		}
	}
}

// No media file may be described twice. A second claim is always a mistake, and
// before the fix it was a silent one — the document already in place was
// overwritten rather than reported.
func TestSidecarWillNotClaimAFileAnotherSidecarHas(t *testing.T) {
	root := t.TempDir()
	copyFixture(t, "photo.jpg", filepath.Join(root, "IMG_1.JPG"))
	write(t, filepath.Join(root, "IMG_1.JPG.supplemental-metadata.json"), `{
		"title": "IMG_1.JPG", "url": "https://photos.google.com/photo/FIRST"
	}`)
	// A second document naming the same file, with no media file of its own.
	write(t, filepath.Join(root, "IMG_1.JPG.supplemental-metadata(1).json"), `{
		"title": "IMG_1.JPG", "url": "https://photos.google.com/photo/SECOND"
	}`)

	_, sidecars, _, _, err := walkExport([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	got := sidecars["."]

	if u := urlOfSidecar(t, got["IMG_1.JPG"]); u != "https://photos.google.com/photo/FIRST" {
		t.Errorf("IMG_1.JPG holds %s, want the document that names it", u)
	}
	// The loser is kept under its own name, which is how scanExport reports it
	// as an orphan rather than losing the photograph it describes.
	orphan, ok := got["IMG_1.JPG.supplemental-metadata(1).json"]
	if !ok {
		t.Fatal("the unplaced sidecar was dropped instead of kept for review")
	}
	if u := urlOfSidecar(t, orphan); u != "https://photos.google.com/photo/SECOND" {
		t.Errorf("orphan holds %s", u)
	}
}
