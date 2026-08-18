package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/snapchat"
)

// The devices a Snapchat import uploads as. Stable across runs, because the
// mapping from a file's path to the asset it became is keyed by device and
// local id, and that mapping is what makes a second run over the same export
// cost one request per two hundred files instead of re-sending a hundred
// gigabytes.
//
// One per half, so that the two can be re-run, audited and revoked
// independently — and so that `photobackup devices` says which of them
// delivered what.
const (
	snapchatMemoriesDevice = "snapchat memories import"
	snapchatChatDevice     = "snapchat chat media import"
)

// runImportSnapchat ingests one half of a Snapchat export.
//
// A Snapchat export is not a Takeout wearing a hat, and the difference is not
// cosmetic. A Takeout writes a JSON document per photograph and puts it beside
// the file. Snapchat writes one document for the whole account, gives its rows
// no identifiers, and relates them to the media by nothing at all — the join is
// the modification time on disk, and if that is lost the location and capture
// time of three thousand photographs are lost with it. So this scans the export
// whole before sending a byte: the join, the overlay pairing and the labelling
// are all decided over the complete tree.
func runImportSnapchat(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("import-snapchat", flag.ExitOnError)
	var from rootsFlag
	fs.Var(&from, "from", "a directory the export was unzipped into; repeat it once per zip (required)")
	half := fs.String("half", halfMemories,
		"which half of the export to import: `memories` or chat")
	server := fs.String("server", "", "photod base URL; empty means the local one from LISTEN_ADDR")
	token := fs.String("token", os.Getenv("PHOTOBACKUP_TOKEN"),
		"device token to upload with; empty means mint one for this machine")
	dryRun := fs.Bool("dry-run", false, "report what the export holds and what would be uploaded, and send nothing")
	workers := fs.Int("concurrency", 3, "simultaneous uploads")
	chunkThreshold := fs.Int64("chunk-threshold", 64<<20, "size at which an upload becomes resumable")
	chunkSize := fs.Int64("chunk-size", 8<<20, "bytes per chunk")
	insecure := fs.Bool("insecure", false, "skip TLS verification; for a server whose CA is not on this machine")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	switch *half {
	case halfMemories, halfChat:
	default:
		fmt.Fprintf(os.Stderr, "--half must be %q or %q, not %q\n", halfMemories, halfChat, *half)
		return exitUsage, nil
	}
	if len(from) == 0 {
		fmt.Fprintln(os.Stderr, "import-snapchat needs --from DIR")
		return exitUsage, nil
	}
	for _, root := range from {
		if _, err := os.Stat(root); err != nil {
			return fail(fmt.Errorf("--from: %w", err))
		}
	}

	// The join is the files' modification times, so a copy that did not
	// preserve them has already destroyed it. Said before the scan rather than
	// after, because the scan cannot tell the difference between an export
	// whose times were flattened and one Snapchat never described.
	if *half == halfMemories {
		fmt.Println("memories are joined to their capture times and locations by file")
		fmt.Println("modification time — make sure the copy being read preserved them")
	}

	cfg := config.FromEnv()
	exif := &exifdata.Reader{Binary: cfg.ExiftoolBin}

	started := time.Now()
	for _, root := range from {
		fmt.Printf("reading %s\n", root)
	}
	export, err := scanSnapchatExport(ctx, exif, from, *half)
	if err != nil {
		return fail(err)
	}
	reportSnapchat(export, time.Since(started))

	if len(export.items) == 0 && len(export.unmatchedHistory) == 0 {
		fmt.Println("\nnothing to import")
		return exitOK, nil
	}
	if *dryRun {
		fmt.Println("\ndry run: nothing was uploaded")
		return exitOK, nil
	}

	device := snapchatMemoriesDevice
	if *half == halfChat {
		device = snapchatChatDevice
	}
	client, err := importClientFor(ctx, cfg, *server, *token, *insecure, *workers, device)
	if err != nil {
		return fail(err)
	}

	if err := runSnapchatUploads(ctx, client, export, *workers, *chunkThreshold, *chunkSize); err != nil {
		return fail(err)
	}
	return exitOK, nil
}

