// Command loadgen pushes a directory of photos at photod over the same
// protocol the phone speaks, so the archive can be run at real size without a
// real phone.
//
// It is a test client, not a product surface. Everything it does the app also
// does — enumerate, ask what the server already has, hash only what the server
// could not answer for, upload only what was asked for, and resume a large
// video rather than restart it. If the two ever diverge, this one is wrong.
//
//	go run ./cmd/loadgen -root ../PHOTOS_TEST -server http://localhost:8787
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type options struct {
	root       string
	server     string
	deviceID   string
	workers    int
	checkBatch int
	threshold  int64
	chunkSize  int64
	limit      int
	abortAfter int
	verbose    bool
}

func main() {
	log.SetFlags(0)
	var opt options

	flag.StringVar(&opt.root, "root", "../PHOTOS_TEST", "directory of originals to upload")
	flag.StringVar(&opt.server, "server", "http://localhost:8787", "photod base URL")
	flag.StringVar(&opt.deviceID, "device-id", "loadgen", "device id to upload as")
	flag.IntVar(&opt.workers, "concurrency", 3, "simultaneous uploads, matching the app's default")
	flag.IntVar(&opt.checkBatch, "check-batch", 200, "items per sync/check request")
	flag.Int64Var(&opt.threshold, "chunk-threshold", 64<<20, "size at which an upload becomes resumable")
	flag.Int64Var(&opt.chunkSize, "chunk-size", 8<<20, "bytes per chunk")
	flag.IntVar(&opt.limit, "limit", 0, "stop after this many files (0 = all)")
	flag.IntVar(&opt.abortAfter, "abort-chunked-after", 0,
		"abandon every chunked upload after this many chunks, leaving the partial on the server; re-run without it to prove resume")
	flag.BoolVar(&opt.verbose, "v", false, "log every item")
	flag.Parse()

	if err := run(opt); err != nil {
		log.Fatalf("loadgen: %v", err)
	}
}

func run(opt options) error {
	c := &client{
		base: strings.TrimRight(opt.server, "/"),
		http: &http.Client{
			Timeout: 0, // a 550MB upload on a slow link is not a stalled one
			Transport: &http.Transport{
				MaxIdleConnsPerHost: opt.workers + 2,
				// A backfill is thousands of sequential requests to one host;
				// letting connections lapse would mean a handshake per photo.
				IdleConnTimeout: 90 * time.Second,
			},
		},
	}

	started := time.Now()
	items, err := scan(opt.root, opt.limit)
	if err != nil {
		return fmt.Errorf("scan %s: %w", opt.root, err)
	}
	if len(items) == 0 {
		return fmt.Errorf("no media found under %s", opt.root)
	}
	var total int64
	for _, it := range items {
		total += it.size
	}
	fmt.Printf("enumerated %d files, %s, in %s\n\n", len(items), bytesOf(total), round(time.Since(started)))

	st := &stats{}

	// Round one: no digests. The point of the protocol is that a library the
	// server already holds costs one request per 200 items and no hashing.
	phase := time.Now()
	if err := checkAll(c, opt, items, false); err != nil {
		return err
	}
	fmt.Printf("check (no digest)   %-8s %s\n", round(time.Since(phase)), tally(items))

	// Round two: hash whatever the server could not answer for, and ask again.
	phase = time.Now()
	unknown := withStatus(items, "unknown")
	if err := hashAll(unknown, opt.workers, st); err != nil {
		return err
	}
	if len(unknown) > 0 {
		fmt.Printf("hash                %-8s %d files, %s\n",
			round(time.Since(phase)), len(unknown), bytesOf(st.hashedBytes.Load()))
	}

	phase = time.Now()
	if err := checkAll(c, opt, hashed(unknown), true); err != nil {
		return err
	}
	fmt.Printf("check (by content)  %-8s %s\n", round(time.Since(phase)), tally(items))

	// Round three: send only what was asked for.
	phase = time.Now()
	wanted := withStatus(items, "want")
	uploadAll(c, opt, wanted, st)
	fmt.Printf("upload              %-8s %s\n\n", round(time.Since(phase)), tally(items))

	st.report(items, total, time.Since(started))
	if st.failed.Load() > 0 {
		return fmt.Errorf("%d item(s) failed", st.failed.Load())
	}
	return nil
}

