package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// cacheableTools is the whitelist of tool names whose results are safe to
// memoize for a short window. We cache only the "instant in-memory"
// GraphRAG tools — they are computed off the live in-memory store, so a
// short cache TTL adds no observability lag worth worrying about while
// dramatically reducing CPU under tight polling loops from agent clients.
//
// DB-backed tools (get_investigations, get_log_context, etc.) are
// deliberately NOT cached — they reflect retention/replay state that
// changes meaningfully on millisecond scales and the per-call DB cost is
// already bounded by the storage layer.
var cacheableTools = map[string]struct{}{
	"get_service_map":      {},
	"impact_analysis":      {},
	"root_cause_analysis":  {},
	"get_anomaly_timeline": {},
	"get_service_health":   {},
}

// isCacheable reports whether a tool name is on the cache whitelist.
func isCacheable(name string) bool {
	_, ok := cacheableTools[name]
	return ok
}

// resultCache is a tiny per-tenant TTL cache for MCP tool results. It
// stores the rendered ToolCallResult so cache hits return the exact bytes
// the cold path produced. The map is bounded by maxEntries; on overflow
// the cache evicts the oldest entry deterministically.
//
// Concurrency: a single sync.RWMutex covers both the map and the LRU-ish
// timestamp used for eviction. For the expected load (≤ a few thousand
// hits/sec from agent polling), this is significantly cheaper than the
// per-tool cost we are saving and keeps the implementation auditable.
type resultCache struct {
	ttl        time.Duration
	maxEntries int
	mu         sync.RWMutex
	entries    map[string]cachedResult
}

type cachedResult struct {
	result   ToolCallResult
	expireAt time.Time
}

// newResultCache constructs a cache with the given TTL and max-entry cap.
// ttl <= 0 disables caching entirely (Get/Set become no-ops).
func newResultCache(ttl time.Duration, maxEntries int) *resultCache {
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &resultCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[string]cachedResult, maxEntries),
	}
}

// disabled reports whether the cache is a no-op.
func (c *resultCache) disabled() bool { return c == nil || c.ttl <= 0 }

// key computes a stable cache key from (tenant, tool, args). Args order
// does not affect the key — JSON serialization is normalized via a sorted
// key list so {"a":1,"b":2} and {"b":2,"a":1} hash to the same value.
func cacheKey(tenant, tool string, args map[string]any) string {
	h := sha256.New()
	_, _ = h.Write([]byte(tenant))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(tool))
	_, _ = h.Write([]byte{0})
	if args != nil {
		// Stable serialization — Go's encoding/json doesn't guarantee map
		// key order without help.
		keys := make([]string, 0, len(args))
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			_, _ = h.Write([]byte(k))
			_, _ = h.Write([]byte("="))
			b, _ := json.Marshal(args[k])
			_, _ = h.Write(b)
			_, _ = h.Write([]byte{1})
		}
	}
	sum := h.Sum(nil)
	return strings.ToLower(hex.EncodeToString(sum))
}

// Get returns the cached result for (tenant, tool, args) and a boolean
// indicating cache hit. Expired entries return false (and are NOT lazily
// evicted here — the next Set bound check handles eviction in bulk).
func (c *resultCache) Get(tenant, tool string, args map[string]any) (ToolCallResult, bool) {
	if c.disabled() || !isCacheable(tool) {
		return ToolCallResult{}, false
	}
	k := cacheKey(tenant, tool, args)
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.entries[k]
	if !ok {
		return ToolCallResult{}, false
	}
	if time.Now().After(v.expireAt) {
		return ToolCallResult{}, false
	}
	return v.result, true
}

// Set records a tool result. If the cache is over capacity, ~10% of the
// oldest entries are dropped in one pass — a cheap eviction policy that
// keeps the map size bounded without dragging in a full LRU list.
func (c *resultCache) Set(tenant, tool string, args map[string]any, result ToolCallResult) {
	if c.disabled() || !isCacheable(tool) {
		return
	}
	k := cacheKey(tenant, tool, args)
	exp := time.Now().Add(c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxEntries {
		c.evictBatch()
	}
	c.entries[k] = cachedResult{result: result, expireAt: exp}
}

// evictBatch drops ~10% of entries with the soonest expiry. Called under mu.
func (c *resultCache) evictBatch() {
	if len(c.entries) == 0 {
		return
	}
	// Collect (key, expireAt) pairs and partial-sort by expireAt.
	type kv struct {
		key string
		exp time.Time
	}
	pairs := make([]kv, 0, len(c.entries))
	for k, v := range c.entries {
		pairs = append(pairs, kv{k, v.expireAt})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].exp.Before(pairs[j].exp) })
	drop := max(len(pairs)/10, 1)
	for i := range drop {
		delete(c.entries, pairs[i].key)
	}
}

// Stats returns the current cache size. Test/observability hook.
func (c *resultCache) Stats() (size int) {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
