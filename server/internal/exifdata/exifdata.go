// Package exifdata reads metadata out of an original by shelling out to
// exiftool. It builds argv, runs it, parses the JSON, and returns a struct —
// deliberately nothing more. Deciding what to do with a missing capture time
// belongs to the worker, not here.
//
// exiftool rather than a Go EXIF library because the archive is mostly HEIC and
// MOV, where the metadata lives in HEIF and QuickTime boxes rather than a JPEG
// APP1 segment, and Apple records the timezone in a maker-note-adjacent tag
// that the Go libraries do not read.
package exifdata

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type Reader struct {
	// Binary is the exiftool entry point, overridable for hosts where it is not
	// on PATH.
	Binary string
}

func New() *Reader { return &Reader{Binary: "exiftool"} }

// Data is what a file was willing to tell us. Every field is optional: a
// screenshot has no camera, a messaging app strips EXIF entirely, and a video
// has no orientation tag in the photographic sense.
type Data struct {
	Width       *int
	Height      *int
	Orientation *int

	CameraMake  string
	CameraModel string
	Lens        string

	GPSLat *float64
	GPSLon *float64

	// CapturedAt is the capture instant, already converted to UTC.
	CapturedAt *time.Time
	// OffsetMinutes is minutes east of UTC, present only when the file actually
	// recorded a timezone. When it is nil, CapturedAt was interpreted as UTC
	// because there was nothing better to assume — see parseCapture.
	OffsetMinutes *int

	DurationSeconds *float64

	// ContentID is Apple's content identifier: the UUID both halves of a Live
	// Photo carry, uppercased. It is the only thing that ties a still to its
	// video once they are two files on a disk — the phone's upload declaration
	// is unavailable to anything the phone did not upload, and an export's
	// filenames agree only by convention.
	//
	// Empty on anything an iPhone did not capture, and on plenty it did: a
	// screenshot has none, and neither does an ordinary video.
	ContentID string
}

// DisplaySize returns the dimensions as the image is meant to be seen, swapping
// them when the orientation tag says the sensor was read sideways.
//
// A phone held upright records 4032x3024 with "Rotate 90 CW", not 3024x4032.
// Reporting the raw pair would label every portrait photo as landscape, and
// -auto-orient means every rendition we generate is already rotated — so the
// raw numbers would not even describe our own output.
func (d Data) DisplaySize() (width, height *int) {
	width, height = d.Width, d.Height
	if d.Orientation == nil {
		return width, height
	}
	// 5 through 8 are the four orientations that involve a 90-degree turn.
	switch *d.Orientation {
	case 5, 6, 7, 8:
		return height, width
	}
	return width, height
}

// The tags requested, kept explicit so the JSON stays small and so it is
// obvious what this package depends on. The date tags are listed in preference
// order and read that way in parseCapture.
var tags = []string{
	"-ImageWidth", "-ImageHeight", "-Orientation",
	"-SubSecDateTimeOriginal", "-DateTimeOriginal", "-CreateDate", "-MediaCreateDate",
	"-OffsetTimeOriginal", "-OffsetTime",
	"-Make", "-Model", "-LensModel",
	"-GPSLatitude", "-GPSLongitude",
	"-Duration",
	// One tag name, two entirely different places. exiftool resolves
	// -ContentIdentifier to the Apple maker note on a HEIC or JPEG and to the
	// QuickTime keys atom on a MOV or MP4, which is exactly the pair of
	// locations a Live Photo's two halves keep it in. MediaGroupUUID is the
	// same maker-note value under the name older exiftool builds used.
	"-ContentIdentifier", "-MediaGroupUUID",
}

// raw mirrors exiftool's JSON. Numbers come back as numbers because of -n;
// dates stay strings in EXIF's own format regardless.
type raw struct {
	ImageWidth  *int `json:"ImageWidth"`
	ImageHeight *int `json:"ImageHeight"`
	Orientation *int `json:"Orientation"`

	SubSecDateTimeOriginal string `json:"SubSecDateTimeOriginal"`
	DateTimeOriginal       string `json:"DateTimeOriginal"`
	CreateDate             string `json:"CreateDate"`
	MediaCreateDate        string `json:"MediaCreateDate"`

	OffsetTimeOriginal string `json:"OffsetTimeOriginal"`
	OffsetTime         string `json:"OffsetTime"`

	Make      string `json:"Make"`
	Model     string `json:"Model"`
	LensModel string `json:"LensModel"`

	GPSLatitude  *float64 `json:"GPSLatitude"`
	GPSLongitude *float64 `json:"GPSLongitude"`

	Duration *float64 `json:"Duration"`

	ContentIdentifier string `json:"ContentIdentifier"`
	MediaGroupUUID    string `json:"MediaGroupUUID"`
}

