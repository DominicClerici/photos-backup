package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/mediatype"
)

// ReindexOptions configures a rebuild.
type ReindexOptions struct {
	// AdoptOrphans also indexes blobs that have no manifest line, reading what
	// it can off the file itself. Without it, a blob the log never recorded
	// stays invisible to the gallery even though the bytes are safe.
	AdoptOrphans bool
	// DryRun reports what would be inserted without touching the database.
	DryRun bool
	// Progress is called per entry.
	Progress func(done int64)
}

// ReindexResult is what a rebuild did.
type ReindexResult struct {
	Lines    int64
	Inserted int64
	Existing int64
	Adopted  int64
	Missing  int64
	Mappings int64
	Elapsed  time.Duration
}

// Reindex rebuilds the database from manifest.jsonl and the blob tree.
//
// This is the recovery path PROJECT.md promises for a lost database, and it is
// why the manifest is written independently of Postgres and fsynced before an
// upload is acked. Every step keys off the SHA-256, so running it against a
// database that is merely incomplete is safe — and running it twice does
// nothing the first run did not.
//
// Derivatives are not rebuilt here. Each restored asset is enqueued as pending
// metadata work and the ordinary worker pool regenerates them, which is the
// same path a fresh upload takes.
func Reindex(ctx context.Context, d Deps, opt ReindexOptions) (ReindexResult, error) {
	started := time.Now()
	var result ReindexResult

	seen := make(map[string]struct{})
	path := filepath.Join(d.PhotosRoot, "manifest.jsonl")

	err := manifest.Scan(path, func(e manifest.Entry) error {
		result.Lines++
		if opt.Progress != nil {
			opt.Progress(result.Lines)
		}
		if e.SHA256 == "" {
			return nil
		}
		seen[e.SHA256] = struct{}{}

		asset, ok, err := assetFromEntry(d, e)
		if err != nil {
			return err
		}
		if !ok {
			// The line describes a blob that is not there. Reported by verify
			// as a manifest orphan; nothing to index here.
			result.Missing++
			return nil
		}

		if opt.DryRun {
			result.Inserted++
			return nil
		}
		return record(ctx, d, asset, &result)
	})
	if err != nil && !os.IsNotExist(err) {
		return result, fmt.Errorf("replay manifest: %w", err)
	}

	if opt.AdoptOrphans {
		if err := adoptOrphans(ctx, d, opt, seen, &result); err != nil {
			return result, err
		}
	}

	result.Elapsed = time.Since(started)
	return result, nil
}

// assetFromEntry turns a manifest line into a row, filling in anything the line
// predates by reading the blob.
func assetFromEntry(d Deps, e manifest.Entry) (db.Asset, bool, error) {
	ext, contentType := e.Ext, e.ContentType

	// Lines written before classification moved off the filename carry neither.
	// The bytes themselves still know what they are.
	if ext == "" || contentType == "" {
		path, found := findBlob(d, e.SHA256, e.Filename)
		if !found {
			return db.Asset{}, false, nil
		}
		contentType, ext = mediatype.Detect(e.Filename, headOf(path))
	}

	if _, err := os.Stat(d.Blobs.Path(e.SHA256, ext)); err != nil {
		if os.IsNotExist(err) {
			return db.Asset{}, false, nil
		}
		return db.Asset{}, false, err
	}

	return db.Asset{
		SHA256:           e.SHA256,
		MD5:              e.MD5,
		ByteSize:         e.Size,
		OriginalFilename: e.Filename,
		Ext:              ext,
		ContentType:      contentType,
		MediaKind:        mediatype.Kind(contentType),
		CapturedAt:       e.CapturedAt,
		ModifiedAt:       e.ModifiedAt,
		DeviceID:         e.DeviceID,
		LocalID:          e.LocalID,
	}, true, nil
}

// findBlob locates a blob whose extension is not known ahead of time, which is
// the case for a manifest line written before the extension was recorded.
func findBlob(d Deps, sha, filename string) (string, bool) {
	// The overwhelmingly likely answer first: whatever the filename implied,
	// which is how those older lines were stored.
	if guess := d.Blobs.Path(sha, filepath.Ext(filename)); exists(guess) {
		return guess, true
	}

	dir := filepath.Dir(d.Blobs.Path(sha, ""))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), sha) {
			return filepath.Join(dir, entry.Name()), true
		}
	}
	return "", false
}

// adoptOrphans indexes blobs the manifest never recorded.
//
// These are the crash-between-rename-and-append case: the archive holds the
// bytes and nothing else knows. Everything about them has to come from the file
// — its name is its digest, its type is in its header, and its capture time
// will be recovered by the metadata worker reading the EXIF.
func adoptOrphans(ctx context.Context, d Deps, opt ReindexOptions, seen map[string]struct{}, result *ReindexResult) error {
	root := filepath.Join(d.PhotosRoot, "blobs")

	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}

		name := entry.Name()
		ext := filepath.Ext(name)
		sha := strings.TrimSuffix(name, ext)
		if len(sha) != 64 {
			return nil
		}
		if _, ok := seen[sha]; ok {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		contentType, detectedExt := mediatype.Detect(name, headOf(path))
		// The file is already committed under this extension; re-deriving one
		// would just point the row at a path that does not exist.
		_ = detectedExt

		asset := db.Asset{
			SHA256:   sha,
			ByteSize: info.Size(),
			// The original filename is gone with the manifest line. The digest
			// is the only true name left.
			OriginalFilename: sha[:16] + ext,
			Ext:              ext,
			ContentType:      contentType,
			MediaKind:        mediatype.Kind(contentType),
		}

		result.Adopted++
		if opt.DryRun {
			return nil
		}
		return record(ctx, d, asset, result)
	})
}

// record inserts an asset and restores its device mapping.
func record(ctx context.Context, d Deps, a db.Asset, result *ReindexResult) error {
	id, inserted, err := d.Store.RecordAsset(ctx, a)
	if err != nil {
		return fmt.Errorf("record %s: %w", a.SHA256[:16], err)
	}
	if inserted {
		result.Inserted++
	} else {
		result.Existing++
	}

	// RecordAsset already writes the delivering device's mapping. This restores
	// the ones from lines that named a different local id for the same content
	// — the second copy of a photo, or a second phone.
	if a.DeviceID != "" && a.LocalID != "" {
		err := d.Store.RecordDeviceMapping(ctx, db.DeviceMapping{
			DeviceID: a.DeviceID, LocalID: a.LocalID, AssetID: id, ModifiedAt: a.ModifiedAt,
		})
		if err != nil {
			return err
		}
		result.Mappings++
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// headOf reads enough of a file for mediatype.Sniff.
func headOf(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	buf := make([]byte, mediatype.SniffLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil
	}
	return buf[:n]
}
