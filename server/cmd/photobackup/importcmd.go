package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/tlsca"
)

// importDeviceName is the device an import uploads as. Stable across runs,
// because the mapping from a file's path to the asset it became is keyed by it,
// and that mapping is what makes a second run over the same export cost one
// request per two hundred files instead of re-reading the whole export.
const importDeviceName = "google-takeout import"

// rootsFlag collects a repeated --from.
//
// One export, several directories: Google splits a large Takeout into numbered
// zips and does not keep an item and its sidecar in the same one. Importing
// them one at a time is what loses the metadata, so the flag takes them all and
// the scan treats them as the single export they are.
type rootsFlag []string

func (r *rootsFlag) String() string { return strings.Join(*r, ", ") }

func (r *rootsFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("an export directory cannot be empty")
	}
	*r = append(*r, value)
	return nil
}

// runImport ingests a Google Photos export.
//
// A Takeout is not the phone's protocol wearing a hat. Nothing in it declares
// which video belongs to which photo, half the files have had their metadata
// rewritten on the way through Google, and the parts Google kept are in JSON
// sidecars beside the files rather than in the files. So this walks the export
// first and uploads second: pairing, sidecar matching, and album membership are
// all decided over the whole tree before a byte is sent.
func runImport(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	var from rootsFlag
	fs.Var(&from, "from", "an export directory to import; repeat it once per zip of a split export (required)")
	server := fs.String("server", "", "photod base URL; empty means the local one from LISTEN_ADDR")
	token := fs.String("token", os.Getenv("PHOTOBACKUP_TOKEN"),
		"device token to upload with; empty means mint one for this machine")
	dryRun := fs.Bool("dry-run", false, "report what the export holds and what would be uploaded, and send nothing")
	includeTrash := fs.Bool("include-trash", false, "also import items the export marks as deleted")
	workers := fs.Int("concurrency", 3, "simultaneous uploads")
	chunkThreshold := fs.Int64("chunk-threshold", 64<<20, "size at which an upload becomes resumable")
	chunkSize := fs.Int64("chunk-size", 8<<20, "bytes per chunk")
	insecure := fs.Bool("insecure", false, "skip TLS verification; for a server whose CA is not on this machine")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}
	if len(from) == 0 {
		fmt.Fprintln(os.Stderr, "import needs --from DIR")
		return exitUsage, nil
	}
	for _, root := range from {
		if _, err := os.Stat(root); err != nil {
			return fail(fmt.Errorf("--from: %w", err))
		}
	}

	cfg := config.FromEnv()
	exif := &exifdata.Reader{Binary: cfg.ExiftoolBin}

	started := time.Now()
	for _, root := range from {
		fmt.Printf("reading %s\n", root)
	}
	export, err := scanExport(ctx, exif, from, *includeTrash)
	if err != nil {
		return fail(err)
	}
	reportExport(export, time.Since(started))

	if len(export.items) == 0 {
		fmt.Println("\nnothing to import")
		return exitOK, nil
	}
	if *dryRun {
		fmt.Println("\ndry run: nothing was uploaded")
		return exitOK, nil
	}

	client, err := importClientFor(ctx, cfg, *server, *token, *insecure, *workers)
	if err != nil {
		return fail(err)
	}

	if err := runImportUploads(ctx, client, export, *workers, *chunkThreshold, *chunkSize); err != nil {
		return fail(err)
	}
	return exitOK, nil
}

// reportExport says what was found before anything is sent, because every
// number here is one somebody would want to check before committing a hundred
// gigabytes to an archive.
func reportExport(export scanResult, elapsed time.Duration) {
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
	fmt.Printf("  %d Live Photo pairs matched by content identifier\n", export.pairs)
	if export.orphanVideos > 0 {
		fmt.Printf("  %d paired videos whose still is not in this export — they import as ordinary\n", export.orphanVideos)
		fmt.Printf("    videos and pair themselves if the still ever arrives\n")
	}
	fmt.Printf("  %d items have a metadata sidecar\n", export.described)
	if len(export.albums) > 0 {
		fmt.Printf("  %d albums: %s\n", len(export.albums), strings.Join(truncateList(export.albums, 6), ", "))
	}
	if export.duplicatePaths > 0 {
		fmt.Printf("  %d files appear at the same path in more than one directory; the first was kept\n",
			export.duplicatePaths)
	}
	if export.skippedTrash > 0 {
		fmt.Printf("  %d skipped as deleted — pass --include-trash to keep them\n", export.skippedTrash)
	}
	if n := len(export.unmatchedSidecars); n > 0 {
		locators := make([]string, 0, n)
		for _, sidecar := range export.unmatchedSidecars {
			locators = append(locators, sidecar.locator)
		}
		fmt.Printf("  %d sidecars matched no file; they are kept whole for review: %s\n",
			n, strings.Join(truncateList(locators, 4), ", "))
	}
}

