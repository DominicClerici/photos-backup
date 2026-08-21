// Package mediatype classifies an upload into a content type, a canonical file
// extension, and an image/video kind.
//
// A filename's extension is a claim and its leading bytes are evidence. The
// extension is trusted while nothing contradicts it — when it is absent or
// unrecognised the bytes settle it, which is not a nicety: a Google Takeout
// export strips the extension off every Live Photo's paired video, and without
// sniffing those arrive as application/octet-stream, get filed as images, and
// fail their thumbnail with no playback rendition queued.
//
// When the two disagree outright the bytes win, because a name is what some
// other system decided to call the file and the bytes are what it actually is.
// iCloud Shared Albums are the case that forced this: Apple re-encodes every
// shared photograph to JPEG and goes on calling it IMG_6822.HEIC, so trusting
// the name stored 39 of this archive's 45 shared stills as HEIC — a label no
// browser will open and no download will name correctly.
//
// Disagreement means disagreement about the file, not about vocabulary. See
// agrees(): an extension that merely names its container more precisely than a
// sniff can keeps its say.
package mediatype

import (
	"bytes"
	"path/filepath"
	"strings"
)

// SniffLen is how many leading bytes Sniff wants. Every signature it knows
// lives in the first 16; the rest is headroom for ones it does not yet.
const SniffLen = 512

// Octet is what an upload is called when neither its name nor its bytes say
// anything. It is stored and served like anything else — an archive that
// refuses files it cannot classify is not an archive.
const Octet = "application/octet-stream"

const (
	KindImage = "image"
	KindVideo = "video"
)

var byExt = map[string]string{
	".heic": "image/heic",
	".heif": "image/heic",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".dng":  "image/x-adobe-dng",
	".mov":  "video/quicktime",
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".avi":  "video/x-msvideo",
	".webm": "video/webm",
}

var canonicalExt = map[string]string{
	"image/heic":        ".heic",
	"image/jpeg":        ".jpg",
	"image/png":         ".png",
	"image/gif":         ".gif",
	"image/webp":        ".webp",
	"image/tiff":        ".tiff",
	"image/x-adobe-dng": ".dng",
	"video/quicktime":   ".mov",
	"video/mp4":         ".mp4",
	"video/x-msvideo":   ".avi",
	"video/webm":        ".webm",
}

// ISO base media brands, read out of the `ftyp` box. Go's own
// http.DetectContentType only recognises brands containing "mp4", so it calls
// every HEIC and every QuickTime movie an octet-stream — which is most of what
// an iPhone produces.
var ftypBrands = map[string]string{
	"qt  ": "video/quicktime",
	"isom": "video/mp4",
	"iso2": "video/mp4",
	"iso4": "video/mp4",
	"iso5": "video/mp4",
	"iso6": "video/mp4",
	"mp41": "video/mp4",
	"mp42": "video/mp4",
	"avc1": "video/mp4",
	"mmp4": "video/mp4",
	"dash": "video/mp4",
	"M4V ": "video/mp4",
	"M4A ": "video/mp4",
	"heic": "image/heic",
	"heix": "image/heic",
	"heim": "image/heic",
	"heis": "image/heic",
	"hevc": "image/heic",
	"hevm": "image/heic",
	"hevs": "image/heic",
	"mif1": "image/heic",
	"msf1": "image/heic",
}

// Detect reports the content type and the extension the blob should be stored
// under.
//
// The returned extension is normalized and always leads with a dot, unless
// nothing identified the file at all — an unclassifiable blob keeps whatever
// extension its name carried, including none.
func Detect(filename string, head []byte) (contentType, ext string) {
	ext = normalizeExt(filepath.Ext(filename))
	named, sniffed := byExt[ext], Sniff(head)

	switch {
	case named == "" && sniffed == "":
		return Octet, ext
	case named == "":
		return sniffed, canonicalExt[sniffed]
	case sniffed == "" || agrees(named, sniffed):
		return named, ext
	default:
		return sniffed, canonicalExt[sniffed]
	}
}

