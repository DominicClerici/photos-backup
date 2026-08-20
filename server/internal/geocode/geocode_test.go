package geocode_test

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/geocode"
)

// The fixture is seven real rows lifted out of cities500: Chicago and three
// places that border it, one Chicago neighbourhood, and two cities on the other
// side of the world. Real rows rather than invented ones because the parsing is
// half of what is under test, and a hand-written line would agree with whatever
// this package expects rather than with what GeoNames actually ships.
func testIndex(t *testing.T) *geocode.Index {
	t.Helper()
	ix, err := geocode.Load("testdata")
	if err != nil {
		t.Fatalf("load the extract: %v", err)
	}
	return ix
}

func TestNearestNamesTheCityAPhotographWasTakenIn(t *testing.T) {
	ix := testIndex(t)

	for _, tc := range []struct {
		name         string
		lat, lon     float64
		city, admin1 string
		country      string
	}{
		{"downtown Chicago", 41.8500, -87.6500, "Chicago", "Illinois", "United States"},
		{"Waikiki", 21.2760, -157.8270, "Honolulu", "Hawaii", "United States"},
		{"Zurich old town", 47.3700, 8.5450, "Zürich", "Zurich", "Switzerland"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ix.Nearest(tc.lat, tc.lon)
			if !ok {
				t.Fatalf("no place for %.4f,%.4f", tc.lat, tc.lon)
			}
			if got.City != tc.city || got.Admin1 != tc.admin1 || got.Country != tc.country {
				t.Errorf("got %q/%q/%q, want %q/%q/%q",
					got.City, got.Admin1, got.Country, tc.city, tc.admin1, tc.country)
			}
		})
	}
}

// A neighbourhood is closer to a photograph than the city it is inside, every
// time, which is why they are dropped on the way in. Without that the whole
// feature inverts: nothing downtown would ever say Chicago.
func TestANeighbourhoodIsNotAPlace(t *testing.T) {
	ix := testIndex(t)

	// The coordinate GeoNames gives for Lincoln Park, which is in the fixture
	// as a PPLX row and must not be the answer to anything.
	got, ok := ix.Nearest(41.9217, -87.64783)
	if !ok {
		t.Fatal("no place for Lincoln Park")
	}
	if got.City != "Chicago" {
		t.Errorf("got %q, want Chicago", got.City)
	}
}

// A photograph taken inside a city is filed under the city, even when the
// nearest recorded centre belongs to something smaller. Halfway between
// Cicero's centre and Chicago's, Cicero is exactly as close and has thirty
// times less of everything else.
func TestAPhotographInsideACityIsFiledUnderTheCity(t *testing.T) {
	ix := testIndex(t)

	got, ok := ix.Nearest(41.84781, -87.70200)
	if !ok {
		t.Fatal("no place between Cicero and Chicago")
	}
	if got.City != "Chicago" {
		t.Errorf("got %q, want Chicago", got.City)
	}
}

// The visible cost of that rule, pinned here so that changing it is a decision
// rather than a surprise. Oak Park is a village with its own name, its own
// government and a shared border with Chicago, and it sits inside Chicago's
// radius — so its photographs read as Chicago.
func TestAVillageOnTheCityLineReadsAsTheCity(t *testing.T) {
	ix := testIndex(t)

	got, ok := ix.Nearest(41.88503, -87.7845)
	if !ok {
		t.Fatal("no place for Oak Park")
	}
	if got.City != "Chicago" {
		t.Errorf("got %q, want Chicago — see the note on coverDensity", got.City)
	}
}

// And where the rule stops. Evanston is 18km up the lakefront, outside
// Chicago's radius, and a photograph taken there was taken in Evanston.
func TestASuburbKeepsItsOwnName(t *testing.T) {
	ix := testIndex(t)

	got, ok := ix.Nearest(42.04114, -87.69006)
	if !ok {
		t.Fatal("no place for Evanston")
	}
	if got.City != "Evanston" {
		t.Errorf("got %q, want Evanston", got.City)
	}
}

