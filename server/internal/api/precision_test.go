package api

import (
	"net/http"
	"testing"
	"time"
)

// The property PROJECT.md's second goal rests on: once an asset is archived, a
// later sync/check answers "have" from the device mapping alone — no digest,
// no hashing, no bytes.
//
// This broke on a timestamp precision mismatch. The upload header carried
// second-precision RFC3339, sync/check carried JSON's nanoseconds, and the
// mapping's exact-equality check could never hold. Every run re-hashed the
// whole library to rediscover what the server already had. At 110 items that is
// invisible; at 100GB it is the difference between a backup that finishes and
// one that grinds through every original on the phone, every time.
func TestArchivedAssetNeedsNoDigestOnTheNextRun(t *testing.T) {
	// Deliberately awkward: sub-microsecond precision Postgres cannot store and
	// no two formatters would spell the same way.
	modified := time.Date(2026, 8, 1, 15, 4, 5, 123456789, time.UTC)
	const localID = "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"

	// Every spelling of the same instant that carries at least the precision
	// the database can store. The phone writes the middle one — Date.toISOString
	// always emits exactly three decimals — which is why milliseconds is the
	// precision everything is normalized to.
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"millisecond precision header", modified.Format("2006-01-02T15:04:05.000Z07:00")},
		{"microsecond precision header", modified.Format("2006-01-02T15:04:05.000000Z07:00")},
		{"nanosecond precision header", modified.Format(time.RFC3339Nano)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			resp := h.upload(t, loadFixture(t), map[string]string{
				"X-Photo-Local-Id":    localID,
				"X-Photo-Modified-At": tc.header,
			})
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("upload status = %d, want 201", resp.StatusCode)
			}

			// Round one of the sync protocol: no digest, nothing hashed.
			got := h.checkOK(t, syncCheckRequest{
				DeviceID: "iphone-14-pro",
				Items:    []syncCheckItem{{LocalID: localID, ModifiedAt: &modified}},
			})
			if got[localID].Status != statusHave {
				t.Errorf("status = %q, want %q — the phone would re-hash this file",
					got[localID].Status, statusHave)
			}
		})
	}
}

// An asset genuinely edited on the phone keeps its local id but changes its
// bytes, so a stale mapping must not answer for it.
func TestEditedAssetIsStillRecheckedByContent(t *testing.T) {
	h := newHarness(t)
	const localID = "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"
	original := time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC)

	resp := h.upload(t, loadFixture(t), map[string]string{
		"X-Photo-Local-Id":    localID,
		"X-Photo-Modified-At": original.Format(time.RFC3339),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", resp.StatusCode)
	}

	edited := original.Add(time.Hour)
	got := h.checkOK(t, syncCheckRequest{
		DeviceID: "iphone-14-pro",
		Items:    []syncCheckItem{{LocalID: localID, ModifiedAt: &edited}},
	})
	if got[localID].Status != statusUnknown {
		t.Errorf("status = %q, want %q for an asset edited since it was archived",
			got[localID].Status, statusUnknown)
	}
}

// Normalization reconciles how an instant is spelled, not which instant it is.
// A client that reports a genuinely coarser time is describing a different
// moment, and the server treats it as one — the cost is a content check, which
// still resolves to "have" without moving any bytes, so a sloppy client is slow
// rather than wrong.
func TestCoarserTimestampIsADifferentInstant(t *testing.T) {
	h := newHarness(t)
	const localID = "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"
	precise := time.Date(2026, 8, 1, 15, 4, 5, 123000000, time.UTC)

	resp := h.upload(t, loadFixture(t), map[string]string{
		"X-Photo-Local-Id": localID,
		// Second precision drops the .123 the check will ask about.
		"X-Photo-Modified-At": precise.Format(time.RFC3339),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", resp.StatusCode)
	}

	got := h.checkOK(t, syncCheckRequest{
		DeviceID: "iphone-14-pro",
		Items:    []syncCheckItem{{LocalID: localID, ModifiedAt: &precise}},
	})
	if got[localID].Status != statusUnknown {
		t.Errorf("status = %q, want %q", got[localID].Status, statusUnknown)
	}
}

// A difference too small for the database to represent is not an edit. Without
// this, the truncation Postgres performs on the way in would make every
// nanosecond-precision client look like it had edited its entire library.
func TestSubMillisecondDriftIsNotAnEdit(t *testing.T) {
	h := newHarness(t)
	const localID = "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"
	stored := time.Date(2026, 8, 1, 15, 4, 5, 500_000_000, time.UTC)

	resp := h.upload(t, loadFixture(t), map[string]string{
		"X-Photo-Local-Id":    localID,
		"X-Photo-Modified-At": stored.Format(time.RFC3339Nano),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", resp.StatusCode)
	}

	jittered := stored.Add(999 * time.Nanosecond)
	got := h.checkOK(t, syncCheckRequest{
		DeviceID: "iphone-14-pro",
		Items:    []syncCheckItem{{LocalID: localID, ModifiedAt: &jittered}},
	})
	if got[localID].Status != statusHave {
		t.Errorf("status = %q, want %q", got[localID].Status, statusHave)
	}
}
