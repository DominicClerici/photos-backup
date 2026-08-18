// Package livecache holds recently rendered Live Photo previews in memory.
//
// It exists because of one asymmetry between a photo's preview and a video's.
// A photo's 2048px preview is rendered per request and cached by nothing but
// the browser, and that is enough: the browser asks for it once. A `<video>`
// does not behave that way. Safari opens with a one-byte range probe before
// requesting the file properly, and any player may re-request a range it has
// already discarded — so the same three seconds would go through ffmpeg two or
// three times for a single press-and-hold.
//
// The alternative was storing the rendition on disk, which is what the archive
// deliberately does not do for anything it can rebuild from a blob. A bounded
// map of the last few clips costs a few tens of megabytes, survives exactly as
// long as it is useful, and disappears with the process.
package livecache

import (
	"context"
	"sync"
)

// DefaultMaxBytes is roughly twenty Live Photo renditions. They are three
// seconds each, so this is a small number that still covers arrowing back and
// forth through a burst.
const DefaultMaxBytes int64 = 64 << 20

// Build renders the bytes for a key. It is called at most once per key while a
// result for that key is live.
type Build func(ctx context.Context) ([]byte, error)

type entry struct {
	ready chan struct{}
	data  []byte
	err   error
}

type Cache struct {
	max int64

	mu      sync.Mutex
	used    int64
	entries map[string]*entry
	// order is least-recently-used first. A handful of entries makes the linear
	// scans cheaper than the bookkeeping a linked list would need.
	order []string
}

func New(maxBytes int64) *Cache {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Cache{max: maxBytes, entries: make(map[string]*entry)}
}

// Get returns the bytes for key, rendering them if they are not held.
//
// Concurrent callers for the same key share one render rather than starting
// their own — which is the whole point when the caller is a browser that just
// opened two requests for the same video a millisecond apart.
//
// A build that fails is not cached, and its error goes to every caller waiting
// on it. The next request tries again.
func (c *Cache) Get(ctx context.Context, key string, build Build) ([]byte, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		c.touch(key)
		c.mu.Unlock()
		select {
		case <-e.ready:
			return e.data, e.err
		case <-ctx.Done():
			// The render carries on for whoever else is waiting.
			return nil, ctx.Err()
		}
	}

	e := &entry{ready: make(chan struct{})}
	c.entries[key] = e
	c.order = append(c.order, key)
	c.mu.Unlock()

	// Deliberately not this request's context. A client that walks away
	// mid-render would otherwise cancel the work every other waiter is blocked
	// on, and the next request would start it over from nothing.
	e.data, e.err = build(context.WithoutCancel(ctx))
	close(e.ready)

	c.mu.Lock()
	switch {
	case e.err != nil, int64(len(e.data)) > c.max:
		// Too big to hold is not an error to the caller — they get their bytes.
		// It just means nothing is kept, rather than everything else being
		// evicted to make room for one clip.
		c.drop(key)
	default:
		c.used += int64(len(e.data))
		c.evict()
	}
	c.mu.Unlock()

	return e.data, e.err
}

// touch moves a key to the most-recently-used end. Called with the lock held.
func (c *Cache) touch(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), key)
			return
		}
	}
}

// drop removes a key and its accounting. Called with the lock held.
func (c *Cache) drop(key string) {
	e, ok := c.entries[key]
	if !ok {
		return
	}
	if e.err == nil {
		c.used -= int64(len(e.data))
	}
	delete(c.entries, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// evict trims back to the size limit, oldest first. Called with the lock held.
//
// An entry still rendering is skipped rather than dropped: it is not holding
// any bytes yet, and removing it would strand the callers waiting on it and let
// a second render of the same clip start behind them.
func (c *Cache) evict() {
	for i := 0; c.used > c.max && i < len(c.order); {
		key := c.order[i]
		e := c.entries[key]
		if e == nil || !isReady(e) {
			i++
			continue
		}
		c.drop(key)
	}
}

func isReady(e *entry) bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}
