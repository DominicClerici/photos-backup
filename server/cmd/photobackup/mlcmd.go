package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// runML is the command that starts the overnight pass, and the reason it is a
// command.
//
// Every other backfill in this archive is a reconcile: bump a version, restart,
// and the pools find the work. `vision` is one — fifteen minutes of GPU is a
// reasonable consequence of a service restart. Captioning is four hours, and a
// `systemctl restart photod` that quietly begins four hours of GPU work is a
// restart with a surprise in it. That is the same objection migrations 0016 and
// 0017 made to queueing from a migration, pointed at the other thing that runs
// without anybody typing.
//
// So it is typed. And because it is typed, it can be bounded — which is what
// makes a thousand photographs and twenty clips an evening's worth of
// vocabulary to build a search page against, rather than a choice between
// nothing and everything.
func runML(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		fmt.Print(mlUsage)
		return exitUsage, nil
	}
	switch args[0] {
	case "backfill":
		return runMLBackfill(ctx, args[1:])
	case "renditions":
		return runMLRenditions(ctx, args[1:])
	case "status":
		return runMLStatus(ctx, args[1:])
	case "reindex":
		return runMLReindex(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Print(mlUsage)
		return exitOK, nil
	default:
		fmt.Printf("unknown ml subcommand %q\n\n%s", args[0], mlUsage)
		return exitUsage, nil
	}
}

const mlUsage = `photobackup ml — what a photograph is of, in words

  ml status                            how far the passes have got
  ml backfill [--kind ocr|describe]    queue the captioner and the text
    [--stills N] [--videos N]          recogniser over what has none;
    [--force]                          --force redoes what already has words
  ml renditions                        re-render every ML rendition, after a
                                       change to derivstore.MLEdge. Renders
                                       only: queues no pass that reads them
  ml reindex                           rebuild the full-text index from the
                                       captions, tags, filenames and places

Needs ML_URL set on the running photod, and photo-ml up. Nothing here does any
work itself: it queues, and the vision pool drains — text recognition first,
then captions, because the first is twenty minutes and the second is four hours.
Watch it with 'ml status' or the status page.
`

// runMLRenditions re-renders what the models read.
//
// The one backfill here that is not about a model at all: it decodes originals
// and writes WebPs on the CPU, in the prep pool, with photo-ml not in the
// picture. It is typed for the reason ReconcileMLPrep explains — the evidence
// this job ran is a file on disk rather than a row, so a changed MLEdge is
// invisible to a restart and has to be asked for.
//
// It queues only the rendering, and migration 0021 is what makes that true
// rather than nearly true: the mlprep job normally queues the three passes that
// read its output when it finishes, which is right for a photograph arriving
// from a phone and wrong for seventeen thousand of them arriving at the rate a
// CPU can render. Re-captioning the library is four hours of GPU and should
// never be the side effect of asking for an hour of CPU. The last lines say
// which command asks for each of them.
func runMLRenditions(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ml renditions", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "say how many would be queued without queueing them")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	if *dryRun {
		fmt.Printf("would re-render every ML rendition at %dpx on its longest edge\n", derivstore.MLEdge)
		fmt.Println("\nnothing was queued (--dry-run)")
		return exitOK, nil
	}

	n, err := jobs.RequeueMLPrep(ctx, a.deps.Store.Pool())
	if err != nil {
		return fail(err)
	}
	fmt.Printf("mlprep   queued %d assets at %dpx\n", n, derivstore.MLEdge)

	fmt.Println()
	fmt.Println("The prep pool drains these on the CPU — about an hour over this library —")
	fmt.Println("and needs no GPU and no photo-ml at all. Nothing that reads the renditions is")
	fmt.Println("queued by this: not the encoder, not the recogniser, not the captioner, and")
	fmt.Println("not for the assets that have never had any of the three. Ask for those by")
	fmt.Println("name, once this has finished:")
	fmt.Println()
	fmt.Println("  photobackup ml backfill --kind ocr --force     read the text again, at the new size")
	fmt.Println("  photobackup ml backfill --kind describe --force   and then the captions")
	fmt.Println("  photobackup ml status                         how far it has got")
	return exitOK, nil
}

func runMLBackfill(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ml backfill", flag.ExitOnError)
	kind := fs.String("kind", "all", "which pass to queue: ocr, describe, or all")
	stills := fs.Int("stills", jobs.Unbounded, "how many photographs, newest first; -1 for every one")
	videos := fs.Int("videos", jobs.Unbounded, "how many videos, newest first; -1 for every one")
	force := fs.Bool("force", false, "redo assets that already have words, not just the ones that have none")
	dryRun := fs.Bool("dry-run", false, "say what would be queued without queueing it")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	kinds, err := passesFor(*kind)
	if err != nil {
		return exitUsage, err
	}

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	before, err := a.deps.Store.DescribeCoverage(ctx)
	if err != nil {
		return fail(err)
	}
	if before.Eligible == 0 {
		fmt.Println("nothing to describe: no asset has ML renditions yet")
		fmt.Println("(that is the mlprep pass; it runs by itself, and 'photobackup ml status' will show it arriving)")
		return exitFindings, nil
	}

	if *dryRun {
		fmt.Printf("%s over at most %s and %s of the %d assets with renditions%s\n",
			names(kinds), count(*stills, "photographs"), count(*videos, "videos"),
			before.Eligible, forcing(*force))
		if *force {
			fmt.Printf("\nof those, %d already have a caption and %d already have text read;\n",
				before.Described, before.Recognised)
			fmt.Println("--force is what redoes them, and their existing words stand until each is rewritten")
		}
		fmt.Println("\nnothing was queued (--dry-run)")
		return exitOK, nil
	}

	for _, k := range kinds {
		model := db.CaptionModel
		if k == jobs.KindOCR {
			model = db.OCRModel
		}
		n, err := jobs.QueueWords(ctx, a.deps.Store.Pool(), k, model, *stills, *videos, *force)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("%-9s queued %d assets\n", k, n)
	}

	fmt.Println()
	fmt.Println("The vision pool drains these in order — text recognition first, then")
	fmt.Println("captions — and only while photo-ml is answering. Nothing else is affected:")
	fmt.Println("backups still commit, the gallery still draws, and every other pool is idle")
	fmt.Println("of this work entirely.")
	if *force {
		fmt.Println()
		fmt.Println("Nothing was deleted to do this. Each asset's words are replaced in one")
		fmt.Println("transaction as the pass reaches it, so search keeps answering throughout —")
		fmt.Println("out of date for as long as the pass takes, never empty.")
		fmt.Println()
		fmt.Println("'ml status' counts assets that have words, so a forced run barely moves it.")
		fmt.Println("The queue is what to watch: the status page, or the pending count.")
	}
	fmt.Println()
	fmt.Println("  photobackup ml status        how far it has got")
	return exitOK, nil
}

