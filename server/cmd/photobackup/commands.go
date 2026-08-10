package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

func runVerify(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	deep := fs.Bool("deep", false, "re-hash every original; this is the bit-rot check and it reads the whole archive")
	fix := fs.Bool("fix", false, "apply the unambiguous repairs: missing manifest lines, missing derivatives, stale temp files")
	staleAfter := fs.Duration("stale-after", 24*time.Hour, "how old an abandoned partial upload must be to count as litter")
	quiet := fs.Bool("quiet", false, "print only findings, for a cron job that mails its output")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	counts, err := a.deps.Store.Counts(ctx)
	if err != nil {
		return fail(err)
	}
	if !*quiet {
		what := "checking"
		if *deep {
			what = "re-hashing"
		}
		fmt.Printf("%s %d assets, %s\n\n", what, counts.Assets, byteCount(counts.Bytes))
	}

	opt := verify.Options{Deep: *deep, Fix: *fix, StaleAfter: *staleAfter}
	if *deep && !*quiet {
		opt.Progress = progressTicker()
	}

	report, err := verify.Run(ctx, a.deps, opt)
	if err != nil {
		return fail(err)
	}
	clearProgress(*deep && !*quiet)

	for _, f := range report.Findings {
		fmt.Println(f)
	}

	if !*quiet || report.Unresolved() > 0 {
		if len(report.Findings) > 0 {
			fmt.Println()
		}
		fmt.Printf("%d assets, %s checked in %s", report.Checked, byteCount(report.Bytes), round(report.Elapsed))
		if report.Hashed > 0 {
			fmt.Printf(" (%s re-hashed)", byteCount(report.Hashed))
		}
		fmt.Println()
		if report.Fixed > 0 {
			fmt.Printf("%d repaired\n", report.Fixed)
		}
	}

	switch {
	case report.Critical():
		fmt.Fprintln(os.Stderr, "\noriginals are missing or damaged — restore them from a backup; --fix will not touch them")
		return exitCritical, nil
	case report.Unresolved() > 0:
		if !*fix {
			fmt.Println("\nre-run with --fix to repair what can be repaired")
		}
		return exitFindings, nil
	default:
		if !*quiet {
			fmt.Println("\narchive is intact")
		}
		return exitOK, nil
	}
}

func runExport(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dest := fs.String("to", "", "directory to build the date tree in (required)")
	copyBytes := fs.Bool("copy", false, "copy instead of hardlinking; only for a destination on another filesystem, and it doubles the bytes used")
	from := fs.String("from", "", "earliest capture date to include, YYYY-MM-DD")
	until := fs.String("until", "", "latest capture date to include, YYYY-MM-DD")
	dryRun := fs.Bool("dry-run", false, "report what would be written without writing it")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}
	if *dest == "" {
		fmt.Fprintln(os.Stderr, "export needs --to DIR")
		return exitUsage, nil
	}

	opt := verify.ExportOptions{Dest: *dest, Copy: *copyBytes, DryRun: *dryRun}
	var err error
	if opt.From, err = parseDay(*from); err != nil {
		return exitUsage, fmt.Errorf("--from: %w", err)
	}
	if opt.To, err = parseDay(*until); err != nil {
		return exitUsage, fmt.Errorf("--until: %w", err)
	}

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	result, err := verify.Export(ctx, a.deps.Store, a.deps.Blobs, opt)
	if err != nil {
		return fail(err)
	}

	verb := "linked"
	if *copyBytes {
		verb = "copied"
	}
	if *dryRun {
		verb = "would link"
	}
	fmt.Printf("%s %d files (%s) into %s in %s\n",
		verb, result.Linked+result.Copied, byteCount(result.Bytes), result.DestRoot, round(result.Elapsed))
	if !*copyBytes && !*dryRun {
		fmt.Println("hardlinks, so this used no additional disk space")
	}
	if result.Skipped > 0 {
		fmt.Printf("%d already present from an earlier export\n", result.Skipped)
	}
	if result.Renamed > 0 {
		fmt.Printf("%d renamed to avoid a filename collision\n", result.Renamed)
	}
	if result.Missing > 0 {
		fmt.Printf("%d indexed originals were not on disk — run `photobackup verify`\n", result.Missing)
		return exitFindings, nil
	}
	return exitOK, nil
}

func runReindex(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("reindex", flag.ExitOnError)
	adopt := fs.Bool("adopt-orphans", true, "also index blobs with no manifest line, reading their type off the file")
	dryRun := fs.Bool("dry-run", false, "report what would be inserted without touching the database")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	before, err := a.deps.Store.Counts(ctx)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("database holds %d assets; replaying the manifest\n\n", before.Assets)

	result, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{
		AdoptOrphans: *adopt,
		DryRun:       *dryRun,
		Progress:     reindexTicker(),
	})
	if err != nil {
		return fail(err)
	}
	clearProgress(true)

	fmt.Printf("%d manifest lines read in %s\n", result.Lines, round(result.Elapsed))
	if *dryRun {
		fmt.Printf("%d would be inserted, %d blobs would be adopted\n", result.Inserted, result.Adopted)
		return exitOK, nil
	}

	fmt.Printf("%d inserted, %d already indexed, %d device mappings restored\n",
		result.Inserted, result.Existing, result.Mappings)
	if result.Adopted > 0 {
		fmt.Printf("%d blobs adopted with no manifest line — their original filenames are gone\n", result.Adopted)
	}
	if result.Missing > 0 {
		fmt.Printf("%d manifest lines name a blob that is not on disk\n", result.Missing)
	}

	after, err := a.deps.Store.Counts(ctx)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("\ndatabase now holds %d assets, %s\n", after.Assets, byteCount(after.Bytes))
	fmt.Println("derivatives are queued as pending; start photod to rebuild them")

	if result.Missing > 0 {
		return exitFindings, nil
	}
	return exitOK, nil
}

func parseDay(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.DateOnly, s)
}
