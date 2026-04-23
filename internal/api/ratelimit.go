package api

import (
	"net/http"
	"sync"
	"time"
)

// RateLimiter is a per-IP token bucket rate limiter middleware.
type RateLimiter struct {
	mu      sync.Mutex
	clients map[string]*ipBucket
	rps     float64 // tokens per second per IP
	burst   float64 // max burst (= rps by default)
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
		ip := clientIP(r)
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

// clientIP extracts the real client IP, respecting X-Forwarded-For.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first (original) IP.
		for i, c := range xff {
			if c == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	// Strip port from RemoteAddr.
	addr := r.RemoteAddr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
