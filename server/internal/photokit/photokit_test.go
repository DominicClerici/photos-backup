package photokit

import (
	"bytes"
	"testing"
)

func TestNormalizeReadsWhatTheLibraryKnows(t *testing.T) {
	got, err := Normalize([]byte(`{
		"favorite": true,
		"subtypes": ["livePhoto", " hdr ", ""],
		"location": { "latitude": 41.7844, "longitude": -122.5848 }
	}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if !got.Favorite {
		t.Error("Favorite = false")
	}
	if len(got.Subtypes) != 2 || got.Subtypes[1] != "hdr" {
		t.Errorf("Subtypes = %v, want the two real ones, trimmed", got.Subtypes)
	}
	if got.GPSLat == nil || *got.GPSLat != 41.7844 {
		t.Errorf("GPSLat = %v", got.GPSLat)
	}
	if len(got.Raw) == 0 {
		t.Error("Raw is empty; the sidecar was not kept")
	}
}

// A serializer writing zeroes for "no location" is far likelier than a photo
// taken at the one point in the Gulf of Guinea where both axes are zero — the
// same call takeout makes about Google's zeroed geoData.
func TestNullIslandIsReadAsNoLocation(t *testing.T) {
	got, err := Normalize([]byte(`{"location": {"latitude": 0, "longitude": 0}}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.GPSLat != nil || got.GPSLon != nil {
		t.Errorf("coordinates = %v/%v, want none", got.GPSLat, got.GPSLon)
	}
}

// The Hidden album, and everything the phone reads straight off PHAsset. Only
// the hiding is modelled; the rest has to survive in Raw, which is the whole
// bargain of storing the sidecar verbatim.
func TestNormalizeReadsWhatPHAssetKnows(t *testing.T) {
	raw := []byte(`{
		"favorite": false,
		"subtypes": ["screenshot"],
		"hidden": true,
		"location": {
			"latitude": 41.7844, "longitude": -122.5848,
			"altitude": 812.4, "horizontalAccuracy": 4.7, "verticalAccuracy": -1,
			"course": -1, "speed": -1, "timestamp": "2025-01-06T01:38:05.000Z"
		},
		"photoKit": {
			"burstIdentifier": "3F0B4B0A-0000-0000-0000-000000000000",
			"hasAdjustments": true,
			"sourceType": { "value": 2, "names": ["typeCloudShared"] },
			"originalFilename": "IMG_5874.HEIC"
		}
	}`)

	got, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if !got.Hidden {
		t.Error("Hidden = false on an asset the phone had in the Hidden album")
	}
	if got.GPSLat == nil || *got.GPSLat != 41.7844 {
		t.Errorf("GPSLat = %v; the richer location still has to reduce to a column", got.GPSLat)
	}
	if !bytes.Contains(got.Raw, []byte("burstIdentifier")) {
		t.Error("the burst identifier did not survive into Raw, where the unmodelled facts live")
	}
	if !bytes.Contains(got.Raw, []byte("hasAdjustments")) {
		t.Error("whether the shot was edited did not survive into Raw")
	}
}

// A phone that could not ask says nothing rather than saying no, so the absence
// has to read as "not hidden" here and stay an absence in the sidecar.
func TestASidecarWithoutHiddenIsNotHidden(t *testing.T) {
	got, err := Normalize([]byte(`{"favorite": true}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Hidden {
		t.Error("Hidden = true on a sidecar that never mentioned it")
	}
}

func TestAnAssetWithNothingToSayIsStillValid(t *testing.T) {
	got, err := Normalize([]byte(`{}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Favorite || len(got.Subtypes) != 0 || got.GPSLat != nil {
		t.Errorf("got %+v, want everything empty", got)
	}
}

func TestMalformedSidecarIsRefused(t *testing.T) {
	if _, err := Normalize([]byte(`{"favorite":`)); err == nil {
		t.Error("a truncated sidecar was accepted")
	}
}
