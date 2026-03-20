package cache

import (
	"sync"
	"time"
)

type entry struct {
	value     interface{}
	expiresAt time.Time
}

// TTLCache is a simple in-memory cache with per-entry TTL expiry.
// Safe for concurrent use. Background goroutine evicts stale entries every 30s.
// Call Stop() to shut down the background goroutine.
type TTLCache struct {
	mu     sync.RWMutex
	items  map[string]entry
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New creates a new TTLCache and starts the background eviction loop.
func New() *TTLCache {
	c := &TTLCache{
		items:  make(map[string]entry),
		stopCh: make(chan struct{}),
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

// Set stores value under key with the given TTL.
func (c *TTLCache) Set(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	c.items[key] = entry{value: value, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
}

// Get returns the cached value and true if it exists and has not expired.
func (c *TTLCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

// Delete removes a key immediately.
func (c *TTLCache) Delete(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
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
			for k, e := range c.items {
				if now.After(e.expiresAt) {
					delete(c.items, k)
				}
			}
			c.mu.Unlock()
		case <-c.stopCh:
			return
		}
	}
}
