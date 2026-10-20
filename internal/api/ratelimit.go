package api

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	maxRateLimitClients = 65536
	maxForwardedBytes   = 4096
	maxForwardedHops    = 32
)

// RateLimiter is a per-IP token bucket rate limiter middleware.
type RateLimiter struct {
	mu             sync.Mutex
	clients        map[string]*ipBucket
	rps            float64        // tokens per second per IP
	burst          float64        // max burst (= rps by default)
	trustedProxies []netip.Prefix // guarded by mu; published slices are immutable
}

// SetTrustedProxies enables forwarded IPs only for the configured proxy CIDRs.
// Empty means trust no proxy. Invalid configuration leaves the old set intact.
func (rl *RateLimiter) SetTrustedProxies(cidrs []string) error {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return fmt.Errorf("invalid trusted proxy CIDR %q: %w", cidr, err)
		}
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return fmt.Errorf("invalid mapped IPv4 proxy CIDR %q", cidr)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		prefixes = append(prefixes, p.Masked())
	}
	rl.mu.Lock()
	rl.trustedProxies = prefixes
	rl.mu.Unlock()
	return nil
}

// NewRateLimiter creates a RateLimiter with the given requests-per-second limit.
func NewRateLimiter(rps float64) *RateLimiter {
	rl := &RateLimiter{
		clients: make(map[string]*ipBucket),
		rps:     rps,
		burst:   rps,
	}
	go rl.cleanup()
	return rl
}

// Middleware returns an http.Handler that enforces rate limiting.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.clientIP(r)
		if !rl.allow(ip) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MiddlewareExcept returns a handler chain that applies the rate limit only
// when skip(path) returns false. Intended to exempt OTLP ingestion paths from
// the per-IP API limiter.
func (rl *RateLimiter) MiddlewareExcept(skip func(path string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := rl.Middleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip != nil && skip(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

func (rl *RateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.clients[ip]
	if !ok {
		// Preserve existing buckets when full. Evicting an active bucket would
		// let rotating addresses repeatedly replenish their burst allowance.
		if len(rl.clients) >= maxRateLimitClients {
			return false
		}
		b = &ipBucket{
			tokens:   rl.burst,
			lastSeen: time.Now(),
		}
		rl.clients[ip] = b
	}
	b.refill(rl.rps, rl.burst)
	return b.take()
}

// cleanup removes stale IP entries every minute.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-2 * time.Minute)
		for ip, b := range rl.clients {
			if b.lastSeen.Before(cutoff) {
				delete(rl.clients, ip)
			}
		}
		rl.mu.Unlock()
	}
}

type ipBucket struct {
	tokens   float64
	lastSeen time.Time
}

func (b *ipBucket) refill(rps, burst float64) {
	now := time.Now()
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.lastSeen = now
	b.tokens += elapsed * rps
	if b.tokens > burst {
		b.tokens = burst
	}
}

func (b *ipBucket) take() bool {
	if b.tokens >= 1.0 {
		b.tokens--
		return true
	}
	return false
}

// clientIP uses only the socket peer. Invalid peers share one bounded key.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	return addr.WithZone("").Unmap().String()
}

func (rl *RateLimiter) clientIP(r *http.Request) string {
	peer := clientIP(r)
	addr, err := netip.ParseAddr(peer)
	if err != nil {
		return peer
	}
	rl.mu.Lock()
	proxies := rl.trustedProxies
	rl.mu.Unlock()
	trusted := func(ip netip.Addr) bool {
		for _, p := range proxies {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(addr) {
		return peer
	}
	values := r.Header.Values("X-Forwarded-For")
	size := 0
	for _, v := range values {
		size += len(v) + 1
		if size > maxForwardedBytes {
			return peer
		}
	}
	if size == 0 {
		return peer
	}
	hops := strings.Split(strings.Join(values, ","), ",")
	if len(hops) > maxForwardedHops {
		return peer
	}
	addresses := make([]netip.Addr, len(hops))
	for i, hop := range hops {
		ip, err := netip.ParseAddr(strings.TrimSpace(hop))
		if err != nil || ip.Zone() != "" {
			return peer
		}
		addresses[i] = ip.Unmap()
	}
	// Proxies append their observed peer. Stop at the nearest untrusted hop;
	// earlier entries may have been supplied by that client.
	for i := len(addresses) - 1; i >= 0; i-- {
		addr = addresses[i]
		if !trusted(addr) {
			break
		}
	}
	return addr.String()
}