// agrees reports whether a name's claim and the bytes' evidence are describing
// the same file, allowing for the two ways they can differ without either being
// wrong.
//
// The first is precision. A DNG is a TIFF carrying extra tags, and telling them
// apart means walking the IFD for SubIFDs — so every raw file in the archive
// sniffs as image/tiff, and a rule of "bytes win" would quietly restore the
// bug Sniff's own comment describes.
//
// The second is container brands. MP4 and QuickTime are the same ISO base media
// format under two names, and which one a file declares says more about the
// camera that wrote it than about what is inside: measured against this
// archive, a quarter of its videos declare the brand of the other extension.
// Relabelling them would rename thousands of blobs to change nothing a player
// can perceive.
func agrees(named, sniffed string) bool {
	if named == sniffed {
		return true
	}
	if refines[named] == sniffed {
		return true
	}
	return isoBaseMedia[named] && isoBaseMedia[sniffed]
}

// Content types that are a narrower reading of another — the key is what the
// extension claims, the value what the bytes can be expected to say.
var refines = map[string]string{
	"image/x-adobe-dng": "image/tiff",
}

// The container the sniffer can identify but not name any more exactly than the
// extension already does.
var isoBaseMedia = map[string]bool{
	"video/mp4":       true,
	"video/quicktime": true,
}

// FromExt reports the content type an extension implies, or "" when it implies
// nothing.
func FromExt(ext string) string {
	return byExt[normalizeExt(ext)]
}

// ExtFor reports the extension a content type is stored under, or "" for one
// with no canonical extension.
func ExtFor(contentType string) string {
	return canonicalExt[strings.ToLower(strings.TrimSpace(contentType))]
}

// Kind classifies a content type as image or video. Anything unrecognised is an
// image, which is the assumption that degrades best: an unidentifiable file
// gets one failed thumbnail rather than a transcode that can never work.
func Kind(contentType string) string {
	if strings.HasPrefix(strings.ToLower(contentType), "video/") {
		return KindVideo
	}
	return KindImage
}

// Sniff reports the content type implied by a file's leading bytes, or "" when
// it recognises nothing. It never needs more than SniffLen bytes and tolerates
// fewer.
func Sniff(head []byte) string {
	switch {
	case len(head) >= 3 && bytes.Equal(head[:3], []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "video/webm"
	case bytes.HasPrefix(head, []byte("II\x2A\x00")), bytes.HasPrefix(head, []byte("MM\x00\x2A")):
		// DNG is TIFF with extra tags; telling them apart means walking the IFD
		// for SubIFDs, and nothing downstream treats them differently.
		return "image/tiff"
	}

	// RIFF containers name their form in bytes 8..12.
	if len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) {
		switch string(head[8:12]) {
		case "WEBP":
			return "image/webp"
		case "AVI ":
			return "video/x-msvideo"
		}
	}

	return sniffISOBMFF(head)
}

// Top-level box types that only ever appear in a QuickTime or ISO base media
// file. A file that opens with one of these and carries no `ftyp` is classic
// QuickTime: iOS writes plenty of movies that way, with `moov` at the very end
// and nothing at the front to identify them. libmagic calls them "data".
var quickTimeBoxes = map[string]bool{
	"wide": true,
	"mdat": true,
	"moov": true,
	"free": true,
	"skip": true,
	"pnot": true,
}

// sniffISOBMFF reads the brands out of an ISO base media `ftyp` box: the major
// brand at bytes 8..12, then the compatible brands that follow. HEIC files
// routinely declare a major brand of "mif1" and only name "heic" further down
// the list, so stopping at the major brand would miss them.
func sniffISOBMFF(head []byte) string {
	if len(head) < 12 {
		return ""
	}
	if !bytes.Equal(head[4:8], []byte("ftyp")) {
		// An MP4 always declares an ftyp; a QuickTime movie need not.
		if quickTimeBoxes[string(head[4:8])] && boxSizeAt(head, 0) >= 8 {
			return "video/quicktime"
		}
		return ""
	}

	// The box length is untrusted input; clamp it to what was actually read.
	boxSize := boxSizeAt(head, 0)
	if boxSize > len(head) || boxSize < 12 {
		boxSize = len(head)
	}

	for off := 8; off+4 <= boxSize; off += 4 {
		// Bytes 12..16 are the major brand's version number, not a brand.
		if off == 12 {
			continue
		}
		if ct := ftypBrands[string(head[off:off+4])]; ct != "" {
			return ct
		}
	}
	return ""
}

// boxSizeAt reads a big-endian ISO base media box length.
func boxSizeAt(head []byte, off int) int {
	if off+4 > len(head) {
		return 0
	}
	return int(uint32(head[off])<<24 | uint32(head[off+1])<<16 | uint32(head[off+2])<<8 | uint32(head[off+3]))
}

func normalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}