// reportSnapchat says what the export holds before anything is sent.
//
// Longer than the Takeout equivalent on purpose. Every number in a Takeout
// import is a count of files; several of these are statements about how much a
// timestamp can be trusted, and those are the ones somebody would want to see
// before committing the result to an archive.
func reportSnapchat(export snapchatExport, elapsed time.Duration) {
	var bytes int64
	stills, videos := 0, 0
	for _, item := range export.items {
		bytes += item.size
		if item.isVideo {
			videos++
		} else {
			stills++
		}
	}

	fmt.Printf("\n  %d files, %s, in %s\n", len(export.items), byteCount(bytes), round(elapsed))
	fmt.Printf("  %d photos, %d videos\n", stills, videos)

	switch export.half {
	case halfMemories:
		fmt.Printf("  %d memories, %d overlays\n", export.mains, export.overlays)
		fmt.Printf("  %d overlays linked to the memory they were drawn on\n", export.linkedOverlays)
		if n := len(export.orphanOverlays); n > 0 {
			fmt.Printf("  %d overlays whose memory is not in this export — they import on their own: %s\n",
				n, strings.Join(truncateList(export.orphanOverlays, 3), ", "))
		}

		fmt.Printf("\n  %d rows in %s, from %s\n",
			export.historyRows, historyPath, strings.Join(export.historyRoots, ", "))
		fmt.Printf("  %d memories matched a row exactly\n", export.matches[snapchat.MatchExact])
		if n := export.matches[snapchat.MatchByType]; n > 0 {
			fmt.Printf("  %d shared a capture second with another and were separated by media type\n", n)
		}
		if n := export.matches[snapchat.MatchAmbiguous]; n > 0 {
			fmt.Printf("  %d could not be told apart from another memory in the same second;\n", n)
			fmt.Printf("    one row was taken, and the sidecar records the match as ambiguous\n")
			if export.ambiguousRisk == 0 {
				fmt.Printf("    every row they chose between agreed on the location, so none of it\n")
				fmt.Printf("    could have come out differently\n")
			} else {
				fmt.Printf("    %d of them chose between rows that disagreed about the location and\n",
					export.ambiguousRisk)
				fmt.Printf("    may have taken the wrong one; the rest chose between rows that agreed\n")
			}
		}
		if n := export.matches[snapchat.MatchNone]; n > 0 {
			fmt.Printf("  %d matched no row and fall back to their own modification time,\n", n)
			fmt.Printf("    which leaves them with a capture time and no location\n")
		}
		if n := len(export.unmatchedHistory); n > 0 {
			fmt.Printf("  %d rows describe a memory this export does not contain;\n", n)
			fmt.Printf("    their time and place are kept as orphans, since nothing else holds them\n")
		}

	case halfChat:
		fmt.Printf("  every capture time here is a file modification time: chat media has no\n")
		fmt.Printf("    history document, and no location was exported for any of it\n")
		if export.publishers > 0 {
			fmt.Printf("  %d items are Discover content, proven by a publisher document beside them\n", export.publishers)
			fmt.Printf("    and labelled %s; the rest of the publisher content is unlabelled\n", snapchat.SubtypeDiscover)
		}
		if export.overlays > 0 {
			fmt.Printf("  %d overlays, none of which can be attached: the export gives a chat\n", export.overlays)
			fmt.Printf("    overlay an identifier that matches no snap in it\n")
		}
	}

	if export.duplicatePaths > 0 {
		fmt.Printf("  %d files appear in more than one delivery; the first was kept\n", export.duplicatePaths)
	}
	reportSkipped(export.skipped)
	fmt.Printf("\n  finding these again: %s\n", snapchatSubtypeHelp())
}

// reportSkipped says what the export held that the archive is not storing,
// grouped by why.
//
// Grouped rather than listed because the reasons carry the meaning: a run that
// says "33 files skipped" invites nobody to ask which, and one that says
// "18 voice notes, because this archive has no kind for audio" is a decision
// waiting to be made.
func reportSkipped(skipped []skippedFile) {
	if len(skipped) == 0 {
		return
	}

	byReason := map[string][]skippedFile{}
	var order []string
	for _, file := range skipped {
		if _, seen := byReason[file.reason]; !seen {
			order = append(order, file.reason)
		}
		byReason[file.reason] = append(byReason[file.reason], file)
	}
	sort.Strings(order)

	fmt.Printf("\n  %d files are in the export and are not being imported:\n", len(skipped))
	for _, reason := range order {
		files := byReason[reason]
		var bytes int64
		names := make([]string, 0, len(files))
		for _, file := range files {
			bytes += file.size
			names = append(names, path.Base(file.rel))
		}
		fmt.Printf("    %d %s (%s) — %s\n", len(files), reason, byteCount(bytes),
			strings.Join(truncateList(names, 2), ", "))
	}
	if files := byReason[skipAudio]; len(files) > 0 {
		fmt.Printf("    the audio is voice notes; this archive's media_kind is image or video,\n")
		fmt.Printf("    so holding them is a schema change rather than a flag\n")
	}
	fmt.Printf("    all of them are recorded as import orphans, so the list survives the export\n")
}

