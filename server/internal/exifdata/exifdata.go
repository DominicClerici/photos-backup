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
	"math"
	"os/exec"
	"strconv"
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
	// Raw is every tag the file carries, group-qualified, exactly as exiftool
	// rendered it.
	//
	// It is here for the same reason an import sidecar is stored verbatim: the
	// fields below are a choice made today about what is worth a column, and
	// the choice is wrong about something. Unlike a sidecar this one could
	// always be re-read from the original — but only by putting the entire
	// archive back through exiftool, which is hours to recover what cost a
	// few kilobytes to keep.
	//
	// It used to hold only the tags in the list below, which made it a copy of
	// the same choice rather than an escape from it: an audit of the real
	// export found 297 distinct tags in the files that were never asked for,
	// 162 of them on more than a tenth of the library. Asking for everything is
	// what makes the guarantee the sentence above claims.
	Raw json.RawMessage

	Width       *int
	Height      *int
	Orientation *int

	CameraMake  string
	CameraModel string
	Lens        string

	GPSLat *float64
	GPSLon *float64
	// GPSAltitude is metres above sea level; Direction is the compass bearing
	// the camera faced; Accuracy is the horizontal error the phone reported.
	GPSAltitude  *float64
	GPSDirection *float64
	GPSAccuracy  *float64
	// GPSAt is the fix's own timestamp, which is UTC by definition and so is
	// the one time in the file that needs no zone guessed for it.
	GPSAt *time.Time

	// The exposure. Half the real library carries all of it and the other half
	// none: a screenshot, a saved image and anything a messaging app rewrote
	// have no camera behind them to have set any of it.
	ISO             *int
	FNumber         *float64
	ExposureSeconds *float64
	FocalLength     *float64
	FocalLength35   *int
	Flash           *int

	// Description is the caption the file itself carries, which is not the same
	// caption an export's sidecar carries and does not overwrite it.
	Description string
	// ColorProfile is the ICC profile's name, e.g. "Display P3". It is what
	// says a rendition needs converting rather than assuming sRGB.
	ColorProfile string
	// CaptureType is Apple's ImageCaptureType: 10 on a Live Photo's still, 12
	// on a portrait, and so on. Recorded rather than interpreted.
	CaptureType *int

	// The video stream, read from the container rather than from ffprobe: this
	// file is already being opened, and one subprocess that answers is cheaper
	// than a second that answers the same.
	VideoCodec    string
	FrameRate     *float64
	Bitrate       *int64
	AudioCodec    string
	AudioChannels *int

	// Faces are the regions something already found in this image, normalized
	// out of XMP's parallel arrays. Not identities: no name survives in the
	// real library, only boxes. They are kept so v2's own face work has
	// something to reconcile against rather than start from.
	Faces []Face

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

