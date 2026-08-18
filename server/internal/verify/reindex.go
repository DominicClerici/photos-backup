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
	// Described counts import sidecars replayed onto assets.
	Described int64
	// Orphans counts what an import could not attach, restored to the table it
	// was recorded in rather than onto an asset — see db.ImportOrphan.
	Orphans int64
	// Retracted counts assets a purge line took back out. See KindPurge: the
	// manifest records deliberate destruction as well as arrival, and a rebuild
	// that ignored those lines would resurrect everything ever deleted.
	Retracted int64
	Elapsed   time.Duration
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
	var pending []manifest.Entry
	// Digests a purge line has taken back, and the line that took them. Held
	// rather than acted on, because the log is replayed in the order it was
	// written and a purge line is always after the asset line it retracts —
	// and, for content that was purged and later archived again, before the
	// asset line that reinstates it. The last line about a digest wins, which
	// is what this map ends up holding.
	retracted := make(map[string]manifest.Entry)
	path := filepath.Join(d.PhotosRoot, "manifest.jsonl")

	err := manifest.Scan(path, func(e manifest.Entry) error {
		result.Lines++
		if opt.Progress != nil {
			opt.Progress(result.Lines)
		}
		if e.Type == manifest.KindImportOrphan {
			// Held back with the metadata lines, and for a stronger reason. An
			// album orphan names an asset that may be further down this log,
			// and a sidecar orphan names no blob at all — so it never passes
			// the digest check below, and replaying it here rather than there
			// is the only way a rebuilt database gets it back. The export it
			// was read from is gone by then.
			pending = append(pending, e)
			return nil
		}
		if e.SHA256 == "" {
			return nil
		}
		if e.Type == manifest.KindPurge {
			retracted[e.SHA256] = e
			// Marked as accounted for, so that a blob whose unlink failed —
			// or one restored from a backup taken before the purge — is not
			// adopted straight back into the library by the orphan pass.
			seen[e.SHA256] = struct{}{}
			return nil
		}
		if !e.IsAsset() {
			// A line describing an asset rather than recording one. Held back
			// and applied at the end, because the asset it names may be
			// several thousand lines further down this same log — the import
			// uploads a file and describes it, but a rebuild replays both in
			// the order they were written, not the order they resolve in.
			pending = append(pending, e)
			return nil
		}
		seen[e.SHA256] = struct{}{}
		// This content is in the archive again after having been thrown away,
		// which is the one thing that un-retracts a digest.
		delete(retracted, e.SHA256)

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

	if !opt.DryRun {
		if err := applyPurges(ctx, d, retracted, &result); err != nil {
			return result, err
		}
		if err := replayImports(ctx, d, pending, &result); err != nil {
			return result, err
		}
	} else {
		result.Retracted = int64(len(retracted))
		for _, e := range pending {
			if e.Type == manifest.KindImportOrphan {
				result.Orphans++
				continue
			}
			result.Described++
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

		LiveParentLocalID: e.LiveParentLocalID,
		// Without this a rebuild would restore every imported Live Photo as two
		// separate items and only get them back together once the metadata
		// worker had re-read all of them — which is the entire archive through
		// exiftool to recover 36 bytes a line already holds.
		ContentID: e.ContentID,
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

// replayImports re-applies the sidecars an import recorded, which is how a
// rebuilt database gets back its captions, albums, people, and the capture
// times of every file whose own metadata was stripped before it ever reached
// Google.
//
// The sidecars are re-parsed rather than replayed as stored conclusions, so a
// rebuild understands an export exactly as well as the current build does —
// which is the point of keeping them raw. A line naming a blob that is not in
// the archive is skipped rather than failing the rebuild: the asset lines are
// the ones that must be complete, and a description of something that is not
// there describes nothing.
func replayImports(ctx context.Context, d Deps, entries []manifest.Entry, result *ReindexResult) error {
	for _, e := range entries {
		if e.Type == manifest.KindImportOrphan {
			if err := replayOrphan(ctx, d, e, result); err != nil {
				return err
			}
			continue
		}

		asset, err := d.Store.AssetBySHA256(ctx, e.SHA256)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("look up %s: %w", e.SHA256[:16], err)
		}

		albums := make([]db.AlbumRef, 0, len(e.ImportAlbums))
		for _, album := range e.ImportAlbums {
			albums = append(albums, db.AlbumRef{Title: album.Title, Description: album.Description})
		}
		meta, err := db.ImportMetadataFrom(e.ImportSource, e.ImportSidecar, albums)
		if err != nil {
			// A format this build cannot read is not a reason to abandon a
			// rebuild. The line stays in the log for a build that can.
			continue
		}
		if err := d.Store.ApplyImportMetadata(ctx, asset.ID, meta); err != nil {
			return fmt.Errorf("apply import metadata for %s: %w", e.SHA256[:16], err)
		}
		result.Described++
	}
	return nil
}

// replayOrphan restores one import orphan from its manifest line.
//
// The asset reference is dropped rather than resolved: the line records where
// the item sat inside an export, and a rebuilt database has no way back from
// that to a row. The evidence — the sidecar, the albums, the reason — is what
// mattered, and it survives whole.
func replayOrphan(ctx context.Context, d Deps, e manifest.Entry, result *ReindexResult) error {
	orphan := db.ImportOrphan{
		Source:  e.ImportSource,
		Kind:    e.OrphanKind,
		Locator: e.Locator,
		Sidecar: e.ImportSidecar,
		Reason:  e.OrphanReason,
	}
	for _, album := range e.ImportAlbums {
		orphan.Albums = append(orphan.Albums,
			db.AlbumRef{Title: album.Title, Description: album.Description})
	}
	if err := orphan.Validate(); err != nil {
		// A line this build cannot read is not a reason to abandon a rebuild,
		// the same rule the import lines above follow. It stays in the log.
		return nil
	}
	if err := d.Store.RecordImportOrphan(ctx, orphan); err != nil {
		return fmt.Errorf("replay import orphan %s: %w", e.Locator, err)
	}
	result.Orphans++
	return nil
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

// applyPurges takes back what the log says was deliberately destroyed, and
// restores the tombstones that keep it from being uploaded again.
//
// Done at the end rather than as the lines arrive because a purge line is a
// statement about a digest, not a position: it may be read long after the asset
// line it retracts, and the rows the replay has been inserting are the ones it
// has to reach.
func applyPurges(ctx context.Context, d Deps, retracted map[string]manifest.Entry, result *ReindexResult) error {
	if len(retracted) == 0 {
		return nil
	}

	shas := make([]string, 0, len(retracted))
	tombstones := make([]db.Purged, 0, len(retracted))
	for sha, e := range retracted {
		shas = append(shas, sha)
		tombstones = append(tombstones, db.Purged{
			SHA256: sha, MD5: e.MD5, ByteSize: e.Size, Filename: e.Filename,
		})
	}

	// The tombstones go in first. A row that is gone and a content key that was
	// never recorded is a photograph the next backup uploads again, and if this
	// is interrupted the harmless half is the one already done.
	if err := d.Store.RecordPurged(ctx, tombstones); err != nil {
		return fmt.Errorf("record purged content: %w", err)
	}
	dropped, err := d.Store.DropBySHA256(ctx, shas)
	if err != nil {
		return fmt.Errorf("drop purged assets: %w", err)
	}
	result.Retracted = int64(dropped)
	return nil
}
