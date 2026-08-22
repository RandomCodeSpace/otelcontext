package aggregate

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestKindStringAndValid(t *testing.T) {
	cases := map[Kind]string{
		KindTenant:      "tenant",
		KindService:     "service",
		KindOperation:   "operation",
		KindMetricName:  "metric_name",
		KindDimKey:      "dim_key",
		KindDimValue:    "dim_value",
		KindDimTuple:    "dim_tuple",
		KindLogTemplate: "log_template",
	}
	if len(cases) != int(kindMax) {
		t.Fatalf("test covers %d kinds, package defines %d", len(cases), kindMax)
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", uint8(k), got, want)
		}
		if !k.Valid() {
			t.Errorf("Kind(%d).Valid() = false", uint8(k))
		}
	}
	if Kind(0).Valid() || (kindMax + 1).Valid() {
		t.Error("out-of-range kinds reported valid")
	}
	if got := Kind(0).String(); got != "kind(0)" {
		t.Errorf("Kind(0).String() = %q", got)
	}
}

func TestCacheInternIsStableAndTenantScoped(t *testing.T) {
	c := NewCache(NewMemRegistrar(nil))

	a1 := c.Intern(1, KindService, "checkout")
	a2 := c.Intern(1, KindService, "checkout")
	if a1 == 0 || a1 != a2 {
		t.Fatalf("repeat intern: %d then %d", a1, a2)
	}

	// Same value, different tenant: distinct identity.
	b := c.Intern(2, KindService, "checkout")
	if b == a1 {
		t.Fatalf("tenant isolation broken: tenant 1 and 2 share ID %d", a1)
	}

	// Same value, different kind: distinct identity.
	k := c.Intern(1, KindOperation, "checkout")
	if k == a1 {
		t.Fatalf("kind isolation broken: service and operation share ID %d", a1)
	}

	// Different value, same scope: distinct identity.
	other := c.Intern(1, KindService, "payments")
	if other == a1 {
		t.Fatalf("distinct values share ID %d", a1)
	}

	stats := c.Stats()
	if stats.Hits != 1 || stats.Misses != 4 {
		t.Fatalf("stats = %+v, want 1 hit / 4 misses", stats)
	}
	if stats.Overflows != 0 || stats.Errors != 0 {
		t.Fatalf("unexpected overflow/error counters: %+v", stats)
	}
	if c.Len() != 4 {
		t.Fatalf("cache len = %d, want 4", c.Len())
	}
}

func TestCacheInternTenantUsesGlobalScope(t *testing.T) {
	reg := NewMemRegistrar(nil)
	c := NewCache(reg)
	id, _ := c.InternTenant("acme")
	if id != c.Intern(GlobalTenant, KindTenant, "acme") {
		t.Fatal("InternTenant disagrees with the global tenant scope")
	}
	entry, ok := reg.Lookup(id)
	if !ok || entry.Kind != KindTenant || entry.TenantID != GlobalTenant || string(entry.Value) != "acme" {
		t.Fatalf("registrar entry = %+v ok=%v", entry, ok)
	}
}

func TestCacheDictionaryFullRoutesToOther(t *testing.T) {
	reg := NewMemRegistrar(&MemRegistrarOptions{Limits: map[Kind]int{KindOperation: 2}})
	c := NewCache(reg)

	first := c.Intern(1, KindOperation, "op-a")
	second := c.Intern(1, KindOperation, "op-b")
	if first == 0 || second == 0 || first == second {
		t.Fatalf("first two interns: %d %d", first, second)
	}

	other := c.Intern(1, KindOperation, "op-c")
	wantOther := reg.OtherID(1, KindOperation)
	if other != wantOther {
		t.Fatalf("overflow intern = %d, want __other__ ID %d", other, wantOther)
	}
	if other == 0 {
		t.Fatal("__other__ ID is zero — identity resolution must never fail")
	}
	// A second overflowing value lands on the same __other__ entry, and the
	// overflow is deliberately not cached (otherwise a hostile cardinality
	// spike grows the cache without bound).
	if got := c.Intern(1, KindOperation, "op-d"); got != wantOther {
		t.Fatalf("second overflow = %d, want %d", got, wantOther)
	}
	if c.Len() != 2 {
		t.Fatalf("cache len = %d, want 2 — overflow must not be cached", c.Len())
	}
	if stats := c.Stats(); stats.Overflows != 2 || stats.Errors != 0 {
		t.Fatalf("stats = %+v, want 2 overflows / 0 errors", stats)
	}

	// The cap is per (tenant, kind): another tenant still has its full budget,
	// and the __other__ entry itself did not consume capacity.
	if got := c.Intern(2, KindOperation, "op-c"); got == wantOther {
		t.Fatal("tenant 2 was starved by tenant 1's exhausted namespace")
	}
	if n := reg.Count(1, KindOperation); n != 2 {
		t.Fatalf("tenant 1 capacity-consuming entries = %d, want 2", n)
	}

	// Values already interned before the dictionary filled still resolve.
	if got := c.Intern(1, KindOperation, "op-a"); got != first {
		t.Fatalf("pre-existing value now resolves to %d, want %d", got, first)
	}
}