// Face is one region a camera or a cloud service marked, in fractions of the
// image so it survives every rendition the archive makes.
type Face struct {
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
	Type string  `json:"type,omitempty"`
	Name string  `json:"name,omitempty"`
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

// The tags the fields above are parsed from, and nothing else.
//
// This list used to do two jobs: name what gets decoded, and name what gets
// kept for the record. The second job is Raw's now that Raw is the whole file,
// so the tags that were only ever read to be stored — Software, HostComputer,
// SceneCaptureType, CompositeImage, ImageUniqueID, BurstUUID,
// LivePhotoVideoIndex, HDRGain, HDRHeadroom, AuxiliaryImageType, MajorBrand,
// ColorSpace, Subject, Keywords, Rating, Title — are gone from here. They are
// captured better by the wide pass, which qualifies them by group and cannot
// silently return a different tag of the same name.
//
// The date tags are listed in preference order and read that way in
// parseCapture.
var tags = []string{
	"-ImageWidth", "-ImageHeight", "-Orientation",
	"-SubSecDateTimeOriginal", "-DateTimeOriginal", "-CreateDate", "-MediaCreateDate",
	// QuickTime's own local-time tag. CreateDate beside it is UTC with no zone,
	// so a video read without this one lands with the right instant and no idea
	// what time it was where it was shot — 4,526 of the real export's 15,689
	// files, every one of them a video.
	"-CreationDate",
	"-OffsetTimeOriginal", "-OffsetTime",
	"-Make", "-Model", "-LensModel",
	"-GPSLatitude", "-GPSLongitude",
	"-GPSAltitude", "-GPSImgDirection", "-GPSHPositioningError", "-GPSDateTime",
	// The exposure. None of it changes what the archive does; all of it is what
	// anyone actually wants to read on a photo they have opened.
	"-ISO", "-FNumber", "-ExposureTime", "-FocalLength", "-FocalLengthIn35mmFormat", "-Flash",
	// The caption a file carries in its own right, in the four places files
	// keep one.
	"-ImageDescription", "-UserComment", "-XMP:Description", "-Caption-Abstract",
	// The colour space a rendition has to be converted from, and Apple's own
	// note of what kind of capture this was.
	"-ProfileDescription", "-ImageCaptureType",
	// The video stream. exiftool is already open on this file; ffprobe answers
	// the same questions from a second process.
	"-CompressorID", "-VideoFrameRate", "-AvgBitrate", "-AudioFormat", "-AudioChannels",
	// Faces, as XMP writes them: five parallel arrays and a pixel size to
	// divide by. 1,470 files in the real export carry them.
	"-RegionType", "-RegionName", "-RegionAreaX", "-RegionAreaY", "-RegionAreaW", "-RegionAreaH",
	// The pixel size a region list was measured against, for the regions that
	// are not written as fractions. Nothing reads it yet — faces() refuses a
	// list it cannot normalize — and it stays here so that when something does,
	// the value is beside the arrays it belongs to rather than in Raw.
	"-RegionAreaUnit", "-RegionAppliedToDimensionsW", "-RegionAppliedToDimensionsH",
	"-Duration",
	// One tag name, two entirely different places. exiftool resolves
	// -ContentIdentifier to the Apple maker note on a HEIC or JPEG and to the
	// QuickTime keys atom on a MOV or MP4, which is exactly the pair of
	// locations a Live Photo's two halves keep it in. MediaGroupUUID is the
	// same maker-note value under the name older exiftool builds used.
	"-ContentIdentifier", "-MediaGroupUUID",
}

// raw mirrors exiftool's JSON.
//
// Every field is one of the two tolerant types below rather than *int, *float64
// or string, because exiftool renders a tag as whatever the file put in it and
// files are full of surprises. A 2008 JPEG in the real library records an ISO of
// 75.4582213796711; decoded into *int that is not a type error in one tag, it is
// an error for the whole record, which fails the metadata job, retries four more
// times and finally marks a perfectly good photograph broken — thumbnail and
// all — over a number nothing was going to display.
type raw struct {
	ImageWidth  number `json:"ImageWidth"`
	ImageHeight number `json:"ImageHeight"`
	Orientation number `json:"Orientation"`

	SubSecDateTimeOriginal text `json:"SubSecDateTimeOriginal"`
	DateTimeOriginal       text `json:"DateTimeOriginal"`
	CreateDate             text `json:"CreateDate"`
	MediaCreateDate        text `json:"MediaCreateDate"`

	OffsetTimeOriginal text `json:"OffsetTimeOriginal"`
	OffsetTime         text `json:"OffsetTime"`

	Make      text `json:"Make"`
	Model     text `json:"Model"`
	LensModel text `json:"LensModel"`

	GPSLatitude  number `json:"GPSLatitude"`
	GPSLongitude number `json:"GPSLongitude"`

	CreationDate text `json:"CreationDate"`

	GPSAltitude          number `json:"GPSAltitude"`
	GPSImgDirection      number `json:"GPSImgDirection"`
	GPSHPositioningError number `json:"GPSHPositioningError"`
	GPSDateTime          text   `json:"GPSDateTime"`

	ISO                     number `json:"ISO"`
	FNumber                 number `json:"FNumber"`
	ExposureTime            number `json:"ExposureTime"`
	FocalLength             number `json:"FocalLength"`
	FocalLengthIn35mmFormat number `json:"FocalLengthIn35mmFormat"`
	Flash                   number `json:"Flash"`

	ImageDescription text `json:"ImageDescription"`
	UserComment      text `json:"UserComment"`
	XMPDescription   text `json:"Description"`
	CaptionAbstract  text `json:"Caption-Abstract"`

	ProfileDescription text   `json:"ProfileDescription"`
	ImageCaptureType   number `json:"ImageCaptureType"`

	CompressorID   text   `json:"CompressorID"`
	VideoFrameRate number `json:"VideoFrameRate"`
	AvgBitrate     number `json:"AvgBitrate"`
	AudioFormat    text   `json:"AudioFormat"`
	AudioChannels  number `json:"AudioChannels"`

	// XMP writes a region list as one array per attribute, all of them the same
	// length. exiftool collapses a single-element array to a bare value, so
	// every one of these has to survive arriving as either.
	RegionType     stringList `json:"RegionType"`
	RegionName     stringList `json:"RegionName"`
	RegionAreaX    floatList  `json:"RegionAreaX"`
	RegionAreaY    floatList  `json:"RegionAreaY"`
	RegionAreaW    floatList  `json:"RegionAreaW"`
	RegionAreaH    floatList  `json:"RegionAreaH"`
	RegionAreaUnit stringList `json:"RegionAreaUnit"`

	Duration number `json:"Duration"`

	ContentIdentifier text `json:"ContentIdentifier"`
	MediaGroupUUID    text `json:"MediaGroupUUID"`
}

// number is a numeric tag, however the file spelled it.
//
// A tag that is not a number at all — "undef", "inf", an object — reads as
// absent rather than as an error, which is this package's rule everywhere: a
// file that will not explain itself still deserves a thumbnail and a place on
// the timeline.
type number struct {
	set   bool
	value float64
}

func (n *number) UnmarshalJSON(data []byte) error {
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		*n = number{}
		return nil
	}

	switch value := decoded.(type) {
	case float64:
		*n = number{set: true, value: value}
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			*n = number{}
			return nil
		}
		*n = number{set: true, value: parsed}
	default:
		*n = number{}
		return nil
	}

	// Infinities and NaN are not measurements, and a column is a poor place for
	// either.
	if math.IsInf(n.value, 0) || math.IsNaN(n.value) {
		*n = number{}
	}
	return nil
}

