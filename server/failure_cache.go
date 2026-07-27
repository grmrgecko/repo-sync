package server

import (
	"sync"
	"time"
)

// failureEntry is one remembered upstream failure with its expiry.
type failureEntry struct {
	status  int
	expires time.Time
}

// failureCache remembers recent upstream failures for generic paths so a
// missing or erroring file does not trigger an upstream request per client.
// It is memory-only; nothing here reaches the state file.
type failureCache struct {
	mu      sync.Mutex
	entries map[string]failureEntry
}

// fetchFailures is the server's shared failure cache.
var fetchFailures = &failureCache{entries: map[string]failureEntry{}}

// Hit returns the remembered status for a key, pruning expired entries.
func (c *failureCache) Hit(key string, now time.Time) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return 0, false
	}
	if !now.Before(e.expires) {
		delete(c.entries, key)
		return 0, false
	}
	return e.status, true
}

// Add remembers a failure until now+ttl.
func (c *failureCache) Add(key string, status int, now time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = failureEntry{status: status, expires: now.Add(ttl)}
}

// Sweep drops expired entries that nobody re-requested.
func (c *failureCache) Sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, key)
		}
	}
}
