package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/geocode"
)

// geocodeBatch is how many rows are written per statement. Large enough that
// eleven thousand assets are a couple of dozen round trips rather than eleven
// thousand, small enough that an interrupted run has committed most of its work.
const geocodeBatch = 500

// runGeocode fills in place names for assets that already have coordinates.
//
// The counterpart to the hook in the metadata job, which covers everything that
// arrives from now on. This is for the library that was already here — 11,045
// photographs with a GPS fix and nothing to say where that was — and it is the
// same code doing the same thing, so a photograph geocoded by the backfill and
// one geocoded on arrival cannot disagree.
//
// It reads the whole set into memory rather than paging. Eleven thousand
// coordinate pairs is under a megabyte, and holding them means the tree is
// walked while nothing else is waiting on a cursor.
func runGeocode(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("geocode", flag.ExitOnError)
	all := fs.Bool("all", false, "resolve every asset with coordinates, not only the ones nobody has looked at; for after a new extract")
	dryRun := fs.Bool("dry-run", false, "report what would be written without touching the database")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	cfg := config.FromEnv()
	dir, err := filepath.Abs(cfg.GeoNamesDir)
	if err != nil {
		return fail(err)
	}

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	targets, err := a.deps.Store.PendingGeocode(ctx, *all)
	if err != nil {
		return fail(err)
	}
	if len(targets) == 0 {
		fmt.Println("nothing to geocode: every asset with coordinates already has a place name")
		fmt.Println("(--all resolves them again, which is what a newer extract wants)")
		return exitOK, nil
	}

	started := time.Now()
	index, err := geocode.Load(dir)
	if err != nil {
		if errors.Is(err, geocode.ErrNoData) {
			fmt.Fprintf(os.Stderr, "no GeoNames extract in %s\n\n%s\n\n", dir, geocode.DownloadHint)
			fmt.Fprintf(os.Stderr, "%d assets are waiting on it. GEONAMES_DIR moves the directory.\n", len(targets))
			return exitFindings, nil
		}
		return fail(err)
	}
	fmt.Printf("%d places loaded in %s; resolving %d assets\n\n",
		index.Len(), round(time.Since(started)), len(targets))

	resolved := make([]db.AssetPlace, 0, len(targets))
	elsewhere := 0
	tally := make(map[string]int)
	for _, t := range targets {
		var place db.Place
		if found, ok := index.Nearest(t.Lat, t.Lon); ok {
			place = db.Place{
				City:    found.City,
				Admin1:  found.Admin1,
				Country: found.Country,
				Source:  geocode.Source,
			}
			tally[label(found)]++
		} else {
			// Recorded as looked-at-and-empty rather than skipped, so the next
			// run does not offer it again. See db.ApplyPlaces.
			elsewhere++
		}
		resolved = append(resolved, db.AssetPlace{AssetID: t.AssetID, Place: place})
	}

	if *dryRun {
		report(tally, elsewhere, len(resolved), time.Since(started))
		fmt.Println("\nnothing was written (--dry-run)")
		return exitOK, nil
	}

	var written int64
	for start := 0; start < len(resolved); start += geocodeBatch {
		end := min(start+geocodeBatch, len(resolved))
		n, err := a.deps.Store.ApplyPlaces(ctx, resolved[start:end])
		if err != nil {
			return fail(err)
		}
		written += n
	}

	report(tally, elsewhere, int(written), time.Since(started))
	// The difference is assets hidden between the read and the write, which the
	// vault check in ApplyPlaces refuses — a photograph in the vault does not
	// get a place name written onto its row. Silence would make that look like
	// a lost update.
	if skipped := len(resolved) - int(written); skipped > 0 {
		fmt.Printf("%d were hidden while this ran and were left alone\n", skipped)
	}
	return exitOK, nil
}

func report(tally map[string]int, elsewhere, written int, elapsed time.Duration) {
	type row struct {
		place string
		n     int
	}
	rows := make([]row, 0, len(tally))
	for place, n := range tally {
		rows = append(rows, row{place, n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].place < rows[j].place
	})

	// The top of the list is the check that this worked. A library's most
	// photographed places are places its owner recognises, and a geocoder that
	// has gone wrong says so here rather than three weeks later in a search.
	for i, r := range rows {
		if i == 10 {
			fmt.Printf("  ... and %d more places\n", len(rows)-10)
			break
		}
		fmt.Printf("  %6d  %s\n", r.n, r.place)
	}

	fmt.Printf("\n%d assets given a place in %s\n", written, round(elapsed))
	if elsewhere > 0 {
		fmt.Printf("%d had no inhabited place within %dkm and were recorded as such\n", elsewhere, geocode.MaxDistanceKM)
	}
}

func label(p geocode.Place) string {
	switch {
	case p.Admin1 != "" && p.Country != "":
		return fmt.Sprintf("%s, %s, %s", p.City, p.Admin1, p.Country)
	case p.Country != "":
		return fmt.Sprintf("%s, %s", p.City, p.Country)
	default:
		return p.City
	}
}
