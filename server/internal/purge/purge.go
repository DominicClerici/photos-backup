// Package purge destroys what the trash has finished holding.
//
// It is the only code in the archive that removes an original, and it is
// deliberately its own package rather than a method on the store: the row, the
// blob, the derivatives and the manifest live in four places that otherwise
// never have to know about each other, and the order they are touched in is the
// whole of what makes a half-finished purge recoverable.
//
// That order is rows, then manifest, then files:
//
//   - The rows go first, in one statement that also writes the content
//     tombstone. Those two must not be able to come apart — a row deleted
//     without a tombstone is a photograph the next backup uploads again, which
//     would make "permanently delete" mean "until you open the app".
//   - The manifest line goes next, so a rebuild from the blob tree knows this
//     was a decision rather than a loss.
//   - The bytes go last, because they are the only step that cannot be undone
//     by rerunning the ones before it. A file left behind is a finding
//     `verify` can report; a row deleted after its file would be an asset the
//     archive can no longer produce.
package purge

import (
	"context"
	"log/slog"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

// Deps are the four places one asset lives.
//
// Blobs, Derivatives and Manifest are optional in the same way they are
// optional everywhere else in this server: without them the rows still go and
// the bytes are reported rather than removed, which is a purge that leaves work
// for `verify` instead of a purge that refuses to run.
type Deps struct {
	Store       *db.Store
	Blobs       *blobstore.Store
	Derivatives *derivstore.Store
	Manifest    *manifest.Log
	Log         *slog.Logger
}

// Result is what one purge destroyed. Items counts photographs, Rows counts the
// asset rows behind them — a Live Photo is one of the first and two of the
// second — and Bytes is what came back off the disk.
type Result struct {
	Items int
	Rows  int
	Bytes int64
}

// Selection purges exactly what was named, which must already be in the trash.
func Selection(ctx context.Context, d Deps, sel db.Selection) (Result, error) {
	gone, err := d.Store.Purge(ctx, sel)
	if err != nil {
		return Result{}, err
	}
	return d.destroy(gone), nil
}

// Expired purges whatever has served its retention, up to `limit` rows.
//
// The empty case is the overwhelmingly common one — this runs on a timer and
// the answer is nearly always "nothing is due" — so it costs one index probe
// and returns.
func Expired(ctx context.Context, d Deps, limit int) (Result, error) {
	gone, err := d.Store.PurgeExpired(ctx, limit)
	if err != nil {
		return Result{}, err
	}
	result := d.destroy(gone)

	albums, err := d.Store.PurgeExpiredAlbums(ctx)
	if err != nil {
		return result, err
	}
	if albums > 0 {
		d.logger().Info("purged expired albums", "count", albums)
	}
	return result, nil
}

// destroy carries out the two steps after the rows are gone. It never fails:
// by this point the database has already committed, and the only honest thing
// left to do with a file that will not go away is to say so.
func (d Deps) destroy(gone []db.Purged) Result {
	result := Result{Items: db.PurgedItems(gone), Rows: len(gone)}
	if len(gone) == 0 {
		return result
	}

	d.tombstone(gone)

	for _, p := range gone {
		if d.Blobs != nil {
			bytes, err := d.Blobs.Remove(p.SHA256, p.Ext)
			if err != nil {
				d.logger().Error("could not remove a purged original",
					"error", err, "sha256", p.SHA256, "filename", p.Filename)
			}
			result.Bytes += bytes
		}
		if d.Derivatives != nil {
			_, bytes, err := d.Derivatives.RemoveAll(p.SHA256)
			if err != nil {
				d.logger().Warn("could not remove a purged derivative",
					"error", err, "sha256", p.SHA256)
			}
			result.Bytes += bytes
		}
	}
	return result
}

// tombstone records the purge in the manifest, which is the copy that survives
// losing the database.
func (d Deps) tombstone(gone []db.Purged) {
	if d.Manifest == nil {
		return
	}
	for _, p := range gone {
		err := d.Manifest.Append(manifest.Entry{
			Type:     manifest.KindPurge,
			SHA256:   p.SHA256,
			MD5:      p.MD5,
			Size:     p.ByteSize,
			Filename: p.Filename,
			Ext:      p.Ext,
		})
		if err != nil {
			// Loud, and not fatal. The database already holds the tombstone
			// that keeps this content from being uploaded again; what is at
			// risk is only the rebuild path, and only if the database is lost
			// as well.
			d.logger().Error("could not record a purge in the manifest",
				"error", err, "sha256", p.SHA256)
		}
	}
}

func (d Deps) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}
