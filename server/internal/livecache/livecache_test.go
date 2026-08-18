package livecache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSecondCallIsServedWithoutBuilding(t *testing.T) {
	c := New(1 << 20)
	var builds atomic.Int32

	build := func(context.Context) ([]byte, error) {
		builds.Add(1)
		return []byte("three seconds"), nil
	}
	for range 3 {
		if _, err := c.Get(context.Background(), "sha", build); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}

	if got := builds.Load(); got != 1 {
		t.Errorf("built %d times, want 1", got)
	}
}

// The case this package exists for: a video element opening two requests for
// the same clip a millisecond apart must not run two ffmpegs.
func TestConcurrentCallersShareOneBuild(t *testing.T) {
	c := New(1 << 20)
	var builds atomic.Int32
	release := make(chan struct{})

	build := func(context.Context) ([]byte, error) {
		builds.Add(1)
		<-release
		return []byte("clip"), nil
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), "sha", build); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	close(release)
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Errorf("built %d times, want 1 shared build", got)
	}
}

func TestAFailedBuildIsNotKept(t *testing.T) {
	c := New(1 << 20)
	var builds atomic.Int32

	build := func(context.Context) ([]byte, error) {
		builds.Add(1)
		return nil, errors.New("ffmpeg fell over")
	}
	for range 2 {
		if _, err := c.Get(context.Background(), "sha", build); err == nil {
			t.Fatal("Get returned no error for a failing build")
		}
	}

	if got := builds.Load(); got != 2 {
		t.Errorf("built %d times, want 2 — a failure must be retried, not cached", got)
	}
}

func TestOldestEntriesAreEvictedAtTheLimit(t *testing.T) {
	const each = 100
	c := New(each * 3)

	for i := range 4 {
		key := fmt.Sprintf("sha-%d", i)
		if _, err := c.Get(context.Background(), key, func(context.Context) ([]byte, error) {
			return make([]byte, each), nil
		}); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}

	c.mu.Lock()
	held, used := len(c.entries), c.used
	c.mu.Unlock()

	if held != 3 {
		t.Errorf("holding %d entries, want 3", held)
	}
	if used > c.max {
		t.Errorf("holding %d bytes, over the %d limit", used, c.max)
	}
	if _, ok := c.entries["sha-0"]; ok {
		t.Error("the oldest entry survived eviction")
	}
}

// Bytes too big to hold are still returned. Nothing is kept, rather than
// everything else being evicted to make room for one clip.
func TestAnOversizedRenditionIsReturnedButNotKept(t *testing.T) {
	c := New(10)

	data, err := c.Get(context.Background(), "sha", func(context.Context) ([]byte, error) {
		return make([]byte, 100), nil
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(data) != 100 {
		t.Fatalf("got %d bytes, want 100", len(data))
	}

	c.mu.Lock()
	held := len(c.entries)
	c.mu.Unlock()
	if held != 0 {
		t.Errorf("holding %d entries, want none", held)
	}
}

// A client that walks away mid-render must not cancel the work everyone else is
// blocked on, and must not leave a half-built entry behind for the next caller.
func TestACancelledCallerDoesNotCancelTheBuild(t *testing.T) {
	c := New(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	finish := make(chan struct{})

	go func() {
		_, _ = c.Get(ctx, "sha", func(buildCtx context.Context) ([]byte, error) {
			close(started)
			<-finish
			if buildCtx.Err() != nil {
				return nil, buildCtx.Err()
			}
			return []byte("clip"), nil
		})
	}()

	<-started
	cancel()
	close(finish)

	data, err := c.Get(context.Background(), "sha", func(context.Context) ([]byte, error) {
		t.Error("the abandoned build was thrown away and started over")
		return []byte("rebuilt"), nil
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != "clip" {
		t.Errorf("got %q, want the build that was already running", data)
	}
}