// runImportUploads is the phone's three rounds: ask, hash what could not be
// answered, ask again, send what was asked for.
func runImportUploads(ctx context.Context, client *importClient, export scanResult, workers int, chunkThreshold, chunkSize int64) error {
	fmt.Printf("\nuploading as device %s\n", client.device)

	// Round one, no digests: an export the archive already holds costs one
	// request per two hundred items and reads none of them.
	if err := checkAll(ctx, client, export.items, false); err != nil {
		return err
	}

	unknown := withStatus(export.items, "unknown")
	if len(unknown) > 0 {
		fmt.Printf("hashing %d files the archive could not answer for\n", len(unknown))
		// A file that cannot be read is reported and left behind rather than
		// ending the run. An export is a pile of files off a zip off a cloud;
		// one of them being unreadable says nothing about the other 50,000.
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

	// Stills before videos, so a paired video's row finds its still already
	// present and resolves in the transaction that inserts it. sortForPairing
	// put them in that order; uploading the two halves in separate passes is
	// what keeps concurrency from undoing it.
	var uploaded, described, failed atomic.Int64
	send := func(item *importItem) error {
		result, err := uploadOne(client, item, chunkThreshold, chunkSize)
		if err != nil {
			failed.Add(1)
			fmt.Fprintf(os.Stderr, "  %s: %v\n", item.localID, err)
			return nil // one bad file does not end an import of fifty thousand
		}
		item.assetID = result.ID
		uploaded.Add(1)
		if err := describeOne(client, item); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: metadata not recorded: %v\n", item.localID, err)
		} else if item.sidecar != nil {
			described.Add(1)
		}
		return nil
	}

	started := time.Now()
	for _, phase := range [][]*importItem{videosLast(wanted, false), videosLast(wanted, true)} {
		if err := each(ctx, phase, workers, send); err != nil {
			return err
		}
	}

	// The ones already archived still need describing: a re-run over an export
	// whose files all landed last time is exactly how a missing sidecar gets
	// applied, and it is how anyone recovers from an interrupted import.
	if err := each(ctx, have, workers, func(item *importItem) error {
		if item.assetID == "" || item.sidecar == nil {
			return nil
		}
		if err := describeOne(client, item); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: metadata not recorded: %v\n", item.localID, err)
			return nil
		}
		described.Add(1)
		return nil
	}); err != nil {
		return err
	}

	recordOrphanSidecars(ctx, client, export)

	fmt.Printf("\n%d uploaded, %d sidecars applied", uploaded.Load(), described.Load())
	if n := failed.Load(); n > 0 {
		fmt.Printf(", %d failed", n)
	}
	fmt.Printf(", in %s\n", round(time.Since(started)))
	fmt.Println("derivatives are queued; photod builds them as it goes")

	if failed.Load() > 0 {
		fmt.Println("\nre-run the same command to retry the failures; what landed is not re-sent")
		return nil
	}
	return nil
}

func uploadOne(client *importClient, item *importItem, chunkThreshold, chunkSize int64) (uploadResult, error) {
	if item.size >= chunkThreshold {
		return client.uploadChunked(item, chunkSize)
	}
	return client.upload(item)
}

// The kinds of orphan this importer records. They are the wire spelling of
// db.OrphanSidecar and db.OrphanAlbum, repeated rather than imported so that
// the client stays a client — it speaks the archive's HTTP protocol and knows
// nothing about its schema, exactly as the phone does.
const (
	orphanSidecar = "sidecar"
	orphanAlbum   = "album"
)

func describeOne(client *importClient, item *importItem) error {
	if item.sidecar == nil && len(item.albums) == 0 {
		return nil
	}
	if item.sidecar == nil {
		// Album membership with no sidecar has nothing to travel in: the
		// endpoint that applies an import reads the source's own JSON, and
		// there is none. Inventing one would put a fabricated document in the
		// archive's verbatim copy, which is the one thing that copy must never
		// contain — so this is recorded as an orphan instead, with the asset it
		// belongs to named, and left for a decision rather than guessed at.
		return client.orphan(orphanAlbum, item.localID, item.assetID,
			nil, item.albums,
			"the item is in an album folder but no sidecar matched it, "+
				"and album membership exists nowhere else in an export")
	}
	return client.describe(item.assetID, item)
}

// recordOrphanSidecars hands the archive every sidecar that matched no file.
//
// After the uploads rather than before, because an orphan is only an orphan
// once the whole export has been read — and because it must not be able to
// delay a single byte of the archive.
func recordOrphanSidecars(ctx context.Context, client *importClient, export scanResult) {
	if len(export.unmatchedSidecars) == 0 {
		return
	}

	var kept, failed int
	for _, sidecar := range export.unmatchedSidecars {
		if ctx.Err() != nil {
			break
		}
		err := client.orphan(orphanSidecar, sidecar.locator, "", sidecar.raw, nil,
			"no media file in this export matched the sidecar; "+
				"it describes a photograph that is somewhere else")
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  %s: orphan not recorded: %v\n", sidecar.locator, err)
			continue
		}
		kept++
	}

	fmt.Printf("%d unmatched sidecars kept for review", kept)
	if failed > 0 {
		fmt.Printf(", %d could not be recorded", failed)
	}
	fmt.Println()
}

