package db

import (
	"context"
	"testing"
	"time"
)

func chicago() Place {
	return Place{City: "Chicago", Admin1: "Illinois", Country: "United States", Source: "geonames"}
}

func withCoordinates(t *testing.T, s *Store, id string, lat, lon float64) {
	t.Helper()
	if err := s.ApplyMetadata(context.Background(), id, Metadata{GPSLat: &lat, GPSLon: &lon}); err != nil {
		t.Fatalf("apply coordinates: %v", err)
	}
}

func TestOnlyAssetsWithCoordinatesAndNoPlaceAreWaiting(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	located := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	bare := seedAsset(t, s, 2, time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC))
	done := seedAsset(t, s, 3, time.Date(2025, 6, 3, 12, 0, 0, 0, time.UTC))
	withCoordinates(t, s, located, 41.8500, -87.6500)
	withCoordinates(t, s, done, 41.8500, -87.6500)
	if err := s.ApplyPlace(ctx, done, chicago()); err != nil {
		t.Fatalf("apply place: %v", err)
	}

	pending, err := s.PendingGeocode(ctx, false)
	if err != nil {
		t.Fatalf("PendingGeocode: %v", err)
	}
	if len(pending) != 1 || pending[0].AssetID != located {
		t.Fatalf("waiting: %v, want just the one asset with coordinates and no place", pending)
	}
	if pending[0].Lat != 41.8500 || pending[0].Lon != -87.6500 {
		t.Errorf("coordinates %.4f,%.4f are not the ones stored", pending[0].Lat, pending[0].Lon)
	}
	if pending[0].AssetID == bare {
		t.Error("an asset with no GPS fix is waiting on a geocoder that has nothing to work from")
	}

	// --all is what a better extract needs: everything with coordinates, said
	// or not.
	every, err := s.PendingGeocode(ctx, true)
	if err != nil {
		t.Fatalf("PendingGeocode(all): %v", err)
	}
	if len(every) != 2 {
		t.Errorf("--all offered %d assets, want the 2 with coordinates", len(every))
	}
}

func TestAPlaceIsStoredAndReadBackOffTheRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	withCoordinates(t, s, id, 41.8500, -87.6500)
	if err := s.ApplyPlace(ctx, id, chicago()); err != nil {
		t.Fatalf("apply place: %v", err)
	}

	asset, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if asset.Place != chicago() {
		t.Errorf("place = %+v, want %+v", asset.Place, chicago())
	}
	if asset.GeocodedAt == nil {
		t.Error("geocoded_at is null on an asset that was just geocoded")
	}
}

// A photograph taken over open water has coordinates and no place, and that is
// an answer. Recording it as one is what stops the backfill offering the same
// 67 assets on every run forever.
func TestNothingWithinReachIsStillRecordedAsLookedAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	withCoordinates(t, s, id, 0, -140)
	if err := s.ApplyPlace(ctx, id, Place{}); err != nil {
		t.Fatalf("apply an empty place: %v", err)
	}

	asset, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if !asset.Place.Empty() {
		t.Errorf("place = %+v, want nothing", asset.Place)
	}
	if asset.GeocodedAt == nil {
		t.Fatal("geocoded_at is null, so this asset is pending again")
	}

	pending, err := s.PendingGeocode(ctx, false)
	if err != nil {
		t.Fatalf("PendingGeocode: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d assets still waiting, want none", len(pending))
	}
}

// The metadata job asks the row rather than reasoning about what it just wrote,
// because the coordinates it should resolve can be the ones ApplyMetadata
// coalesced in from an import sidecar a moment earlier — a screenshot whose own
// EXIF has none, for instance.
func TestTheCoordinatesToResolveCanComeFromTheSidecar(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, id, heicSidecar)
	if err := s.ApplyMetadata(ctx, id, Metadata{}); err != nil {
		t.Fatalf("apply metadata with no coordinates of its own: %v", err)
	}

	target, ok, err := s.AssetToGeocode(ctx, id)
	if err != nil {
		t.Fatalf("AssetToGeocode: %v", err)
	}
	if !ok {
		t.Fatal("no coordinates to resolve, but the sidecar carried some")
	}
	if target.Lat != 41.7844 || target.Lon != -122.5848 {
		t.Errorf("got %.4f,%.4f, want the sidecar's 41.7844,-122.5848", target.Lat, target.Lon)
	}

	bare := seedAsset(t, s, 2, time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC))
	if _, ok, err := s.AssetToGeocode(ctx, bare); err != nil || ok {
		t.Errorf("an asset with no fix offered coordinates (ok=%v, err=%v)", ok, err)
	}
}

// A place name is a more legible description of where somebody was than the
// coordinates it came from, so the vault has to take it too — and give it back.
func TestHidingEmptiesThePlaceAndRestoringPutsItBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	withCoordinates(t, s, id, 41.8500, -87.6500)
	if err := s.ApplyPlace(ctx, id, chicago()); err != nil {
		t.Fatalf("apply place: %v", err)
	}

	hide(t, s, VaultArchive, Selection{IDs: []string{id}})

	hidden, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load hidden asset: %v", err)
	}
	if !hidden.Place.Empty() {
		t.Errorf("the row still says the photograph was taken in %q", hidden.Place.City)
	}
	if hidden.GeocodedAt != nil {
		t.Error("the row still says when it was geocoded")
	}

	// And nothing puts one back while it is hidden.
	n, err := s.ApplyPlaces(ctx, []AssetPlace{{AssetID: id, Place: chicago()}})
	if err != nil {
		t.Fatalf("ApplyPlaces on a hidden asset: %v", err)
	}
	if n != 0 {
		t.Errorf("%d hidden rows were given a place name", n)
	}
	pending, err := s.PendingGeocode(ctx, true)
	if err != nil {
		t.Fatalf("PendingGeocode: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("the backfill offered %d hidden assets", len(pending))
	}

	unhide(t, s, []string{id})

	back, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load restored asset: %v", err)
	}
	if back.Place != chicago() {
		t.Errorf("place = %+v after a restore, want %+v", back.Place, chicago())
	}
	if back.GeocodedAt == nil {
		t.Error("the restore did not put geocoded_at back")
	}
}
