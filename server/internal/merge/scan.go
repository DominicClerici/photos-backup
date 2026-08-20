package merge

import "time"

// ScanResult is what one sweep of the library found.
//
// It lives here rather than beside the sweep itself because two packages that
// must not know about each other both need it: the worker runs the scan, and
// the API hands the result back to the page that asked for one.
type ScanResult struct {
	// Segments and Duplicates are groups newly proposed — not groups found. A
	// scan run twice over an unchanged library finds everything it found last
	// time and proposes none of it, because the fingerprint is already there.
	Segments   int `json:"segments"`
	Duplicates int `json:"duplicates"`
	// Queued is how many joins were set going. It counts every pending segment
	// group, not just the new ones, so a job lost to a crash is picked back up.
	Queued int `json:"queued"`
	// Signed and Assets say how much of the library the duplicate half could
	// actually see. A scan over a tenth of the signatures is not a small answer
	// to the question, it is an answer to a different question, and the review
	// page says so rather than reporting "3 groups" as though that were all.
	Signed int64 `json:"signed"`
	Assets int64 `json:"assets"`

	Took time.Duration `json:"-"`
}
