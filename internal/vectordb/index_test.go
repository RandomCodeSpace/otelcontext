package vectordb

import (
	"strconv"
	"sync"
	"testing"
)

// TestTenantIsolation_Search is the RAN-20 confidentiality bar: a query on
// tenant A never returns a document indexed under tenant B, even when the
// vocabularies collide on the query terms.
func TestTenantIsolation_Search(t *testing.T) {
	idx := New(1_000)

	idx.Add(1, "acme", "checkout", "ERROR", "payment gateway timeout upstream")
	idx.Add(2, "acme", "checkout", "ERROR", "payment gateway refused charge")
	idx.Add(10, "globex", "auth", "ERROR", "payment gateway token expired")
	idx.Add(11, "globex", "auth", "ERROR", "payment gateway 500 internal error")

	acmeHits := idx.Search("acme", "payment gateway timeout", 10)
	if len(acmeHits) == 0 {
		t.Fatalf("acme search returned zero hits despite matching docs")
	}
	for _, h := range acmeHits {
		if h.Tenant != "acme" || h.LogID >= 10 {
			t.Fatalf("acme search leaked id=%d tenant=%q body=%q", h.LogID, h.Tenant, h.Body)
		}
	}

	globexHits := idx.Search("globex", "payment gateway token", 10)
	if len(globexHits) == 0 {
		t.Fatalf("globex search returned zero hits despite matching docs")
	}
	for _, h := range globexHits {
		if h.Tenant != "globex" || h.LogID < 10 {
			t.Fatalf("globex search leaked id=%d tenant=%q body=%q", h.LogID, h.Tenant, h.Body)
		}
	}
}

// TestUnknownTenantReturnsEmpty proves a tenant with no indexed docs returns
// nothing even when other tenants have matching content.
func TestUnknownTenantReturnsEmpty(t *testing.T) {
	idx := New(100)
	idx.Add(1, "acme", "svc", "ERROR", "database connection refused upstream")

	if got := idx.Search("initech", "database connection", 10); len(got) != 0 {
		t.Fatalf("unknown tenant saw %d cross-tenant hits", len(got))
	}
}

// TestEmptyTenantCoercedToDefault verifies Add and Search coerce an empty
// tenant to the platform default so untenanted callers stay isolated from
// real tenants.
func TestEmptyTenantCoercedToDefault(t *testing.T) {
	idx := New(100)
	idx.Add(1, "", "svc", "ERROR", "network unreachable upstream host")

	if hits := idx.Search("", "network unreachable", 10); len(hits) != 1 {
		t.Fatalf("search with empty tenant: want 1 hit, got %d", len(hits))
	}
	if hits := idx.Search(defaultTenantID, "network unreachable", 10); len(hits) != 1 {
		t.Fatalf("search with default tenant id: want 1 hit, got %d", len(hits))
	}
	if hits := idx.Search("acme", "network unreachable", 10); len(hits) != 0 {
		t.Fatalf("acme saw %d cross-tenant hits for default-tenant doc", len(hits))
	}
}

// TestFIFOEvictionFairness is TechLead's requested assertion: a tenant that
// writes near-cap volume cannot evict another tenant's documents from the
// shared index. Under a naive global-FIFO policy tenant B's flood would
// remove tenant A's older entries and A would silently "lose" its warm
// rows — a confidentiality-safe but availability-breaking failure mode.
func TestFIFOEvictionFairness(t *testing.T) {
	const cap = 200
	idx := New(cap)

	// Tenant A writes a small set of distinctive markers.
	for i := 0; i < 5; i++ {
		idx.Add(uint(1+i), "acme", "checkout", "ERROR", "acme-canary-marker alpha beta gamma "+strconv.Itoa(i))
	}

	// Tenant B floods the index well past the cap — enough to trigger
	// multiple eviction cycles.
	for i := 0; i < cap*4; i++ {
		idx.Add(uint(10_000+i), "globex", "svc", "ERROR", "globex chatter filling the index "+strconv.Itoa(i))
	}

	// Every one of acme's canary rows must still be findable.
	hits := idx.Search("acme", "acme-canary-marker alpha beta gamma", 20)
	if len(hits) < 5 {
		t.Fatalf("eviction unfairness: acme canaries evicted by globex flood. want >=5 hits, got %d", len(hits))
	}
	seen := map[uint]bool{}
	for _, h := range hits {
		if h.Tenant != "acme" {
			t.Fatalf("cross-tenant leak during eviction test: id=%d tenant=%q", h.LogID, h.Tenant)
		}
		seen[h.LogID] = true
	}
	for id := uint(1); id <= 5; id++ {
		if !seen[id] {
			t.Fatalf("acme canary id=%d missing after globex flood", id)
		}
	}
}

// TestConcurrentTenantAddSearch pins down race-detector cleanliness and
// cross-tenant isolation under concurrent readers/writers.
func TestConcurrentTenantAddSearch(t *testing.T) {
	idx := New(5_000)
	var wg sync.WaitGroup

	for _, tenant := range []string{"acme", "globex"} {
		wg.Add(2)
		go func(ten string) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				idx.Add(uint(i), ten, "svc", "ERROR", ten+" error kafka partition "+strconv.Itoa(i))
			}
		}(tenant)
		go func(ten string) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				for _, h := range idx.Search(ten, "kafka partition", 5) {
					if h.Tenant != ten {
						t.Errorf("tenant %s saw cross-tenant hit tenant=%q body=%q", ten, h.Tenant, h.Body)
						return
					}
				}
			}
		}(tenant)
	}
	wg.Wait()
}
