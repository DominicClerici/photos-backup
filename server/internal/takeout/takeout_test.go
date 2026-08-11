package takeout

import (
	"testing"
	"time"
)

// The names Google actually produces. Every one of these is a shape seen in a
// real export, and the reason this table exists rather than a rule: the
// transformations compose, so the only way to know the rule is right is to run
// it against the compositions.
func TestMediaNameForHandlesTheNamesGoogleWrites(t *testing.T) {
	cases := []struct {
		name    string
		sidecar string
		media   string
	}{
		{"current form", "IMG_5874.HEIC.supplemental-metadata.json", "IMG_5874.HEIC"},
		{"suffix truncated", "IMG_5874.HEIC.supplemental-met.json", "IMG_5874.HEIC"},
		{"suffix truncated hard", "IMG_5874.HEIC.s.json", "IMG_5874.HEIC"},
		{"older export", "IMG_5874.HEIC.json", "IMG_5874.HEIC"},
		{"collision counter migrates inward", "IMG_5874.HEIC.supplemental-metadata(1).json", "IMG_5874(1).HEIC"},
		{"collision counter with truncated suffix", "IMG_5874.HEIC.supple(2).json", "IMG_5874(2).HEIC"},
		{"no extension on the media", "IMG_5874.supplemental-metadata.json", "IMG_5874"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MediaNameFor(tc.sidecar)
			for _, candidate := range got {
				if candidate == tc.media {
					return
				}
			}
			t.Errorf("MediaNameFor(%q) = %v, none of which is %q", tc.sidecar, got, tc.media)
		})
	}
}

// The bare `<media>.json` form is ambiguous with the truncated-suffix form: for
// `IMG_1.HEIC.json` both "IMG_1.HEIC" and "IMG_1" are plausible readings. Both
// are offered so the caller can pick whichever is on disk, and the more likely
// one comes first.
func TestMediaNameForOffersTheUntrimmedNameToo(t *testing.T) {
	got := MediaNameFor("IMG_5874.HEIC.supplemental-metadata.json")
	if len(got) < 2 || got[0] != "IMG_5874.HEIC" {
		t.Fatalf("candidates = %v, want IMG_5874.HEIC first", got)
	}
}

func TestMediaNameForIgnoresWhatIsNotASidecar(t *testing.T) {
	if got := MediaNameFor("IMG_5874.HEIC"); got != nil {
		t.Errorf("MediaNameFor on a media file = %v, want nil", got)
	}
}

// A real sidecar from the sample export, whole.
const sampleSidecar = `{
  "title": "IMG_5874.HEIC",
  "description": "at the border",
  "imageViews": "6",
  "creationTime": { "timestamp": "1736409424", "formatted": "Jan 9, 2025, 7:57:04 AM UTC" },
  "photoTakenTime": { "timestamp": "1736125085", "formatted": "Jan 6, 2025, 12:58:05 AM UTC" },
  "geoData": { "latitude": 41.7844, "longitude": -122.5848, "altitude": 853.04 },
  "geoDataExif": { "latitude": 41.7844, "longitude": -122.5848, "altitude": 853.04 },
  "favorited": true,
  "people": [{ "name": "Brody" }, { "name": "" }],
  "url": "https://photos.google.com/photo/AF1QipOfz7MJcp",
  "googlePhotosOrigin": { "mobileUpload": { "deviceType": "IOS_PHONE" } }
}`

func TestNormalizeReadsASidecar(t *testing.T) {
	m, err := Normalize([]byte(sampleSidecar))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	// photoTakenTime, not creationTime: the photo was taken on the 6th and
	// reached Google on the 9th, and sorting a library by the latter would file
	// it three days late.
	want := time.Unix(1736125085, 0).UTC()
	if m.TakenAt == nil || !m.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", m.TakenAt, want)
	}
	if m.Description != "at the border" {
		t.Errorf("Description = %q", m.Description)
	}
	if !m.Favorite {
		t.Error("Favorite = false, want true")
	}
	if m.GPSLat == nil || *m.GPSLat != 41.7844 {
		t.Errorf("GPSLat = %v, want 41.7844", m.GPSLat)
	}
	if len(m.People) != 1 || m.People[0] != "Brody" {
		t.Errorf("People = %v, want [Brody] with the blank dropped", m.People)
	}
	if string(m.Raw) != sampleSidecar {
		t.Error("Raw is not the sidecar verbatim")
	}
}

// Google writes zeroes rather than omitting geoData, and a literal reading puts
// every screenshot in an archive on one point in the Gulf of Guinea.
func TestNormalizeReadsNullIslandAsNoLocation(t *testing.T) {
	m, err := Normalize([]byte(`{
		"title": "IMG_5887.JPG",
		"photoTakenTime": { "timestamp": "1736480878" },
		"geoData": { "latitude": 0.0, "longitude": 0.0, "altitude": 0.0 }
	}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if m.GPSLat != nil || m.GPSLon != nil {
		t.Errorf("coordinates = (%v, %v), want none", m.GPSLat, m.GPSLon)
	}
}

// A location the file recorded and Google did not is still a location.
func TestNormalizeFallsBackToTheExifCopy(t *testing.T) {
	m, err := Normalize([]byte(`{
		"geoData": { "latitude": 0.0, "longitude": 0.0 },
		"geoDataExif": { "latitude": 41.5363, "longitude": -109.4786 }
	}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if m.GPSLat == nil || *m.GPSLat != 41.5363 {
		t.Errorf("GPSLat = %v, want the EXIF copy", m.GPSLat)
	}
}

func TestNormalizeFallsBackToCreationTime(t *testing.T) {
	m, err := Normalize([]byte(`{"creationTime": {"timestamp": "1736409424"}}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if m.TakenAt == nil || !m.TakenAt.Equal(time.Unix(1736409424, 0).UTC()) {
		t.Errorf("TakenAt = %v, want the creation time", m.TakenAt)
	}
}

func TestNormalizeTreatsBothSpellingsOfDeletedAsDeleted(t *testing.T) {
	for _, field := range []string{"trashed", "inTrash"} {
		m, err := Normalize([]byte(`{"` + field + `": true}`))
		if err != nil {
			t.Fatalf("Normalize %s: %v", field, err)
		}
		if !m.Trashed {
			t.Errorf("%s: Trashed = false", field)
		}
	}
}

func TestIsDatedFolder(t *testing.T) {
	cases := map[string]bool{
		"Photos from 2025": true,
		"photos from 1999": true,
		"Fotos von 2025":   true,
		"Fotos from 2025":  true,
		"Iceland 2025":     false,
		"Photos from Mars": false,
		"2025":             false,
		"Brody":            false,
	}
	for name, want := range cases {
		if got := IsDatedFolder(name); got != want {
			t.Errorf("IsDatedFolder(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDirectoryJSONIsNotAnItemSidecar(t *testing.T) {
	for _, name := range []string{"metadata.json", "print-subscriptions.json", "shared_album_comments.json"} {
		if !IsDirectoryJSON(name) {
			t.Errorf("IsDirectoryJSON(%q) = false", name)
		}
	}
	if IsDirectoryJSON("IMG_5874.HEIC.supplemental-metadata.json") {
		t.Error("an item sidecar was taken for a directory file")
	}
	if !IsAlbumDescriptor("metadata.json") || IsAlbumDescriptor("print-subscriptions.json") {
		t.Error("album descriptors are not being told apart from the export's other bookkeeping")
	}
}
