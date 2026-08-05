package read

import (
	"sync"
	"time"
)

// cache holds recent results so repeated identical requests do not each drive a
// browser at X.
type cache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]entry
}

type entry struct {
	result  Result
	expires time.Time
}

func newCache(ttl time.Duration) *cache {
	return &cache{ttl: ttl, entries: make(map[string]entry)}
}

func (c *cache) get(key string) (Result, bool) {
	if c.ttl <= 0 {
		return Result{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		return Result{}, false
	}
	if time.Now().After(e.expires) {
		// Drop on read rather than sweeping: entries are few and only the ones
		// actually asked for are worth the work.
		delete(c.entries, key)
		return Result{}, false
	}
	return e.result, true
}

func (c *cache) put(key string, result Result) {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry{result: result, expires: time.Now().Add(c.ttl)}
}

// Invalidate drops every cached result. Writes call this, since posting or
// liking changes what a subsequent read should return.
func (c *cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
}
