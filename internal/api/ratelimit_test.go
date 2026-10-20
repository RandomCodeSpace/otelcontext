package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRateLimiterIgnoresUntrustedForwardedHeaders(t *testing.T) {
	rl := NewRateLimiter(1)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
		r.RemoteAddr = "192.0.2.1:12345"
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusTooManyRequests
		if i == 0 {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Fatalf("request %d: status = %d, want %d", i, w.Code, want)
		}
	}
}

func TestRateLimiterTrustedProxyChain(t *testing.T) {
	rl := NewRateLimiter(1)
	if err := rl.SetTrustedProxies([]string{"10.0.0.0/8", "2001:db8:1::/48"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, peer, want string
		headers          []string
	}{
		{"direct spoof", "192.0.2.1:80", "192.0.2.1", []string{"198.51.100.1"}},
		{"proxy", "10.0.0.1:80", "198.51.100.1", []string{"198.51.100.1"}},
		{"chain", "10.0.0.1:80", "198.51.100.1", []string{"198.51.100.1, 10.0.0.2"}},
		{"spoofed prefix", "10.0.0.1:80", "198.51.100.1", []string{"203.0.113.9, 198.51.100.1"}},
		{"multiple headers", "10.0.0.1:80", "198.51.100.1", []string{"203.0.113.9", "198.51.100.1, 10.0.0.2"}},
		{"all trusted", "10.0.0.1:80", "10.0.0.3", []string{"10.0.0.3, 10.0.0.2"}},
		{"IPv6", "[2001:db8:1::1]:80", "2001:db8:2::1", []string{" 2001:0db8:2:0::1, 2001:db8:1::2 "}},
		{"mapped IPv4", "[::ffff:10.0.0.1]:80", "198.51.100.1", []string{"::ffff:198.51.100.1"}},
		{"no header", "10.0.0.1:80", "10.0.0.1", nil},
		{"empty", "10.0.0.1:80", "10.0.0.1", []string{""}},
		{"malformed", "10.0.0.1:80", "10.0.0.1", []string{"junk, 10.0.0.2"}},
		{"port", "10.0.0.1:80", "10.0.0.1", []string{"198.51.100.1:80"}},
		{"zone", "10.0.0.1:80", "10.0.0.1", []string{"fe80::1%eth0"}},
		{"too many hops", "10.0.0.1:80", "10.0.0.1", []string{strings.Repeat("10.0.0.2,", maxForwardedHops) + "198.51.100.1"}},
		{"too many bytes", "10.0.0.1:80", "10.0.0.1", []string{strings.Repeat(" ", maxForwardedBytes) + "198.51.100.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.peer
			for _, value := range tc.headers {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := rl.clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRateLimiterProxyConfiguration(t *testing.T) {
	rl := NewRateLimiter(1)
	if err := rl.SetTrustedProxies([]string{"::ffff:10.0.0.0/104"}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:80"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	for _, bad := range []string{"", "10.0.0.1", "10.0.0.0/99", "::ffff:10.0.0.0/80"} {
		if err := rl.SetTrustedProxies([]string{bad}); err == nil {
			t.Fatalf("accepted invalid CIDR %q", bad)
		}
		if got := rl.clientIP(r); got != "198.51.100.1" {
			t.Fatalf("invalid configuration changed trust: %s", got)
		}
	}
	if err := rl.SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
	if got := rl.clientIP(r); got != "10.0.0.1" {
		t.Fatalf("empty configuration still trusts proxy: %s", got)
	}
}

func TestRateLimiterBoundsBuckets(t *testing.T) {
	rl := NewRateLimiter(1)
	rl.mu.Lock()
	for i := 0; i < maxRateLimitClients; i++ {
		rl.clients[fmt.Sprint(i)] = &ipBucket{}
	}
	rl.mu.Unlock()
	if rl.allow("192.0.2.1") {
		t.Fatal("admitted new client with a full bucket map")
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.clients) != maxRateLimitClients {
		t.Fatalf("bucket count = %d, want %d", len(rl.clients), maxRateLimitClients)
	}
}

func TestRateLimiterConcurrentProxyConfiguration(t *testing.T) {
	rl := NewRateLimiter(100)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.0.0.1:80"
			r.Header.Set("X-Forwarded-For", "198.51.100.1")
			for j := 0; j < 100; j++ {
				if err := rl.SetTrustedProxies([]string{"10.0.0.0/8"}); err != nil {
					t.Error(err)
				}
				rl.allow(rl.clientIP(r))
			}
		}()
	}
	wg.Wait()
}

func TestClientIPCanonicalRemotePeer(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"192.0.2.1:1234", "192.0.2.1"},
		{"[2001:db8::1]:1234", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"[::ffff:192.0.2.1]:1234", "192.0.2.1"},
		{"[fe80::1%eth0]:1234", "fe80::1"},
		{"invalid peer", "unknown"},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