func (n number) Float() *float64 {
	if !n.set {
		return nil
	}
	value := n.value
	return &value
}

// Int rounds, which is what a fractional ISO or a 59.94 frame count needs: the
// tag is an integer everywhere it matters and the fraction is the file being
// odd, not a measurement anyone wanted.
func (n number) Int() *int {
	if !n.set {
		return nil
	}
	value := int(math.Round(n.value))
	return &value
}

func (n number) Int64() *int64 {
	if !n.set {
		return nil
	}
	value := int64(math.Round(n.value))
	return &value
}

// text is a string tag that may not arrive as a string. Real files render
// Software as 12.4 and ImageDescription as a number often enough that the
// distinction is not worth failing over.
type text string

func (t *text) UnmarshalJSON(data []byte) error {
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		*t = ""
		return nil
	}

	switch value := decoded.(type) {
	case string:
		*t = text(value)
	case float64:
		*t = text(strconv.FormatFloat(value, 'f', -1, 64))
	case bool:
		*t = text(strconv.FormatBool(value))
	default:
		*t = ""
	}
	return nil
}

func (t text) String() string { return string(t) }

// stringList and floatList read a tag that exiftool renders as a bare value
// when there is one of it and as an array when there are several. Numbers in a
// region list also come back quoted often enough that the string form has to be
// accepted for both.
type stringList []string

func (l *stringList) UnmarshalJSON(data []byte) error { return unmarshalList(data, l) }

type floatList []float64