// Read runs exiftool over one file.
//
// A file exiftool cannot parse is not an error here: it returns empty Data. The
// archive holds whatever the phone sent, and an asset with no readable metadata
// still deserves a thumbnail and a place on the timeline.
func (r *Reader) Read(ctx context.Context, path string) (Data, error) {
	args := append([]string{
		"-json",
		"-n",                         // numeric values: signed GPS, numeric orientation, seconds for duration
		"-api", "LargeFileSupport=1", // multi-GB videos
		"-charset", "filename=UTF8",
	}, tags...)
	args = append(args, path)

	cmd := exec.CommandContext(ctx, r.binary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return Data{}, fmt.Errorf("exiftool %s: %w: %s", path, err, bytes.TrimSpace(stderr.Bytes()))
	}

	var parsed []raw
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return Data{}, fmt.Errorf("parse exiftool output for %s: %w", path, err)
	}
	if len(parsed) == 0 {
		return Data{}, nil
	}
	return parsed[0].toData(), nil
}

// Scanned is one file a tree scan could say something about.
type Scanned struct {
	Path string
	// ContentID is the Apple content identifier, empty when the file has none.
	ContentID string
	// MIMEType is what exiftool made of the file, which is the same question
	// mediatype asks of an upload and a more informed answer: it comes from
	// parsing the container rather than from sniffing 512 bytes, and an export
	// contains files whose extension lies about them — the sample has a PNG
	// that is a JPEG.
	MIMEType string
}

// IsVideo reports whether the file is a video, which is what decides which half
// of a Live Photo it could be.
func (s Scanned) IsVideo() bool { return strings.HasPrefix(strings.ToLower(s.MIMEType), "video/") }