// runSnapchatUploads sends the export, in the one order that works.
//
// Overlays before memories, which is the only ordering constraint here and is
// not the Takeout importer's. A memory is described with the content hash of
// its overlay, and the server refuses a hash for a blob it does not hold — so
// the layer has to be archived before the photograph it belongs to can be told
// about it.
func runSnapchatUploads(ctx context.Context, client *importClient, export snapchatExport, workers int, chunkThreshold, chunkSize int64) error {
	fmt.Printf("\nuploading as device %s\n", client.device)

	// Round one, no digests: an export the archive already holds costs one
	// request per two hundred items and reads none of them.
	if err := checkAll(ctx, client, export.items, false); err != nil {
		return err
	}

	unknown := withStatus(export.items, "unknown")
	if len(unknown) > 0 {
		fmt.Printf("hashing %d files the archive could not answer for\n", len(unknown))
		var unreadable atomic.Int64
		err := each(ctx, unknown, workers, func(item *importItem) error {
			if err := hashItem(item); err != nil {
				unreadable.Add(1)
				fmt.Fprintf(os.Stderr, "  %s: %v\n", item.localID, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if n := unreadable.Load(); n > 0 {
			fmt.Printf("%d could not be read and are not being imported\n", n)
		}
		if err := checkAll(ctx, client, unknown, true); err != nil {
			return err
		}
	}

	wanted := withStatus(export.items, "want")
	have := withStatus(export.items, "have")
	fmt.Printf("%d already archived, %d to upload\n\n", len(have), len(wanted))

	var uploaded, failed atomic.Int64
	send := func(item *importItem) error {
		result, err := uploadOne(client, item, chunkThreshold, chunkSize)
		if err != nil {
			failed.Add(1)
			fmt.Fprintf(os.Stderr, "  %s: %v\n", item.localID, err)
			return nil // one bad file does not end an import of three thousand
		}
		item.assetID, item.sha256 = result.ID, result.SHA256
		uploaded.Add(1)
		return nil
	}

	started := time.Now()
	// Three passes. Overlays first for the reason above; then stills before
	// videos, which is the Takeout importer's rule and is kept because a
	// Snapchat export can contain iPhone footage that pairs by content
	// identifier like anything else.
	overlays, mains := splitOverlays(wanted)
	for _, phase := range [][]*importItem{overlays, videosLast(mains, false), videosLast(mains, true)} {
		if err := each(ctx, phase, workers, send); err != nil {
			return err
		}
	}

	// The overlay hashes, for every memory that has one — including the ones
	// that were already archived, which have an asset id from sync/check and no
	// hash from anywhere.
	//
	// Computed from the file rather than read from an upload's answer, because
	// re-running an import is how anyone recovers a half-finished one, and on
	// the second run nothing is uploaded and every answer is missing. The hash
	// of the bytes on disk is the same value either way, and it is the only one
	// available in both.
	if err := resolveOverlayHashes(ctx, export.items, workers); err != nil {
		return err
	}

	// Describing is its own pass rather than a step inside send, because the
	// overlay link makes an item's metadata depend on a different item's
	// upload. Doing it here means every byte is in the archive before any claim
	// is made about how the pieces fit together.
	var described, undescribed atomic.Int64
	describable := append(append([]*importItem{}, wanted...), have...)
	if err := each(ctx, describable, workers, func(item *importItem) error {
		if item.assetID == "" || item.sidecar == nil {
			return nil
		}
		if err := client.describe(item.assetID, item); err != nil {
			undescribed.Add(1)
			fmt.Fprintf(os.Stderr, "  %s: metadata not recorded: %v\n", item.localID, err)
			return nil
		}
		described.Add(1)
		return nil
	}); err != nil {
		return err
	}

	recordHistoryOrphans(ctx, client, export)
	recordSkippedFiles(ctx, client, export)

	fmt.Printf("\n%d uploaded, %d described", uploaded.Load(), described.Load())
	if n := undescribed.Load(); n > 0 {
		fmt.Printf(", %d not described", n)
	}
	if n := failed.Load(); n > 0 {
		fmt.Printf(", %d failed", n)
	}
	fmt.Printf(", in %s\n", round(time.Since(started)))
	fmt.Println("derivatives are queued; photod builds them as it goes")

	if failed.Load() > 0 || undescribed.Load() > 0 {
		fmt.Println("\nre-run the same command to retry; what landed is not re-sent")
	}
	return nil
}

// splitOverlays separates the drawn-on layers from the photographs.
func splitOverlays(items []*importItem) (overlays, mains []*importItem) {
	for _, item := range items {
		if isOverlayItem(item) {
			overlays = append(overlays, item)
			continue
		}
		mains = append(mains, item)
	}
	return overlays, mains
}

// resolveOverlayHashes fills in the content hash each memory names its overlay
// by, reading the overlay files.
func resolveOverlayHashes(ctx context.Context, items []*importItem, workers int) error {
	var pending []*importItem
	for _, item := range items {
		if item.overlayItem != nil && item.assetID != "" {
			pending = append(pending, item)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	return each(ctx, pending, workers, func(item *importItem) error {
		sum, err := fileSHA256(item.overlayItem.path)
		if err != nil {
			// The memory is described without the link rather than not at all.
			// Losing the overlay costs the composite; losing the sidecar would
			// cost the only record of when and where the photograph was taken.
			fmt.Fprintf(os.Stderr, "  %s: overlay not linked: %v\n", item.localID, err)
			return nil
		}
		item.overlaySHA256 = sum
		return nil
	})
}

// fileSHA256 is the identity the archive stores a blob under: the SHA-256 of
// the bytes, and nothing wrapped around them. See blobstore.Put.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// recordHistoryOrphans hands the archive every history row whose memory is not
// in the export.
//
// 443 of the real export's 3,237 rows are these: Snapchat listed the memory,
// left the download link empty, and shipped no file. What is left is a UTC
// instant and a pair of coordinates for a photograph that is not here — which
// is a record of where somebody was on a particular evening, and the last copy
// of it, since the export is deleted after the import.
func recordHistoryOrphans(ctx context.Context, client *importClient, export snapchatExport) {
	if len(export.unmatchedHistory) == 0 {
		return
	}

	var kept, failed int
	for _, row := range export.unmatchedHistory {
		if ctx.Err() != nil {
			break
		}
		err := client.orphan(snapchat.Source, orphanSidecar, row.locator, "", row.raw, nil,
			"the memories history describes a memory whose file is not in this export; "+
				"Snapchat exported the row and not the media")
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  %s: orphan not recorded: %v\n", row.locator, err)
			continue
		}
		kept++
	}

	fmt.Printf("%d history rows with no file kept for review", kept)
	if failed > 0 {
		fmt.Printf(", %d could not be recorded", failed)
	}
	fmt.Println()
}

// recordSkippedFiles writes down every file the export held and the archive did
// not take.
//
// The bytes are not archived — that is what being skipped means — so this
// records that they existed, what they were, and why they were refused. It is
// worth the rows for one reason: the export is deleted after an import, and
// "there were 18 voice notes in the chat media and this archive could not hold
// audio" is a sentence nobody can reconstruct afterwards from what did land.
//
// A skip is evidence rather than a decision, which is exactly what
// import_orphans is for. It is idempotent on the locator, so re-running the
// import refreshes these rather than duplicating them — and the day the archive
// learns to hold audio, this list is the work queue.
func recordSkippedFiles(ctx context.Context, client *importClient, export snapchatExport) {
	if len(export.skipped) == 0 {
		return
	}

	var kept, failed int
	for _, file := range export.skipped {
		if ctx.Err() != nil {
			break
		}
		sidecar := mustSidecar(snapchat.Sidecar{
			Export: snapchat.Source,
			Kind:   export.half,
			File:   path.Base(file.rel),
			Path:   file.rel,
		})
		reason := fmt.Sprintf(
			"the export holds this file and the archive did not store it (%s); "+
				"exiftool called it %q, and it is %d bytes",
			file.reason, file.mimeType, file.size)

		if err := client.orphan(snapchat.Source, orphanSidecar, file.rel, "", sidecar, nil, reason); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  %s: skip not recorded: %v\n", file.rel, err)
			continue
		}
		kept++
	}

	fmt.Printf("%d skipped files recorded", kept)
	if failed > 0 {
		fmt.Printf(", %d could not be recorded", failed)
	}
	fmt.Println()
}

// snapchatSubtypeHelp lists what the import labels things with, for the command
// that has to explain how to find them again.
func snapchatSubtypeHelp() string {
	subtypes := []string{
		snapchat.SubtypeMemory, snapchat.SubtypeChat, snapchat.SubtypeOverlay,
		snapchat.SubtypeThumbnail, snapchat.SubtypeDiscover,
	}
	sort.Strings(subtypes)
	return fmt.Sprintf("import_source = %q, subtypes from %s",
		db.SourceSnapchat, strings.Join(subtypes, ", "))
}