func (l *floatList) UnmarshalJSON(data []byte) error {
	var raw stringList
	if err := unmarshalList(data, &raw); err != nil {
		return err
	}

	out := make(floatList, 0, len(raw))
	for _, value := range raw {
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			// Dropped rather than raised. One unparseable entry desynchronizes
			// the parallel arrays and a box built from mismatched columns is
			// worse than no box — but this is a face rectangle in a file that
			// is otherwise fine, and refusing the whole record would fail the
			// metadata job, lose the exposure and the capture time with it, and
			// eventually mark the asset broken over an XMP oddity.
			*l = nil
			return nil
		}
		out = append(out, f)
	}
	*l = out
	return nil
}

// unmarshalList reads a tag that may be an array or a bare value. Anything it
// cannot make sense of reads as absent, for the reason above: the raw JSON
// keeps whatever it was, and no tag here is worth failing a file over.
func unmarshalList(data []byte, into *stringList) error {
	var many []any
	if err := json.Unmarshal(data, &many); err == nil {
		out := make(stringList, 0, len(many))
		for _, value := range many {
			out = append(out, fmt.Sprint(value))
		}
		*into = out
		return nil
	}

	var one any
	if err := json.Unmarshal(data, &one); err != nil {
		*into = nil
		return nil
	}
	*into = stringList{fmt.Sprint(one)}
	return nil
}

// maxRawBytes bounds what one file's tag dump may put in a column.
//
// The real export's median is 4KB and its 95th percentile 7.5KB, so this is
// three orders of magnitude of headroom and exists only to stop a pathological
// file — a dashcam clip whose embedded telemetry runs to every frame — from
// putting a hundred megabytes of JSON in one row. Raw is explicitly not the
// recovery path: the original is archived, so exceeding this costs a re-read
// later rather than the data.
const maxRawBytes = 8 << 20

// Read runs exiftool over one file.
//
// One process, two commands, joined by -execute. The first asks for the tags
// the fields above are parsed from; the second asks for everything the file
// carries, which is what Raw keeps.
//
// Two documents rather than one because the two want incompatible output. The
// full dump has to be group-qualified (-G0) or tags of the same name in
// different groups collide — a file can carry Description in both XMP and
// QuickTime, and Caption-Abstract in both IPTC and XMP. But the parse below
// depends on exiftool's own priority between those groups, which is per-tag and
// not reproducible from a group name: -GPSLatitude resolves to the Composite
// tag, which applies GPSLatitudeRef's sign and reads a video's location out of
// QuickTime's GPSCoordinates, where the bare EXIF tag is an unsigned magnitude
// that videos do not have at all. Re-deriving that by hand would put every
// southern and western photograph on the wrong side of the equator. So the
// narrow request stays exactly as it was, and the wide one is additive.
//
// The second pass costs about 25% more wall clock per file and no extra process
// spawn, which is where almost all of exiftool's cost is.
//
// A file exiftool cannot parse is not an error here: it returns empty Data. The
// archive holds whatever the phone sent, and an asset with no readable metadata
// still deserves a thumbnail and a place on the timeline.
func (r *Reader) Read(ctx context.Context, path string) (Data, error) {
	common := []string{
		"-n",                         // numeric values: signed GPS, numeric orientation, seconds for duration
		"-api", "LargeFileSupport=1", // multi-GB videos
		"-charset", "filename=UTF8",
	}

	args := append([]string{"-json"}, common...)
	args = append(args, tags...)
	args = append(args, path, "-execute", "-json")
	args = append(args, common...)
	args = append(args,
		"-a",  // every instance of a tag, not just the highest-priority one
		"-G0", // qualify each by the group it was found in
		"-ee", // the embedded streams: a video's timed GPS track lives here and
		// nowhere else, and 381 files in the sample say so themselves in
		// an exiftool warning nothing was reading
		path)

	cmd := exec.CommandContext(ctx, r.binary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return Data{}, fmt.Errorf("exiftool %s: %w: %s", path, err, bytes.TrimSpace(stderr.Bytes()))
	}

	narrow, full, err := splitDocuments(stdout.Bytes())
	if err != nil {
		return Data{}, fmt.Errorf("parse exiftool output for %s: %w", path, err)
	}

	var parsed []raw
	if err := json.Unmarshal(narrow, &parsed); err != nil {
		return Data{}, fmt.Errorf("parse exiftool output for %s: %w", path, err)
	}
	if len(parsed) == 0 {
		return Data{}, nil
	}

	data := parsed[0].toData()
	data.Raw = keepRaw(full)
	return data, nil
}

