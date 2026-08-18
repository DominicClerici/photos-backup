package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// stats collects what the run is for: how fast the bytes moved, and what did
// not make it.
type stats struct {
	hashedBytes atomic.Int64
	uploaded    atomic.Int64
	bytes       atomic.Int64
	duplicates  atomic.Int64
	chunked     atomic.Int64
	resumed     atomic.Int64
	aborted     atomic.Int64
	failed      atomic.Int64

	mu        sync.Mutex
	latencies []time.Duration
	failures  []failure
}

type failure struct {
	name string
	err  error
}

func (s *stats) record(out outcome, chunked bool, took time.Duration) {
	s.uploaded.Add(1)
	s.bytes.Add(out.sent)
	if out.result.Duplicate {
		s.duplicates.Add(1)
	}
	if chunked {
		s.chunked.Add(1)
	}
	if out.resumed {
		s.resumed.Add(1)
	}

	s.mu.Lock()
	s.latencies = append(s.latencies, took)
	s.mu.Unlock()
}

func (s *stats) fail(it *item, err error) {
	s.failed.Add(1)
	s.mu.Lock()
	// Bounded: a server that is down fails every item, and 3,000 identical
	// error lines tell you nothing the first twenty did not.
	if len(s.failures) < 20 {
		s.failures = append(s.failures, failure{name: it.localID, err: err})
	}
	s.mu.Unlock()
}

func (s *stats) report(items []*item, libraryBytes int64, elapsed time.Duration) {
	uploaded := s.uploaded.Load()
	sent := s.bytes.Load()

	fmt.Printf("%-22s %s\n", "wall clock", round(elapsed))
	fmt.Printf("%-22s %d of %d files\n", "uploaded", uploaded, len(items))
	// "sent", not "size": a resumed upload puts fewer bytes on the wire than
	// the file weighs, and that gap is the feature working.
	fmt.Printf("%-22s %s of %s\n", "bytes sent", bytesOf(sent), bytesOf(libraryBytes))

	if seconds := elapsed.Seconds(); seconds > 0 && sent > 0 {
		fmt.Printf("%-22s %s/s, %.1f files/s\n", "throughput",
			bytesOf(int64(float64(sent)/seconds)), float64(uploaded)/seconds)
	}
	if n := s.duplicates.Load(); n > 0 {
		fmt.Printf("%-22s %d\n", "already archived", n)
	}
	if n := s.chunked.Load(); n > 0 {
		fmt.Printf("%-22s %d (%d resumed from a partial)\n", "chunked uploads", n, s.resumed.Load())
	}
	if n := s.aborted.Load(); n > 0 {
		fmt.Printf("%-22s %d\n", "abandoned on purpose", n)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.latencies) > 0 {
		sort.Slice(s.latencies, func(i, j int) bool { return s.latencies[i] < s.latencies[j] })
		fmt.Printf("%-22s p50 %s  p95 %s  p99 %s  max %s\n", "upload latency",
			round(percentile(s.latencies, 0.50)),
			round(percentile(s.latencies, 0.95)),
			round(percentile(s.latencies, 0.99)),
			round(s.latencies[len(s.latencies)-1]))
	}

	// Second-run proof: the goal in PROJECT.md is that backing up an archived
	// library moves no bytes at all. It is only that if nothing was left
	// outstanding — a run that abandoned or failed items also sends no bytes,
	// and calling that "already held everything" would be a lie.
	if sent == 0 && s.aborted.Load() == 0 && s.failed.Load() == 0 {
		fmt.Printf("\nzero bytes uploaded — the server already held everything\n")
	}

	if failed := s.failed.Load(); failed > 0 {
		fmt.Printf("\n%d failure(s):\n", failed)
		for _, f := range s.failures {
			fmt.Printf("  %s: %v\n", f.name, f.err)
		}
		if int64(len(s.failures)) < failed {
			fmt.Printf("  ... and %d more\n", failed-int64(len(s.failures)))
		}
	}
}

// percentile picks the nearest-rank sample. With a few thousand uploads the
// difference from an interpolated percentile is noise.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