// ScanTree reads the pairing-relevant metadata off every file under root,
// calling fn once per file exiftool could identify.
//
// One exiftool process for the whole tree rather than Read per file, which is
// the difference between a few seconds and half an hour on a hundred-thousand
// file export: almost all of Read's cost is process startup, and an import has
// to know every identifier in the tree before it can upload the first file —
// it decides which video belongs to which still, and that decision has to be
// made before anything is sent.
//
// Errors exiftool reports about individual files are ignored. A tree that has
// been through a phone, a cloud, and a zip contains files nothing can parse,
// and none of them are a reason to refuse to import the rest.
func (r *Reader) ScanTree(ctx context.Context, root string, fn func(Scanned) error) error {
	cmd := exec.CommandContext(ctx, r.binary(),
		"-json", "-n",
		"-q", "-q", // suppress the per-file warnings and the trailing summary
		"-r",
		"-api", "LargeFileSupport=1",
		"-charset", "filename=UTF8",
		"-ContentIdentifier", "-MediaGroupUUID", "-MIMEType",
		root,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe exiftool output: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start exiftool: %w", err)
	}
	// Decoded a record at a time. The whole-tree JSON for a large export is
	// tens of megabytes, and there is no reason for any of it to be resident
	// once the caller has been handed it.
	scanErr := scanTree(bufio.NewReaderSize(stdout, 64*1024), fn)

	// Drained either way: a decoder that stopped early leaves exiftool blocked
	// on a write to a pipe nobody is reading, and Wait would never return.
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil && scanErr == nil {
		// Exit status 1 with no output at all is "found nothing to read",
		// which an empty or media-free directory legitimately produces.
		return fmt.Errorf("exiftool %s: %w: %s", root, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return scanErr
}

func scanTree(r io.Reader, fn func(Scanned) error) error {
	dec := json.NewDecoder(r)

	// The opening bracket. Absent when exiftool found nothing, which is not an
	// error — it is an export directory holding no readable media.
	if _, err := dec.Token(); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("read exiftool output: %w", err)
	}

	for dec.More() {
		var entry struct {
			SourceFile        string `json:"SourceFile"`
			ContentIdentifier string `json:"ContentIdentifier"`
			MediaGroupUUID    string `json:"MediaGroupUUID"`
			MIMEType          string `json:"MIMEType"`
		}
		if err := dec.Decode(&entry); err != nil {
			return fmt.Errorf("parse exiftool output: %w", err)
		}
		if entry.SourceFile == "" {
			continue
		}
		err := fn(Scanned{
			Path:      entry.SourceFile,
			ContentID: NormalizeContentID(cmp.Or(entry.ContentIdentifier, entry.MediaGroupUUID)),
			MIMEType:  entry.MIMEType,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Reader) binary() string {
	if r.Binary == "" {
		return "exiftool"
	}
	return r.Binary
}

func (x raw) toData() Data {
	d := Data{
		Width:           x.ImageWidth,
		Height:          x.ImageHeight,
		Orientation:     x.Orientation,
		CameraMake:      strings.TrimSpace(x.Make),
		CameraModel:     strings.TrimSpace(x.Model),
		Lens:            strings.TrimSpace(x.LensModel),
		GPSLat:          x.GPSLatitude,
		GPSLon:          x.GPSLongitude,
		DurationSeconds: x.Duration,
		ContentID:       NormalizeContentID(cmp.Or(x.ContentIdentifier, x.MediaGroupUUID)),
	}

	// Preference order: the most specific tag that a file actually carries.
	// SubSec first because it keeps fractional seconds and often the offset too;
	// MediaCreateDate last because QuickTime records it in UTC with no offset.
	for _, candidate := range []string{
		x.SubSecDateTimeOriginal, x.DateTimeOriginal, x.CreateDate, x.MediaCreateDate,
	} {
		at, offset, ok := parseCapture(candidate, x.OffsetTimeOriginal, x.OffsetTime)
		if ok {
			d.CapturedAt = &at
			d.OffsetMinutes = offset
			break
		}
	}
	return d
}

// NormalizeContentID puts an Apple content identifier into the one spelling the
// archive compares, and rejects anything that is not one.
//
// Both halves of a pair must normalize identically or they will never meet, and
// they arrive by different routes: read out of a maker note here, declared in an
// upload header by a client that read it somewhere else, replayed from a
// manifest line written by an older build. Uppercasing and unwrapping braces
// settles the spellings that actually occur.
//
// The shape check is the part that matters. Pairing on this value hides one
// asset behind another, so a tag holding something that is not a UUID — a
// camera that reuses the name for a serial number, a file a tool rewrote — is
// discarded rather than matched on. Two such files sharing a value would
// otherwise hide one of them.
func NormalizeContentID(raw string) string {
	id := strings.ToUpper(strings.TrimSpace(raw))
	id = strings.TrimSuffix(strings.TrimPrefix(id, "{"), "}")
	if !isCanonicalUUID(id) {
		return ""
	}
	return id
}

// isCanonicalUUID reports whether s is 8-4-4-4-12 hex.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// parseCapture turns EXIF's "2026:04:30 18:12:00" into an instant.
//
// EXIF stores wall-clock time with no zone. If the file carries an offset —
// either appended to the timestamp or in OffsetTimeOriginal — the result is
// exact. If it does not, the time is interpreted as UTC and the offset is
// reported as nil, which is a documented lie: the instant may be up to 14 hours
// out. It is still the best available answer, the caller keeps the phone's
// capture time alongside it, and a nil offset tells the viewer not to claim it
// knows the local time.
func parseCapture(value string, offsets ...string) (time.Time, *int, bool) {
	value = strings.TrimSpace(value)
	// exiftool renders an unset date as all zeroes rather than omitting it.
	if value == "" || strings.HasPrefix(value, "0000") {
		return time.Time{}, nil, false
	}

	// A trailing zone on the timestamp wins: it belongs to this tag, whereas
	// OffsetTimeOriginal is a separate tag that may describe a different one.
	if at, offset, ok := parseWithZone(value); ok {
		return at, &offset, true
	}

	for _, raw := range offsets {
		offset, ok := parseOffset(raw)
		if !ok {
			continue
		}
		at, ok := parseNaive(value)
		if !ok {
			return time.Time{}, nil, false
		}
		return at.Add(-time.Duration(offset) * time.Minute).UTC(), &offset, true
	}

	at, ok := parseNaive(value)
	if !ok {
		return time.Time{}, nil, false
	}
	return at.UTC(), nil, true
}

var naiveLayouts = []string{
	"2006:01:02 15:04:05.999999",
	"2006:01:02 15:04:05",
	"2006:01:02 15:04",
}

// parseNaive reads a zone-less EXIF timestamp as if it were UTC. The caller
// then shifts it by the real offset, or accepts the assumption.
func parseNaive(value string) (time.Time, bool) {
	for _, layout := range naiveLayouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var zonedLayouts = []string{
	"2006:01:02 15:04:05.999999-07:00",
	"2006:01:02 15:04:05-07:00",
	"2006:01:02 15:04:05.999999Z07:00",
	"2006:01:02 15:04:05Z07:00",
}

func parseWithZone(value string) (time.Time, int, bool) {
	for _, layout := range zonedLayouts {
		t, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		_, seconds := t.Zone()
		return t.UTC(), seconds / 60, true
	}
	return time.Time{}, 0, false
}

// parseOffset reads OffsetTimeOriginal, which is "+05:30", "-04:00", or "Z".
func parseOffset(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if value == "Z" || value == "z" {
		return 0, true
	}

	t, err := time.Parse("-07:00", value)
	if err != nil {
		return 0, false
	}
	_, seconds := t.Zone()
	return seconds / 60, true
}