// splitDocuments separates the two JSON arrays -execute writes back to back.
//
// A single document is read as the narrow one, which is what an exiftool too
// old to know -execute would produce: the columns keep working and only Raw is
// lost, rather than the file failing outright.
func splitDocuments(stdout []byte) (narrow, full []byte, err error) {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var documents []json.RawMessage
	for {
		var doc json.RawMessage
		switch err := dec.Decode(&doc); {
		case err == io.EOF:
			switch len(documents) {
			case 0:
				return nil, nil, nil
			case 1:
				return documents[0], nil, nil
			default:
				return documents[0], documents[1], nil
			}
		case err != nil:
			return nil, nil, err
		}
		documents = append(documents, doc)
	}
}

// archivePaths are the tags that describe where the blob sits rather than what
// it is a photograph of.
//
// Everything else the file carries is kept, however obscure. These six are
// dropped because they are facts about the content-addressed tree — a hashed
// filename, the directory it hashes into, the times we happened to write and
// read it — and storing them would pin the archive's own layout into every row
// while saying nothing about the photograph. FileSize, FileType, MIMEType and
// the rest of the File group describe the bytes and stay.
var archivePaths = []string{
	"SourceFile",
	"File:FileName",
	"File:Directory",
	"File:FileModifyDate",
	"File:FileAccessDate",
	"File:FileInodeChangeDate",
	"File:FilePermissions",
}