func runMLStatus(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ml status", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	vectors, err := a.deps.Store.EmbeddingCoverage(ctx, db.VisionModel)
	if err != nil {
		return fail(err)
	}
	words, err := a.deps.Store.DescribeCoverage(ctx)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("%d assets have ML renditions and are in scope\n\n", words.Eligible)
	line("embedded", vectors.Embedded, words.Eligible, db.VisionModel)
	fmt.Printf("  %-12s %d frames across those assets\n", "", vectors.Frames)
	line("described", words.Described, words.Eligible, db.CaptionModel)
	line("text read", words.Recognised, words.Eligible, db.OCRModel)

	fmt.Printf("\n%d tag claims over a vocabulary of %d words\n", words.Tags, words.Vocabulary)
	if words.Vocabulary > 0 {
		tags, err := a.deps.Store.TagCleanupCounts(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("  %-12s %6d kept, %d struck out, %d merged away\n",
			"cleanup", tags.Kept, tags.Junk, tags.Folded)
		switch {
		case tags.Untriaged > 0:
			fmt.Printf("  %-12s %6d words nothing has judged yet\n", "", tags.Untriaged)
		case tags.Unreviewed > 0:
			fmt.Printf("  %-12s %6d verdicts nobody has read yet\n", "", tags.Unreviewed)
		case tags.Unembedded > 0:
			fmt.Printf("  %-12s %6d words with no vector to cluster by\n", "", tags.Unembedded)
		}
		// The whole of it is a screen rather than a command, because every step
		// is a judgement about a word and the evidence is a photograph. This is
		// the reading; /tags is the doing.
		fmt.Println("  (the vocabulary is free-form on purpose; the cleanup is the Tags card on the status page)")
	}

	counts, err := a.deps.Queue.Counts(ctx)
	if err != nil {
		return fail(err)
	}
	var queued bool
	for _, c := range counts {
		switch c.Kind {
		case jobs.KindVision, jobs.KindOCR, jobs.KindDescribe:
			if c.Count == 0 {
				continue
			}
			if !queued {
				fmt.Println("\nqueue:")
				queued = true
			}
			fmt.Printf("  %-9s %-8s %d\n", c.Kind, c.State, c.Count)
		}
	}
	if !queued {
		fmt.Println("\nnothing queued. 'photobackup ml backfill' starts a pass.")
	}
	return exitOK, nil
}

// runMLReindex rebuilds the tsvector for the whole library.
//
// Needed after two things this tool cannot see: a tag merge, which changes what
// every asset carrying the merged word should be findable by, and a re-geocode,
// which changes the place name in the weight-C half of the row. Both are one
// column somewhere else and neither knows what a tsvector is.
//
// Cheap — seconds over 23,000 rows — so it is offered rather than automated.
func runMLReindex(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ml reindex", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	started := time.Now()
	n, err := a.deps.Store.RefreshSearch(ctx)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("rebuilt the full-text index over %d assets in %s\n", n, round(time.Since(started)))
	return exitOK, nil
}

func passesFor(kind string) ([]jobs.Kind, error) {
	switch kind {
	case "all":
		// Text recognition first, and the order here is only documentation —
		// the pool's own claim order is what actually decides. See
		// jobs.ClaimInOrder.
		return []jobs.Kind{jobs.KindOCR, jobs.KindDescribe}, nil
	case "ocr":
		return []jobs.Kind{jobs.KindOCR}, nil
	case "describe":
		return []jobs.Kind{jobs.KindDescribe}, nil
	}
	return nil, fmt.Errorf("--kind must be ocr, describe or all, not %q", kind)
}

func names(kinds []jobs.Kind) string {
	out := ""
	for i, k := range kinds {
		if i > 0 {
			out += " and "
		}
		out += string(k)
	}
	return out
}

func count(n int, what string) string {
	if n < 0 {
		return "every one of the " + what
	}
	return fmt.Sprintf("%d %s", n, what)
}

// forcing says which of the two things --force changes, in the one clause the
// dry run has room for.
func forcing(force bool) string {
	if force {
		return ", including the ones that already have words"
	}
	return ", skipping the ones that already have words"
}

func line(label string, done, of int64, model string) {
	percent := 0.0
	if of > 0 {
		percent = 100 * float64(done) / float64(of)
	}
	fmt.Printf("  %-12s %6d  %5.1f%%  %s\n", label, done, percent, model)
}
