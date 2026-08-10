// Command photobackup is the archive's maintenance tool: audit it, materialize
// a readable copy of it, or rebuild its index from the bytes on disk.
//
// It reads the same environment photod does, so pointing it at an archive is
// exactly as involved as running the server against one.
//
//	photobackup verify [--deep] [--fix]
//	photobackup export --to DIR [--from DATE] [--until DATE] [--copy]
//	photobackup reindex [--adopt-orphans] [--dry-run]
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

const usage = `photobackup — maintenance for the photo archive

  verify [--deep] [--fix]              audit the archive against itself
  export --to DIR [--copy]             materialize a date tree of hardlinks
  reindex [--adopt-orphans]            rebuild the database from manifest.jsonl

Reads PHOTOS_ROOT, DERIVATIVES_ROOT and DATABASE_URL, the same as photod.
Run a subcommand with --help for its own flags.
`

// Exit codes, so a timer or a cron job can tell the three outcomes apart
// without parsing output.
const (
	exitOK = 0
	// exitFindings means the archive is intact but something needs attention.
	exitFindings = 1
	// exitCritical means originals are missing or no longer match their hash.
	exitCritical = 2
	exitUsage    = 64
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		code int
		err  error
	)
	switch os.Args[1] {
	case "verify":
		code, err = runVerify(ctx, os.Args[2:])
	case "export":
		code, err = runExport(ctx, os.Args[2:])
	case "reindex":
		code, err = runReindex(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(exitUsage)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "photobackup: %v\n", err)
		if code == exitOK {
			code = exitCritical
		}
	}
	os.Exit(code)
}

// archive is everything a subcommand needs to reach the archive, opened from
// the same environment photod reads.
type archive struct {
	deps  verify.Deps
	close func()
}

func open(ctx context.Context) (archive, error) {
	cfg := config.FromEnv()

	root, err := filepath.Abs(cfg.PhotosRoot)
	if err != nil {
		return archive{}, err
	}
	if _, err := os.Stat(root); err != nil {
		return archive{}, fmt.Errorf("photos root %s is not readable: %w", root, err)
	}

	derivRoot := cfg.DerivativesRoot
	if derivRoot == "" {
		derivRoot = filepath.Join(root, "derivatives")
	}
	if derivRoot, err = filepath.Abs(derivRoot); err != nil {
		return archive{}, err
	}

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return archive{}, fmt.Errorf("open database: %w", err)
	}
	// Migrating here means a rebuild works against a database that was dropped
	// and recreated empty, which is the situation reindex exists for.
	if err := store.Migrate(); err != nil {
		store.Close()
		return archive{}, fmt.Errorf("migrate: %w", err)
	}

	return archive{
		deps: verify.Deps{
			Store:       store,
			Blobs:       blobstore.New(root),
			Derivatives: derivstore.New(derivRoot),
			Uploads:     uploads.New(filepath.Join(root, "incoming")),
			Queue:       jobs.NewQueue(store.Pool()),
			PhotosRoot:  root,
		},
		close: store.Close,
	}, nil
}

func fail(err error) (int, error) {
	if errors.Is(err, context.Canceled) {
		return exitFindings, errors.New("interrupted")
	}
	return exitCritical, err
}