// checkAll asks the server about every pending item, in batches.
func checkAll(c *client, opt options, items []*item, withDigest bool) error {
	for start := 0; start < len(items); start += opt.checkBatch {
		batch := items[start:min(start+opt.checkBatch, len(items))]

		payload := make([]checkItem, 0, len(batch))
		for _, it := range batch {
			entry := checkItem{LocalID: it.localID, ModifiedAt: it.modifiedAt}
			if withDigest {
				entry.MD5 = it.md5
				size := it.size
				entry.Size = &size
			}
			payload = append(payload, entry)
		}

		results, err := c.check(opt.deviceID, payload)
		if err != nil {
			return fmt.Errorf("sync/check: %w", err)
		}

		byLocalID := make(map[string]checkResult, len(results))
		for _, r := range results {
			byLocalID[r.LocalID] = r
		}
		for _, it := range batch {
			r, ok := byLocalID[it.localID]
			if !ok {
				it.status = "failed"
				continue
			}
			it.status = r.Status
			it.assetID = r.AssetID
			// A digest was supplied and the server still cannot place it, so
			// the only thing left is to send the bytes.
			if withDigest && it.status == "unknown" {
				it.status = "want"
			}
		}
	}
	return nil
}

func hashAll(items []*item, workers int, st *stats) error {
	if len(items) == 0 {
		return nil
	}

	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(items) {
					return
				}
				it := items[i]
				sum, err := hash(it.path)
				if err != nil {
					it.status = "failed"
					st.fail(it, err)
					continue
				}
				it.md5 = sum
				st.hashedBytes.Add(it.size)
			}
		}()
	}
	wg.Wait()
	return nil
}

// uploadAll runs a worker pool rather than a barrier, so one 550MB video does
// not hold up the photos queued behind it.
func uploadAll(c *client, opt options, items []*item, st *stats) {
	if len(items) == 0 {
		return
	}

	var next atomic.Int64
	var wg sync.WaitGroup
	for range opt.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(items) {
					return
				}
				uploadOne(c, opt, items[i], st)
			}
		}()
	}
	wg.Wait()
}

func uploadOne(c *client, opt options, it *item, st *stats) {
	started := time.Now()

	var (
		out outcome
		err error
	)
	chunked := it.size >= opt.threshold
	if chunked {
		out, err = c.uploadChunked(opt.deviceID, it, opt.chunkSize, opt.abortAfter)
	} else {
		out, err = c.uploadSingle(opt.deviceID, it)
	}

	switch {
	case errors.Is(err, errAborted):
		it.status = "aborted"
		st.aborted.Add(1)
		fmt.Printf("  abandoned %s after %d chunk(s) — re-run to resume\n", it.filename, opt.abortAfter)
		return
	case err != nil:
		it.status = "failed"
		st.fail(it, err)
		return
	}

	it.status = "done"
	it.assetID = out.result.ID
	st.record(out, chunked, time.Since(started))
	if opt.verbose {
		fmt.Printf("  %s %s %s\n", it.filename, bytesOf(it.size), round(time.Since(started)))
	}
}

func withStatus(items []*item, status string) []*item {
	var out []*item
	for _, it := range items {
		if it.status == status {
			out = append(out, it)
		}
	}
	return out
}

func hashed(items []*item) []*item {
	var out []*item
	for _, it := range items {
		if it.md5 != "" {
			out = append(out, it)
		}
	}
	return out
}

func tally(items []*item) string {
	counts := map[string]int{}
	for _, it := range items {
		counts[it.status]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, " ")
}

func round(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func bytesOf(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
