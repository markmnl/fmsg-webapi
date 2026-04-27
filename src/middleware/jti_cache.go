package middleware

import (
	"sync"
	"time"
)

// jtiCacheMaxEntries bounds memory usage of the in-process replay cache.
// When exceeded, expired entries are swept first; if still over the limit,
// new entries are dropped (the request is still rejected only on a true
// duplicate, never on overflow).
const jtiCacheMaxEntries = 100_000

// jtiCache tracks JWT IDs that have already been seen, until their
// corresponding token expiry, to prevent replay attacks.
//
// The cache lives in-process; it does not coordinate across multiple API
// instances. For a horizontally-scaled deployment, replace with a shared
// store (e.g. Postgres or Redis).
type jtiCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	stop    chan struct{}
}

// newJTICache returns a cache with a background sweeper running until Close.
func newJTICache() *jtiCache {
	c := &jtiCache{
		entries: make(map[string]time.Time),
		stop:    make(chan struct{}),
	}
	go c.sweepLoop(time.Minute)
	return c
}

// Seen atomically checks whether jti has been recorded with an unexpired
// entry; if not, records it with the given expiry. Returns true if the
// jti was already present (i.e. this is a replay).
//
// Empty jti strings are never considered seen (caller decides policy).
func (c *jtiCache) Seen(jti string, exp time.Time) bool {
	if jti == "" {
		return false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[jti]; ok && existing.After(now) {
		return true
	}
	if len(c.entries) >= jtiCacheMaxEntries {
		c.sweepLocked(now)
		if len(c.entries) >= jtiCacheMaxEntries {
			// Cache full of unexpired entries; refuse to grow but do not
			// falsely flag the token as a replay.
			return false
		}
	}
	c.entries[jti] = exp
	return false
}

// Close stops the background sweeper.
func (c *jtiCache) Close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
}

func (c *jtiCache) sweepLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-t.C:
			c.mu.Lock()
			c.sweepLocked(now)
			c.mu.Unlock()
		}
	}
}

func (c *jtiCache) sweepLocked(now time.Time) {
	for k, exp := range c.entries {
		if !exp.After(now) {
			delete(c.entries, k)
		}
	}
}