// failingRegistrar fails every registration with a non-ErrDictFull error, the
// shape a durable Phase 2 registrar takes when the database is unhappy.
type failingRegistrar struct {
	mem *MemRegistrar
	err error
}

func (f *failingRegistrar) Register(uint32, Kind, []byte) (uint32, error) {
	return 0, f.err
}

func (f *failingRegistrar) OtherID(tenantID uint32, kind Kind) uint32 {
	return f.mem.OtherID(tenantID, kind)
}

func TestCacheRegistrarErrorRoutesToOther(t *testing.T) {
	mem := NewMemRegistrar(nil)
	reg := &failingRegistrar{mem: mem, err: errors.New("database is on fire")}
	c := NewCache(reg)

	id := c.Intern(1, KindService, "checkout")
	if id != mem.OtherID(1, KindService) || id == 0 {
		t.Fatalf("registrar error resolved to %d, want the __other__ ID", id)
	}
	stats := c.Stats()
	if stats.Errors != 1 || stats.Overflows != 0 {
		t.Fatalf("stats = %+v, want 1 error / 0 overflows", stats)
	}
}

func TestCacheConcurrentInternIsConsistent(t *testing.T) {
	const (
		goroutines = 16
		values     = 64
		rounds     = 8
	)
	c := NewCache(NewMemRegistrar(nil))

	var wg sync.WaitGroup
	results := make([]map[string]uint32, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := make(map[string]uint32, values)
			for range rounds {
				for v := range values {
					name := fmt.Sprintf("svc-%d", v)
					id := c.Intern(7, KindService, name)
					if id == 0 {
						t.Errorf("intern %q returned 0", name)
						return
					}
					if prev, ok := seen[name]; ok && prev != id {
						t.Errorf("intern %q returned %d then %d", name, prev, id)
						return
					}
					seen[name] = id
				}
			}
			results[g] = seen
		}()
	}
	wg.Wait()

	if c.Len() != values {
		t.Fatalf("cache len = %d, want %d", c.Len(), values)
	}
	for g, seen := range results {
		for name, id := range seen {
			if want := results[0][name]; id != want {
				t.Fatalf("goroutine %d saw %q as %d, goroutine 0 saw %d", g, name, id, want)
			}
		}
	}
}

func TestAppendCanonicalDimsIsOrderIndependent(t *testing.T) {
	orders := [][]DimPair{
		{{KeyID: 3, ValueID: 30}, {KeyID: 1, ValueID: 10}, {KeyID: 2, ValueID: 20}},
		{{KeyID: 1, ValueID: 10}, {KeyID: 2, ValueID: 20}, {KeyID: 3, ValueID: 30}},
		{{KeyID: 2, ValueID: 20}, {KeyID: 3, ValueID: 30}, {KeyID: 1, ValueID: 10}},
	}
	var want []byte
	for i, pairs := range orders {
		got := AppendCanonicalDims(nil, pairs)
		if i == 0 {
			want = got
			continue
		}
		if string(got) != string(want) {
			t.Fatalf("order %d encoded as %v, want %v", i, got, want)
		}
	}
	if len(want) == 0 {
		t.Fatal("canonical encoding is empty")
	}
	if got := AppendCanonicalDims(nil, nil); got != nil {
		t.Fatalf("empty pairs encoded as %v, want nil", got)
	}
}