// checkAll asks the archive about every item, in batches.
func checkAll(ctx context.Context, client *importClient, items []*importItem, withDigest bool) error {
	const batch = 200

	byLocalID := make(map[string]*importItem, len(items))
	var pending []checkItem
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		results, err := client.check(pending)
		if err != nil {
			return err
		}
		for _, r := range results {
			if item, ok := byLocalID[r.LocalID]; ok {
				item.status = r.Status
				if r.AssetID != "" {
					item.assetID = r.AssetID
				}
			}
		}
		pending = pending[:0]
		return nil
	}

	for _, item := range items {
		if withDigest && item.md5 == "" {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		byLocalID[item.localID] = item

		entry := checkItem{LocalID: item.localID, ModifiedAt: &item.modified}
		if withDigest {
			size := item.size
			entry.MD5, entry.Size = item.md5, &size
		}
		pending = append(pending, entry)
		if len(pending) == batch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func withStatus(items []*importItem, status string) []*importItem {
	var out []*importItem
	for _, item := range items {
		if item.status == status {
			out = append(out, item)
		}
	}
	return out
}

func videosLast(items []*importItem, videos bool) []*importItem {
	var out []*importItem
	for _, item := range items {
		if item.isVideo == videos {
			out = append(out, item)
		}
	}
	return out
}

// each runs fn over every item with a fixed number of workers, stopping early
// for a cancelled context or the first error fn chose to raise.
//
// A worker that has hit an error keeps draining the queue instead of returning
// from it. That is not tidiness: with an unbuffered channel, workers that walk
// away leave the loop below blocked on a send that nobody will ever receive, and
// the import hangs rather than reporting the error it already has.
func each(ctx context.Context, items []*importItem, workers int, fn func(*importItem) error) error {
	if workers <= 0 {
		workers = 1
	}
	queue := make(chan *importItem)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	var failed atomic.Bool

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range queue {
				if failed.Load() || ctx.Err() != nil {
					continue
				}
				if err := fn(item); err != nil {
					once.Do(func() { firstErr = err })
					failed.Store(true)
				}
			}
		}()
	}

	for _, item := range items {
		if ctx.Err() != nil || failed.Load() {
			break
		}
		queue <- item
	}
	close(queue)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// importClientFor builds a client against the local photod, minting itself a
// device token if it was not given one.
//
// Everything here defaults to the archive this command is already configured
// for: the address photod listens on, the CA photod issued itself, and a device
// row this command creates. Importing an export that is sitting on the archive
// machine should not require pairing ceremony against localhost.
func importClientFor(ctx context.Context, cfg config.Config, server, token string, insecure bool, workers int) (*importClient, error) {
	base := server
	if base == "" {
		base = localServerURL(cfg)
	}
	base = strings.TrimRight(base, "/")

	tlsConfig, err := localTLS(cfg, base, insecure)
	if err != nil {
		return nil, err
	}

	client := &importClient{
		base:  base,
		token: token,
		http: &http.Client{
			Timeout: 0, // a 4GB video on a slow disk is not a stalled request
			Transport: &http.Transport{
				MaxIdleConnsPerHost: workers + 2,
				IdleConnTimeout:     90 * time.Second,
				TLSClientConfig:     tlsConfig,
			},
		},
	}

	if token != "" {
		// A token given on the command line names its own device, and the
		// server tells us which when the first request goes out. Leaving the
		// device id empty is what the phone does.
		return client, nil
	}

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := store.Migrate(); err != nil {
		store.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	device, minted, err := devices.New(store.Pool()).Provision(ctx, importDeviceName, "import")
	store.Close()
	if err != nil {
		return nil, err
	}
	client.token, client.device = minted, device.ID
	return client, nil
}

// localServerURL is where photod is listening on this machine.
func localServerURL(cfg config.Config) string {
	scheme := "https"
	if cfg.TLSDisabled {
		scheme = "http"
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil || port == "" {
		port = "8787"
	}
	return fmt.Sprintf("%s://127.0.0.1:%s", scheme, port)
}

// localTLS trusts the CA photod issued itself, which is on this disk.
func localTLS(cfg config.Config, base string, insecure bool) (*tls.Config, error) {
	if insecure {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	if strings.HasPrefix(base, "http://") {
		return nil, nil
	}

	root, err := filepath.Abs(cfg.PhotosRoot)
	if err != nil {
		return nil, err
	}
	tlsDir := cfg.TLSDir
	if tlsDir == "" {
		tlsDir = filepath.Join(root, "tls")
	}

	certs, err := tlsca.Open(tlsDir, cfg.TLSExtraSANs, slog.New(slog.DiscardHandler))
	if err != nil {
		return nil, fmt.Errorf("read the archive's CA from %s: %w (pass --insecure to skip verification)", tlsDir, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certs.CACertPEM()) {
		return nil, errors.New("the archive's CA certificate could not be parsed")
	}
	return &tls.Config{RootCAs: pool}, nil
}

func truncateList(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return append(values[:n:n], fmt.Sprintf("and %d more", len(values)-n))
}
