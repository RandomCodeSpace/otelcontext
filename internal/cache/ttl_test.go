package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type sizedValue []byte

func (v sizedValue) CacheSizeBytes() int64 { return int64(len(v)) }

func TestBoundedCacheLimitsDistinctKeyChurn(t *testing.T) {
	const (
		maxEntries = 256
		maxBytes   = int64(16 << 20)
	)
	c := NewBounded(maxEntries, maxBytes)
	t.Cleanup(c.Stop)

	maxObservedEntries := 0
	var maxObservedBytes int64
	for i := range 300 {
		if !c.Set(fmt.Sprintf("key-%d", i), sizedValue(make([]byte, 40)), time.Minute) {
			t.Fatalf("Set(%d) rejected an admissible value", i)
		}
		entries, bytes := c.Usage()
		maxObservedEntries = max(maxObservedEntries, entries)
		maxObservedBytes = max(maxObservedBytes, bytes)
		if entries > maxEntries || bytes > maxBytes {
			t.Fatalf("usage after Set(%d) = %d entries/%d bytes, limits %d/%d", i, entries, bytes, maxEntries, maxBytes)
		}
	}
	t.Logf("maximum observed usage: %d entries, %d charged bytes", maxObservedEntries, maxObservedBytes)
}

func TestBoundedCacheByteLimitEvicts(t *testing.T) {
	c := NewBounded(10, 256)
	t.Cleanup(c.Stop)

	if !c.Set("first", sizedValue(make([]byte, 80)), time.Minute) || !c.Set("second", sizedValue(make([]byte, 80)), time.Minute) {
		t.Fatal("Set rejected values that fit individually")
	}
	if _, ok := c.Get("first"); ok {
		t.Fatal("byte pressure retained the older entry")
	}
	if _, ok := c.Get("second"); !ok {
		t.Fatal("byte pressure evicted the admitted entry")
	}
	if entries, bytes := c.Usage(); entries != 1 || bytes > 256 {
		t.Fatalf("usage after byte eviction = %d entries/%d bytes", entries, bytes)
	}
}

func TestBoundedCacheReplacementAndOversize(t *testing.T) {
	c := NewBounded(2, 256)
	t.Cleanup(c.Stop)

	if !c.Set("same", sizedValue(make([]byte, 16)), time.Minute) {
		t.Fatal("initial Set rejected")
	}
	_, before := c.Usage()
	if !c.Set("same", sizedValue(make([]byte, 80)), time.Minute) {
		t.Fatal("replacement Set rejected")
	}
	entries, after := c.Usage()
	if entries != 1 || after <= before {
		t.Fatalf("replacement usage = %d entries/%d bytes, initial bytes %d", entries, after, before)
	}

	if c.Set("same", sizedValue(make([]byte, 1024)), time.Minute) {
		t.Fatal("oversized replacement was admitted")
	}
	if _, ok := c.Get("same"); ok {
		t.Fatal("rejected replacement left the prior value cached")
	}
	if entries, bytes := c.Usage(); entries != 0 || bytes != 0 {
		t.Fatalf("usage after rejected replacement = %d entries/%d bytes", entries, bytes)
	}
}

func TestBoundedCacheExpiryReleasesCharge(t *testing.T) {
	c := NewBounded(1, 256)
	t.Cleanup(c.Stop)

	if !c.Set("expired", sizedValue("payload"), -time.Second) {
		t.Fatal("Set rejected")
	}
	if _, ok := c.Get("expired"); ok {
		t.Fatal("expired value returned as a hit")
	}
	if entries, bytes := c.Usage(); entries != 0 || bytes != 0 {
		t.Fatalf("expired usage = %d entries/%d bytes", entries, bytes)
	}
}

func TestBoundedCacheConcurrentInsertsStayWithinLimits(t *testing.T) {
	const (
		maxEntries = 8
		maxBytes   = 1024
	)
	c := NewBounded(maxEntries, maxBytes)
	t.Cleanup(c.Stop)

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(fmt.Sprintf("key-%d", i), sizedValue(make([]byte, 32)), time.Minute)
		}()
	}
	wg.Wait()
	entries, bytes := c.Usage()
	if entries > maxEntries || bytes > maxBytes {
		t.Fatalf("concurrent usage = %d entries/%d bytes, limits %d/%d", entries, bytes, maxEntries, maxBytes)
	}
}

func TestUnboundedCacheStillAcceptsUnsizedValues(t *testing.T) {
	c := New()
	t.Cleanup(c.Stop)
	if !c.Set("key", struct{}{}, time.Minute) {
		t.Fatal("unbounded cache rejected an unsized value")
	}
	if _, ok := c.Get("key"); !ok {
		t.Fatal("unbounded cache missed stored value")
	}
}

func TestBoundedCacheRejectsUnsizedValues(t *testing.T) {
	c := NewBounded(1, 256)
	t.Cleanup(c.Stop)
	if c.Set("key", struct{}{}, time.Minute) {
		t.Fatal("bounded cache admitted a value without retained-size accounting")
	}
}