func TestInternDimsIsOrderIndependent(t *testing.T) {
	c := NewCache(NewMemRegistrar(nil))

	base := c.InternDims(1, []DimPair{{KeyID: 9, ValueID: 90}, {KeyID: 4, ValueID: 40}, {KeyID: 7, ValueID: 70}})
	if base == 0 {
		t.Fatal("non-empty dims interned to 0, the no-dims sentinel")
	}
	shuffled := c.InternDims(1, []DimPair{{KeyID: 7, ValueID: 70}, {KeyID: 9, ValueID: 90}, {KeyID: 4, ValueID: 40}})
	if shuffled != base {
		t.Fatalf("reordered pairs interned to %d, want %d", shuffled, base)
	}

	// A different value on the same key is a different tuple.
	if got := c.InternDims(1, []DimPair{{KeyID: 9, ValueID: 91}, {KeyID: 4, ValueID: 40}, {KeyID: 7, ValueID: 70}}); got == base {
		t.Fatal("a different dimension value produced the same DimsID")
	}
	// A subset is a different tuple.
	if got := c.InternDims(1, []DimPair{{KeyID: 9, ValueID: 90}, {KeyID: 4, ValueID: 40}}); got == base {
		t.Fatal("a subset of dimensions produced the same DimsID")
	}
	// Tuples are tenant-scoped like every other dictionary kind.
	if got := c.InternDims(2, []DimPair{{KeyID: 9, ValueID: 90}, {KeyID: 4, ValueID: 40}, {KeyID: 7, ValueID: 70}}); got == base {
		t.Fatal("dim tuples leak across tenants")
	}
	// No configured dims is the 0 sentinel, never a dictionary entry.
	if got := c.InternDims(1, nil); got != 0 {
		t.Fatalf("empty dims interned to %d, want 0", got)
	}
}

func TestInternDimsHandlesLargeIDs(t *testing.T) {
	c := NewCache(NewMemRegistrar(nil))
	pairs := []DimPair{{KeyID: ^uint32(0), ValueID: ^uint32(0)}, {KeyID: 1, ValueID: ^uint32(0) - 1}}
	id := c.InternDims(1, pairs)
	if id == 0 {
		t.Fatal("large IDs interned to 0")
	}
	if again := c.InternDims(1, pairs); again != id {
		t.Fatalf("repeat intern of large IDs = %d, want %d", again, id)
	}
}

func TestMemRegistrarIsIdempotentAndRejectsBadKind(t *testing.T) {
	r := NewMemRegistrar(nil)
	first, err := r.Register(1, KindService, []byte("checkout"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	second, err := r.Register(1, KindService, []byte("checkout"))
	if err != nil {
		t.Fatalf("Register (repeat): %v", err)
	}
	if first != second {
		t.Fatalf("Register is not idempotent: %d then %d", first, second)
	}
	if _, err := r.Register(1, Kind(0), []byte("x")); err == nil {
		t.Fatal("Register accepted an invalid kind")
	}
	if _, err := r.Register(1, Kind(200), []byte("x")); err == nil {
		t.Fatal("Register accepted an out-of-range kind")
	}
}

func TestMemRegistrarOtherBypassesTheCap(t *testing.T) {
	r := NewMemRegistrar(&MemRegistrarOptions{Limits: map[Kind]int{KindLogTemplate: 1}})
	if _, err := r.Register(1, KindLogTemplate, []byte("tmpl-a")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := r.Register(1, KindLogTemplate, []byte("tmpl-b")); !errors.Is(err, ErrDictFull) {
		t.Fatalf("Register past the cap = %v, want ErrDictFull", err)
	}
	other := r.OtherID(1, KindLogTemplate)
	if other == 0 {
		t.Fatal("OtherID returned 0 with the namespace full")
	}
	if again := r.OtherID(1, KindLogTemplate); again != other {
		t.Fatalf("OtherID is not stable: %d then %d", other, again)
	}
	entry, ok := r.Lookup(other)
	if !ok || string(entry.Value) != OtherValue {
		t.Fatalf("__other__ entry = %+v ok=%v", entry, ok)
	}
	if n := r.Count(1, KindLogTemplate); n != 1 {
		t.Fatalf("capacity-consuming entries = %d, want 1 — __other__ must be exempt", n)
	}
}

func TestMemRegistrarDoesNotRetainValueSlice(t *testing.T) {
	r := NewMemRegistrar(nil)
	buf := []byte("checkout")
	id, err := r.Register(1, KindService, buf)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	copy(buf, "OVERWRIT")
	entry, ok := r.Lookup(id)
	if !ok || string(entry.Value) != "checkout" {
		t.Fatalf("registrar retained the caller's slice: %q", entry.Value)
	}
	if got := r.Count(1, KindService); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}

func BenchmarkCacheInternHit(b *testing.B) {
	c := NewCache(NewMemRegistrar(nil))
	c.Intern(1, KindService, "checkout")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if c.Intern(1, KindService, "checkout") == 0 {
			b.Fatal("intern returned 0")
		}
	}
}