// keepRaw reduces exiftool's full dump to the one object worth storing.
func keepRaw(stdout []byte) json.RawMessage {
	if len(stdout) == 0 {
		return nil
	}
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(stdout, &objects); err != nil || len(objects) == 0 {
		return nil
	}
	for _, tag := range archivePaths {
		delete(objects[0], tag)
	}
	if len(objects[0]) == 0 {
		return nil
	}
	encoded, err := json.Marshal(objects[0])
	if err != nil || len(encoded) > maxRawBytes {
		return nil
	}
	return encoded
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
		// Every file, whatever it is called. Recursing a directory, exiftool
		// reads only the extensions it recognises and skips a file with no
		// extension at all — which is precisely the shape a Google Takeout gives
		// a Live Photo's video half. Left at the default, this scan never sees
		// them, and an importer that asks it what is in the tree never learns
		// they exist: 239 of the 15,689 files in the real export, all of them
		// video. The sidecars are excluded again because they are the one kind
		// of file here that is read by something else.
		"-ext", "*", "--ext", "json",
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
	var read int
	scanErr := scanTree(bufio.NewReaderSize(stdout, 64*1024), func(s Scanned) error {
		read++
		return fn(s)
	})

	// Drained either way: a decoder that stopped early leaves exiftool blocked
	// on a write to a pipe nobody is reading, and Wait would never return.
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil && scanErr == nil && read == 0 {
		// Anything it could describe, it described, so a non-zero exit after a
		// tree it did describe is exiftool reporting the files it could not
		// read — which is the documented contract of this function and not a
		// reason to abandon the ones it could.
		//
		// It has to be tolerated rather than merely tidy: a Snapchat export's
		// chat media contains encrypted blobs that nothing has the key for —
		// 15 of 4,142 in the real one — and exiting non-zero for those was
		// enough to refuse the other 4,127. The same was true, unnoticed, of
		// any single corrupt file anywhere in a 50,000-file Takeout.
		//
		// Nothing read at all is the case still worth failing on: an empty
		// directory produces it, and so does a bad binary or an unreadable
		// path, and the difference between those is in the message.
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
		Width:           x.ImageWidth.Int(),
		Height:          x.ImageHeight.Int(),
		Orientation:     x.Orientation.Int(),
		CameraMake:      strings.TrimSpace(x.Make.String()),
		CameraModel:     strings.TrimSpace(x.Model.String()),
		Lens:            strings.TrimSpace(x.LensModel.String()),
		GPSLat:          x.GPSLatitude.Float(),
		GPSLon:          x.GPSLongitude.Float(),
		GPSAltitude:     x.GPSAltitude.Float(),
		GPSDirection:    x.GPSImgDirection.Float(),
		GPSAccuracy:     x.GPSHPositioningError.Float(),
		ISO:             x.ISO.Int(),
		FNumber:         x.FNumber.Float(),
		ExposureSeconds: x.ExposureTime.Float(),
		FocalLength:     x.FocalLength.Float(),
		FocalLength35:   x.FocalLengthIn35mmFormat.Int(),
		Flash:           x.Flash.Int(),
		ColorProfile:    strings.TrimSpace(x.ProfileDescription.String()),
		CaptureType:     x.ImageCaptureType.Int(),
		VideoCodec:      strings.TrimSpace(x.CompressorID.String()),
		FrameRate:       x.VideoFrameRate.Float(),
		Bitrate:         x.AvgBitrate.Int64(),
		AudioCodec:      strings.TrimSpace(x.AudioFormat.String()),
		AudioChannels:   x.AudioChannels.Int(),
		DurationSeconds: x.Duration.Float(),
		ContentID: NormalizeContentID(
			cmp.Or(x.ContentIdentifier.String(), x.MediaGroupUUID.String())),
		Faces: x.faces(),
	}

	// The first of these that says anything. A screenshot's UserComment is
	// literally "Screenshot" and an empty one is a bare newline, so this is the
	// first that says anything once trimmed.
	for _, candidate := range []text{
		x.ImageDescription, x.XMPDescription, x.CaptionAbstract, x.UserComment,
	} {
		if caption := strings.TrimSpace(candidate.String()); caption != "" {
			d.Description = caption
			break
		}
	}

	// Preference order: the most specific tag that a file actually carries.
	// SubSec first because it keeps fractional seconds and often the offset too;
	// CreationDate ahead of the two UTC QuickTime tags because it is the only
	// one on a video that knows what time it was where the video was taken;
	// MediaCreateDate last because QuickTime records it in UTC with no offset.
	for _, candidate := range []text{
		x.SubSecDateTimeOriginal, x.DateTimeOriginal, x.CreationDate, x.CreateDate, x.MediaCreateDate,
	} {
		at, offset, ok := parseCapture(candidate.String(),
			x.OffsetTimeOriginal.String(), x.OffsetTime.String())
		if ok {
			d.CapturedAt = &at
			d.OffsetMinutes = offset
			break
		}
	}

	// The fix's own time, which is UTC by definition — exiftool renders it with
	// a trailing Z, and it is the one timestamp here that needs nothing assumed.
	if at, _, ok := parseCapture(x.GPSDateTime.String()); ok {
		d.GPSAt = &at
	}
	return d
}

// faces reads XMP's parallel region arrays into rectangles.
//
// Every array has to be the same length as the boxes: a region list where the
// columns disagree is one this cannot line up, and half a face box is worse
// than none. The raw JSON keeps the arrays either way.
func (x raw) faces() []Face {
	n := len(x.RegionAreaX)
	if n == 0 || len(x.RegionAreaY) != n || len(x.RegionAreaW) != n || len(x.RegionAreaH) != n {
		return nil
	}
	// Every region in the real library is written in fractions. One measured in
	// pixels would need the dimensions it was measured against, and storing it
	// as though it were a fraction would put the box in the wrong place.
	for _, unit := range x.RegionAreaUnit {
		if !strings.EqualFold(strings.TrimSpace(unit), "normalized") {
			return nil
		}
	}

	faces := make([]Face, 0, n)
	for i := range n {
		face := Face{X: x.RegionAreaX[i], Y: x.RegionAreaY[i], W: x.RegionAreaW[i], H: x.RegionAreaH[i]}
		if i < len(x.RegionType) {
			face.Type = strings.TrimSpace(x.RegionType[i])
		}
		if i < len(x.RegionName) {
			face.Name = strings.TrimSpace(x.RegionName[i])
		}
		faces = append(faces, face)
	}
	return faces
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
