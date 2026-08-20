package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/geocode"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// The fixture was taken at 40.7216, -73.9783, so the extract this writes holds
// the one place that matters plus a decoy far enough away to prove the tree is
// being asked rather than the first row being returned.
func installExtract(t *testing.T, h *harness) {
	t.Helper()
	dir := filepath.Join(h.root, "geonames")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the extract directory: %v", err)
	}

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("cities500.txt", strings.Join([]string{
		geoLine("5128581", "New York City", "PPLA", "40.71427", "-74.00597", "US", "NY", "8804190"),
		geoLine("4887398", "Chicago", "PPLA2", "41.85003", "-87.65005", "US", "IL", "2664452"),
	}, "\n")+"\n")
	write("admin1CodesASCII.txt", "US.NY\tNew York\tNew York\t5128638\nUS.IL\tIllinois\tIllinois\t4896861\n")
	write("countryInfo.txt", "#ISO\nUS\tUSA\t840\tUS\tUnited States\tWashington\n")

	h.Places = geocode.NewLoader(dir)
}

// geoLine renders one row in the nineteen-column layout GeoNames publishes.
func geoLine(id, name, code, lat, lon, country, admin1, pop string) string {
	f := make([]string, 19)
	f[0], f[1], f[2] = id, name, name
	f[4], f[5] = lat, lon
	f[6], f[7] = "P", code
	f[8], f[10] = country, admin1
	f[14] = pop
	return strings.Join(f, "\t")
}

func TestMetadataJobNamesThePlaceAPhotographWasTaken(t *testing.T) {
	h := newHarness(t)
	installExtract(t, h)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, asset.ID)
	if got.Place.City != "New York City" {
		t.Errorf("City = %q, want New York City", got.Place.City)
	}
	if got.Place.Admin1 != "New York" || got.Place.Country != "United States" {
		t.Errorf("Admin1/Country = %q/%q, want New York/United States", got.Place.Admin1, got.Place.Country)
	}
	if got.Place.Source != geocode.Source {
		t.Errorf("Source = %q, want %q", got.Place.Source, geocode.Source)
	}
	if got.GeocodedAt == nil {
		t.Error("geocoded_at is null after a metadata job that resolved a place")
	}
}

// A photograph with no fix is not a failure and not a pending geocode. It is a
// photograph with no fix, and the row should say nothing at all.
func TestAPhotographWithNoFixGetsNoPlace(t *testing.T) {
	h := newHarness(t)
	installExtract(t, h)
	asset := h.ingest(t, "bare.jpg", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, asset.ID)
	if !got.Place.Empty() {
		t.Errorf("place = %+v on a photograph with no coordinates", got.Place)
	}
	if got.GeocodedAt != nil {
		t.Error("geocoded_at is set on a photograph there was nothing to geocode")
	}
}

// PROJECT.md §4's rule, applied one rung down: an optional reference table must
// not be able to stop a thumbnail being built.
func TestAMissingExtractCostsThePlaceNameAndNothingElse(t *testing.T) {
	h := newHarness(t)
	h.Places = geocode.NewLoader(filepath.Join(h.root, "not-installed"))
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, asset.ID)
	if got.DerivedState != db.DerivedReady {
		t.Fatalf("DerivedState = %q, want ready — the metadata job failed over a missing text file", got.DerivedState)
	}
	if got.GPSLat == nil {
		t.Error("the coordinates were not recorded")
	}
	if !got.Place.Empty() {
		t.Errorf("place = %+v with no extract installed", got.Place)
	}
}

// The runner is also allowed to have no geocoder at all, which is what every
// other test in this package runs as.
func TestNoGeocoderConfiguredIsFine(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)

	if got := h.reload(t, asset.ID); got.DerivedState != db.DerivedReady {
		t.Errorf("DerivedState = %q, want ready", got.DerivedState)
	}
}
