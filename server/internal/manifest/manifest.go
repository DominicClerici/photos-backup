// Package manifest maintains the append-only recovery log that sits beside the
// blobs. It is the disaster-recovery path when the database is gone, so it is
// written independently of Postgres and never rewritten in place.
package manifest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Kinds of manifest line. A line with no type at all is an asset line, which is
// every line written before imports existed.
const (
	KindAsset    = "asset"
	KindMetadata = "metadata"
	// KindImportOrphan records something an import read but could not attach to
	// an asset: a sidecar whose media file was not in the export, or albums for
	// an item that had no sidecar to carry them.
	//
	// It names no blob, which is why it is its own kind rather than a metadata
	// line with an empty digest. It is here rather than only in the database
	// for the reason every other line is: the export it was read from is
	// usually deleted the week after the import, and an unmatched sidecar is
	// then the only copy of what Google knew about that photograph.
	KindImportOrphan = "import-orphan"
)

// Entry is one line of manifest.jsonl: everything needed to rebuild a database
// row from the blob tree alone.
//
// Two kinds of line share this shape. An asset line records a blob and is
// written once, when the bytes land. A metadata line records something learned
// about a blob afterwards — the contents of an import sidecar — and can be
// written any number of times for the same digest, the last one read winning.
//
// Separate lines rather than a richer asset line because the two facts are
// established at different times by different callers. An import uploads a file
// and then describes it, and folding the description into the upload would mean
// either buffering it in the upload path or rewriting a line in an append-only
// log.
type Entry struct {
	// Type is KindAsset (or empty, which means the same) or KindMetadata.
	Type string `json:"type,omitempty"`

	SHA256 string `json:"sha256"`
	// Omitted when empty so a metadata line, which records no bytes, does not
	// carry a zero-length file's worth of fields describing nothing.
	MD5      string `json:"md5,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Filename string `json:"filename,omitempty"`
	// ContentType and Ext are what the file was classified as on the way in.
	// Ext is also the blob's extension on disk, so a rebuild can find the blob
	// from the manifest alone rather than guessing at the filename — which for
	// an extensionless Takeout video would guess wrong.
	//
	// Both are absent from lines written before classification moved out of the
	// filename; a rebuild re-derives them from the blob's own bytes.
	ContentType string     `json:"content_type,omitempty"`
	Ext         string     `json:"ext,omitempty"`
	CapturedAt  *time.Time `json:"captured_at,omitempty"`
	// ModifiedAt is the local asset's modification time on the source device. It
	// belongs here so a database rebuilt from this log can restore the device
	// mappings complete, rather than forcing every asset through a content check.
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	DeviceID   string     `json:"device_id,omitempty"`
	LocalID    string     `json:"local_id,omitempty"`
	// LiveParentLocalID marks this blob as a Live Photo's paired video and
	// names the still it belongs to. Only the phone knows the two are related,
	// so nothing in the bytes could recover it — which is exactly why it has to
	// be written down here rather than re-derived on a rebuild.
	LiveParentLocalID string `json:"live_parent_local_id,omitempty"`
	// ContentID is Apple's content identifier, the UUID both halves of a Live
	// Photo carry in their own metadata. Unlike LiveParentLocalID this one
	// could be re-derived on a rebuild — but only by running exiftool over
	// every original in the archive, which is hours of work to recover
	// something that costs 36 bytes to write down.
	ContentID string `json:"content_id,omitempty"`

	// The metadata line's payload: an import sidecar exactly as it was
	// received, the importer that received it, and the albums the export's
	// directory structure placed the item in.
	//
	// The sidecar is stored raw rather than reduced to columns so that a
	// rebuild re-runs the current parser over it rather than replaying an old
	// parser's conclusions. Improving what is understood about an export is
	// then a reindex, not a re-import of files that may be long deleted.
	ImportSource  string          `json:"import_source,omitempty"`
	ImportSidecar json.RawMessage `json:"import_sidecar,omitempty"`
	ImportAlbums  []AlbumRef      `json:"import_albums,omitempty"`

	// The orphan line's payload, on top of the three fields above.
	//
	// OrphanKind is "sidecar" or "album". Locator is where the thing sat inside
	// the export — the sidecar's path for the first, the media file's for the
	// second — which is what makes replaying an import idempotent rather than
	// additive. Reason is why it could not be attached, in words, because the
	// point of keeping these is to be read later by someone deciding what to do
	// about them.
	OrphanKind   string `json:"orphan_kind,omitempty"`
	Locator      string `json:"locator,omitempty"`
	OrphanReason string `json:"orphan_reason,omitempty"`

	StoredAt time.Time `json:"stored_at"`
}

// AlbumRef is an album an imported item belonged to, as its source named it.
type AlbumRef struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// IsAsset reports whether this line records a blob, as opposed to describing
// one. Callers walking the log for blobs must ask: a metadata line names a
// digest but no bytes, and reading it as an asset line invents an archived
// file with no size, no type, and no path.
func (e Entry) IsAsset() bool { return e.Type == "" || e.Type == KindAsset }

type Log struct {
	mu   sync.Mutex
	path string
}

func New(path string) *Log {
	return &Log{path: path}
}

// Append writes one entry and fsyncs before returning, so a caller that has
// seen a nil error knows the line survives a crash.
func (l *Log) Append(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal manifest entry: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("write manifest entry: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync manifest: %w", err)
	}
	return nil
}

// Read parses the whole log into memory. Convenient for tests and small
// archives; verify and reindex use Scan instead, because a 100GB library's log
// is hundreds of thousands of lines and there is no reason to hold them all.
func Read(path string) ([]Entry, error) {
	var entries []Entry
	err := Scan(path, func(e Entry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Scan calls fn once per entry, in the order they were appended.
//
// A parse failure names the line rather than aborting silently, and stops the
// scan: a log that has gone unreadable partway through is a fact worth
// surfacing, not one to skip past on the way to a "clean" result.
func Scan(path string, fn func(Entry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	line := 0
	for scanner.Scan() {
		line++
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return fmt.Errorf("parse manifest line %d: %w", line, err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	return nil
}
