package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

// ExportOptions configures a materialized date tree.
type ExportOptions struct {
	// Dest is the directory the tree is built under.
	Dest string
	// Copy duplicates bytes instead of hardlinking. Only needed when Dest is on
	// a different filesystem from the blobs, and it costs a second copy of the
	// entire archive — so it has to be asked for explicitly.
	Copy bool
	// From and To bound the export by capture time. Zero means unbounded.
	From time.Time
	To   time.Time
	// DryRun reports what would be written without writing it.
	DryRun bool
}

// ExportResult is what an export did.
type ExportResult struct {
	Linked   int
	Copied   int
	Skipped  int
	Bytes    int64
	Renamed  int
	Missing  int
	Elapsed  time.Duration
	DestRoot string
}

// Export materializes a human-readable date tree of hardlinks to the blobs.
//
// The blob tree is meaningless without the database — that is the price of
// content-addressed storage. This is how the price is paid back: a directory of
// YYYY/YYYY-MM-DD/original-filename that any file manager, any backup tool, and
// any person can read, costing no extra bytes because every entry is a second
// name for a blob that already exists.
//
// Days are computed from the same sort time and UTC offset the gallery groups
// on, so a photo taken at 23:50 in Vermont files under that day here too rather
// than sliding into tomorrow because the exporting machine is in Berlin.
func Export(ctx context.Context, store *db.Store, blobs blobPather, opt ExportOptions) (ExportResult, error) {
	started := time.Now()
	result := ExportResult{DestRoot: opt.Dest}

	if strings.TrimSpace(opt.Dest) == "" {
		return result, errors.New("export needs a destination directory")
	}
	if !opt.DryRun {
		if err := os.MkdirAll(opt.Dest, 0o755); err != nil {
			return result, fmt.Errorf("create destination: %w", err)
		}
	}

	// Names already used in each day directory, so two photos called IMG_0001
	// on the same day do not silently become one file.
	taken := make(map[string]struct{})

	err := store.EachAsset(ctx, func(a db.Asset) error {
		day := localDay(a)
		if !opt.From.IsZero() && day.Before(opt.From) {
			return nil
		}
		if !opt.To.IsZero() && day.After(opt.To) {
			return nil
		}

		source := blobs.Path(a.SHA256, a.Ext)
		info, err := os.Stat(source)
		if os.IsNotExist(err) {
			// Reported rather than fatal: an export is not the right place to
			// discover a missing original, and `verify` says so far better.
			result.Missing++
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat blob: %w", err)
		}

		dir := filepath.Join(opt.Dest, day.Format("2006"), day.Format("2006-01-02"))
		name, renamed := uniqueName(taken, dir, a)
		if renamed {
			result.Renamed++
		}
		dest := filepath.Join(dir, name)

		if opt.DryRun {
			result.Linked++
			result.Bytes += info.Size()
			return nil
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}

		switch err := link(source, dest, opt.Copy); {
		case errors.Is(err, os.ErrExist):
			// A previous export already placed this exact file. Re-running an
			// export should be free, not an error.
			result.Skipped++
		case err != nil:
			return err
		case opt.Copy:
			result.Copied++
			result.Bytes += info.Size()
		default:
			result.Linked++
			result.Bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return result, err
	}

	result.Elapsed = time.Since(started)
	return result, nil
}

// blobPather is the sliver of the blob store an export needs.
type blobPather interface {
	Path(sha256hex, ext string) string
}

// localDay is the day an asset belongs to in the timezone it was taken in.
func localDay(a db.Asset) time.Time {
	t := a.SortTime
	if a.ExifOffsetMinutes != nil {
		t = t.In(time.FixedZone("capture", *a.ExifOffsetMinutes*60))
	} else {
		t = t.UTC()
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// uniqueName picks a filename for an asset inside a day directory, keeping the
// original name where it can and disambiguating with a short digest where it
// cannot.
//
// Collisions are ordinary, not exceptional: phones reset their counters, and
// two cameras both produce IMG_0001.
func uniqueName(taken map[string]struct{}, dir string, a db.Asset) (string, bool) {
	base := filepath.Base(a.OriginalFilename)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = a.SHA256[:16] + a.Ext
	}
	// The stored extension is authoritative — it is what the bytes actually
	// are, which for a Takeout video is not what the filename said.
	if a.Ext != "" && !strings.EqualFold(filepath.Ext(base), a.Ext) {
		base += a.Ext
	}

	key := filepath.Join(dir, strings.ToLower(base))
	if _, clash := taken[key]; !clash {
		taken[key] = struct{}{}
		return base, false
	}

	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	unique := fmt.Sprintf("%s-%s%s", stem, a.SHA256[:8], ext)
	taken[filepath.Join(dir, strings.ToLower(unique))] = struct{}{}
	return unique, true
}

// link hardlinks source to dest, or copies when asked.
//
// A cross-device hardlink is refused with an explanation rather than silently
// falling back to copying: the whole promise of export is that it costs no
// extra bytes, and quietly doubling a 100GB archive is not a decision to make
// on someone's behalf.
func link(source, dest string, copyInstead bool) error {
	if copyInstead {
		return copyFile(source, dest)
	}

	err := os.Link(source, dest)
	switch {
	case err == nil:
		return nil
	case os.IsExist(err):
		return os.ErrExist
	}

	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && isCrossDevice(linkErr) {
		return fmt.Errorf(
			"%s is on a different filesystem from the blobs, so hardlinks are impossible; "+
				"re-run with --copy to duplicate the bytes instead", filepath.Dir(dest))
	}
	return fmt.Errorf("link %s: %w", dest, err)
}

func copyFile(source, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return os.ErrExist
	}

	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	// O_EXCL so a concurrent export cannot half-write the same file twice.
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return os.ErrExist
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	return out.Close()
}
