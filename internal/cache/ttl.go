package cache

import (
	"sync"
	"time"
)

type entry struct {
	value     any
	expiresAt time.Time
	charge    int64
}

const entryChargeBytes int64 = 64

type retainedSizer interface {
	CacheSizeBytes() int64
}

// TTLCache is a simple in-memory cache with per-entry TTL expiry.
// Safe for concurrent use. Background goroutine evicts stale entries every 30s.
// Call Stop() to shut down the background goroutine.
type TTLCache struct {
	mu           sync.RWMutex
	items        map[string]entry
	maxEntries   int
	maxBytes     int64
	chargedBytes int64
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// New creates a new TTLCache and starts the background eviction loop.
func New() *TTLCache {
	return newCache(0, 0)
}

// NewBounded creates a cache limited by entry count and retained bytes.
// Values admitted to a bounded cache must implement CacheSizeBytes. The byte
// charge includes that payload size, the key length, and a fixed per-entry
// charge for cache bookkeeping. It is an accounting bound, not an RSS bound.
func NewBounded(maxEntries int, maxBytes int64) *TTLCache {
	if maxEntries <= 0 || maxBytes <= 0 {
		panic("cache: bounded limits must be positive")
	}
	return newCache(maxEntries, maxBytes)
}

func newCache(maxEntries int, maxBytes int64) *TTLCache {
	c := &TTLCache{
		items:      make(map[string]entry),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		stopCh:     make(chan struct{}),
	}
	c.wg.Add(1)
	go c.evictLoop()
	return c
}

// Stop shuts down the background eviction goroutine and waits for it to exit.
func (c *TTLCache) Stop() {
	close(c.stopCh)
	c.wg.Wait()
}

// Set stores value under key with the given TTL. It returns false when a
// bounded cache cannot account for or admit the value.
func (c *TTLCache) Set(key string, value any, ttl time.Duration) bool {
	var charge int64
	if c.maxEntries > 0 {
		sized, ok := value.(retainedSizer)
		if !ok {
			c.Delete(key)
			return false
		}
		payloadBytes := sized.CacheSizeBytes()
		keyBytes := int64(len(key))
		if payloadBytes < 0 || payloadBytes > c.maxBytes || keyBytes > c.maxBytes-entryChargeBytes || payloadBytes > c.maxBytes-entryChargeBytes-keyBytes {
			c.Delete(key)
			return false
		}
		charge = entryChargeBytes + keyBytes + payloadBytes
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteExpiredLocked(now)
	c.deleteLocked(key)
	if c.maxEntries > 0 && charge > c.maxBytes {
		return false
	}
	for c.maxEntries > 0 && (len(c.items) >= c.maxEntries || c.chargedBytes+charge > c.maxBytes) {
		for victim := range c.items {
			c.deleteLocked(victim)
			break
		}
	}
	c.items[key] = entry{value: value, expiresAt: now.Add(ttl), charge: charge}
	c.chargedBytes += charge
	return true
}

// Get returns the cached value and true if it exists and has not expired.
func (c *TTLCache) Get(key string) (any, bool) {
	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if !time.Now().Before(e.expiresAt) {
		c.mu.Lock()
		if current, exists := c.items[key]; exists && !time.Now().Before(current.expiresAt) {
			c.deleteLocked(key)
		}
		c.mu.Unlock()
		return nil, false
	}
	return e.value, true
}

// Len reports the entries currently held, expired ones included until the
// eviction loop sweeps them.
func (c *TTLCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Usage reports the current entry count and charged retained bytes. Unbounded
// caches report zero charged bytes.
func (c *TTLCache) Usage() (entries int, chargedBytes int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items), c.chargedBytes
}

// Delete removes a key immediately.
func (c *TTLCache) Delete(key string) {
	c.mu.Lock()
	c.deleteLocked(key)
	c.mu.Unlock()
}

func (c *TTLCache) deleteLocked(key string) {
	if e, ok := c.items[key]; ok {
		delete(c.items, key)
		c.chargedBytes -= e.charge
	}
}

func (c *TTLCache) deleteExpiredLocked(now time.Time) {
	for key, e := range c.items {
		if !now.Before(e.expiresAt) {
			c.deleteLocked(key)
		}
	}
}

// evictLoop removes expired entries every 30 seconds.
func (c *TTLCache) evictLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			c.mu.Lock()
			c.deleteExpiredLocked(now)
			c.mu.Unlock()
		case <-c.stopCh:
			return
		}
	}
}
