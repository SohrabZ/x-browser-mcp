package read

import (
	"sync"
	"time"
)

// cache holds recent results so repeated identical requests do not each drive a
// browser at X.
//
// It is generic over what is cached because the surfaces do not all return posts:
// a notification is not a post and does not reduce to one. One cache per shape
// beats one cache holding a union of them.
type cache[T any] struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]entry[T]
}

type entry[T any] struct {
	result  T
	expires time.Time
}

func newCache[T any](ttl time.Duration) *cache[T] {
	return &cache[T]{ttl: ttl, entries: make(map[string]entry[T])}
}

func (c *cache[T]) get(key string) (T, bool) {
	var zero T
	if c.ttl <= 0 {
		return zero, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if time.Now().After(e.expires) {
		// Drop on read rather than sweeping: entries are few and only the ones
		// actually asked for are worth the work.
		delete(c.entries, key)
		return zero, false
	}
	return e.result, true
}

func (c *cache[T]) put(key string, result T) {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry[T]{result: result, expires: time.Now().Add(c.ttl)}
}

// Invalidate drops every cached result. Writes call this, since posting or
// liking changes what a subsequent read should return.
func (c *cache[T]) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
}