// An empty field is honest; "Honolulu" for a photograph 3,000km from it is not.
func TestOpenWaterHasNoPlace(t *testing.T) {
	ix := testIndex(t)

	if got, ok := ix.Nearest(0, -140); ok {
		t.Errorf("the middle of the Pacific reported %q, %.0fkm away", got.City, got.DistanceKM)
	}
}

func TestCoordinatesOffTheGlobeAreRefused(t *testing.T) {
	ix := testIndex(t)

	for _, tc := range []struct{ lat, lon float64 }{{91, 0}, {-91, 0}, {0, 181}, {0, -181}} {
		if _, ok := ix.Nearest(tc.lat, tc.lon); ok {
			t.Errorf("%.0f,%.0f is not a coordinate and got a place anyway", tc.lat, tc.lon)
		}
	}
}

// Installing the extract should be three downloads and no unzip, so the zip as
// published is read directly.
func TestTheZipIsReadWithoutUnpacking(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"admin1CodesASCII.txt", "countryInfo.txt"} {
		copyFile(t, filepath.Join("testdata", name), filepath.Join(dir, name))
	}

	archive, err := os.Create(filepath.Join(dir, "cities500.zip"))
	if err != nil {
		t.Fatalf("create the zip: %v", err)
	}
	zw := zip.NewWriter(archive)
	entry, err := zw.Create("cities500.txt")
	if err != nil {
		t.Fatalf("add to the zip: %v", err)
	}
	body, err := os.ReadFile(filepath.Join("testdata", "cities500.txt"))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	if _, err := entry.Write(body); err != nil {
		t.Fatalf("write into the zip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close the zip: %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}

	ix, err := geocode.Load(dir)
	if err != nil {
		t.Fatalf("load from the zip: %v", err)
	}
	got, ok := ix.Nearest(41.8500, -87.6500)
	if !ok || got.City != "Chicago" {
		t.Errorf("got %q/%v, want Chicago", got.City, ok)
	}
}

// Not installed is the ordinary state of a fresh checkout, and it has to be
// distinguishable from a corrupt file: one of them is a sentence telling
// somebody what to download, and the other is a fault.
func TestAMissingExtractSaysSoDistinctly(t *testing.T) {
	if _, err := geocode.Load(t.TempDir()); !errors.Is(err, geocode.ErrNoData) {
		t.Errorf("got %v, want ErrNoData", err)
	}

	dir := t.TempDir()
	copyFile(t, filepath.Join("testdata", "admin1CodesASCII.txt"), filepath.Join(dir, "admin1CodesASCII.txt"))
	copyFile(t, filepath.Join("testdata", "countryInfo.txt"), filepath.Join(dir, "countryInfo.txt"))
	if _, err := geocode.Load(dir); !errors.Is(err, geocode.ErrNoData) {
		t.Errorf("a directory with no cities file got %v, want ErrNoData", err)
	}
}

// The loader is what the worker holds, and it must not go back to the disk once
// per photograph — nor retry a missing file eleven thousand times.
func TestTheLoaderRemembersBothAnswers(t *testing.T) {
	l := geocode.NewLoader("testdata")
	first, err := l.Index()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	second, err := l.Index()
	if err != nil {
		t.Fatalf("load again: %v", err)
	}
	if first != second {
		t.Error("the loader parsed the extract twice")
	}

	missing := geocode.NewLoader(t.TempDir())
	if _, err := missing.Index(); !errors.Is(err, geocode.ErrNoData) {
		t.Errorf("got %v, want ErrNoData", err)
	}
	if _, err := missing.Index(); !errors.Is(err, geocode.ErrNoData) {
		t.Errorf("the second call got %v, want ErrNoData", err)
	}

	if _, err := geocode.NewLoader("").Index(); !errors.Is(err, geocode.ErrNoData) {
		t.Error("an unconfigured loader should report ErrNoData rather than reading the working directory")
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}
