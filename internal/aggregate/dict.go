package aggregate

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
)

// Kind names a dictionary namespace. Uniqueness is scoped to (tenant, kind,
// value), matching the durable UNIQUE(tenant_id, kind, value) constraint the
// Phase 2 store will carry (#159, #173).
type Kind uint8

// Dictionary kinds. Zero is reserved so an un-set Kind is never a valid
// namespace.
const (
	KindTenant      Kind = 1
	KindService     Kind = 2
	KindOperation   Kind = 3
	KindMetricName  Kind = 4
	KindDimKey      Kind = 5
	KindDimValue    Kind = 6
	KindDimTuple    Kind = 7
	KindLogTemplate Kind = 8

	kindMax = KindLogTemplate
)

// kindNames is indexed by Kind. The strings are the persisted `kind` column
// values and double as metric label values, so they are part of the contract.
var kindNames = [...]string{
	KindTenant:      "tenant",
	KindService:     "service",
	KindOperation:   "operation",
	KindMetricName:  "metric_name",
	KindDimKey:      "dim_key",
	KindDimValue:    "dim_value",
	KindDimTuple:    "dim_tuple",
	KindLogTemplate: "log_template",
}

// String implements fmt.Stringer.
func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Valid reports whether k is one of the defined dictionary kinds.
func (k Kind) Valid() bool { return k >= KindTenant && k <= kindMax }

// GlobalTenant is the tenant scope that KindTenant entries themselves live in.
// A tenant name cannot be scoped by its own ID, so the tenant dictionary is
// instance-global.
const GlobalTenant uint32 = 0

// OtherValue is the canonical dictionary value of the per-(tenant, kind)
// overflow entry. It is pre-created outside the capacity cap: a quota must
// never prevent creation of the entry that absorbs quota violations (#158).
const OtherValue = "__other__"

// ErrDictFull is returned by a Registrar when a (tenant, kind) namespace has no
// capacity left. The Cache translates it into the pre-created __other__ ID and
// never propagates it to the hot path — identity resolution never fails.
var ErrDictFull = errors.New("aggregate: dictionary full")

// Registrar mints dictionary IDs. Phase 1 ships MemRegistrar, whose IDs are
// provisional and vanish on restart; Phase 2 (#173) plugs in a SQLite-backed
// registrar that mints durable IDs atomically with the first delta referencing
// them, without any other change to this package.
//
// Implementations MUST be safe for concurrent use, MUST be idempotent (a repeat
// Register for the same (tenant, kind, value) returns the same ID, because the
// Cache deliberately allows concurrent duplicate registrations rather than
// holding a lock across the call), MUST return IDs greater than zero, and MUST
// NOT retain the value slice after returning.
type Registrar interface {
	// Register returns the ID for (tenantID, kind, value), minting one if the
	// value is new. It returns ErrDictFull when the namespace is at capacity.
	Register(tenantID uint32, kind Kind, value []byte) (uint32, error)

	// OtherID returns the pre-created overflow ID for (tenantID, kind). It
	// never fails: the entry is created outside the capacity cap.
	OtherID(tenantID uint32, kind Kind) uint32
}

// dictScope is the (tenant, kind) namespace of a dictionary entry. Splitting
// the scope out of the map key lets the value map stay string-keyed, which is
// what makes the byte-slice lookup path allocation-free.
type dictScope struct {
	tenant uint32
	kind   Kind
}

// CacheStats is a snapshot of Cache counters.
type CacheStats struct {
	// Hits counts lookups served from the in-memory map.
	Hits uint64
	// Misses counts lookups that reached the Registrar.
	Misses uint64
	// Overflows counts misses the Registrar rejected as ErrDictFull, which
	// were routed to the __other__ entry.
	Overflows uint64
	// Errors counts misses the Registrar failed for any other reason. These
	// were also routed to __other__; Phase 2 must surface this as a metric.
	Errors uint64
}

// Cache is the hot-path canonical-value to ID map in front of a Registrar. It
// is safe for concurrent use.
//
// Hits take a read lock and no allocation. Misses call the Registrar without
// holding any lock — a durable registrar performs I/O, and serializing every
// ingest goroutine behind one dictionary mutex would be the whole engine's
// bottleneck. The cost is that two goroutines may register the same value
// concurrently, which is why Registrar is required to be idempotent.
//
// Entries are never evicted: the series caps of #158 bound how many distinct
// identities can exist. Overflow (__other__) resolutions are deliberately NOT
// cached, so a pathological-cardinality tenant cannot grow this map without
// bound; they pay a Registrar call per point instead.
type Cache struct {
	reg Registrar

	mu  sync.RWMutex
	ids map[dictScope]map[string]uint32

	hits      atomic.Uint64
	misses    atomic.Uint64
	overflows atomic.Uint64
	errors    atomic.Uint64
}

// NewCache returns a Cache in front of reg. reg must not be nil.
func NewCache(reg Registrar) *Cache {
	return &Cache{
		reg: reg,
		ids: make(map[dictScope]map[string]uint32),
	}
}

// Intern returns the dictionary ID for a string value, registering it on a
// miss. It never fails: a full or failing dictionary resolves to the
// pre-created __other__ ID for the scope.
func (c *Cache) Intern(tenantID uint32, kind Kind, value string) uint32 {
	scope := dictScope{tenant: tenantID, kind: kind}
	c.mu.RLock()
	id, ok := c.ids[scope][value]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
		return id
	}
	return c.register(scope, []byte(value))
}

// InternBytes is Intern for a byte-slice value (dimension tuples). The slice is
// never retained: it is copied when it becomes a map key. The lookup itself
// does not allocate.
func (c *Cache) InternBytes(tenantID uint32, kind Kind, value []byte) uint32 {
	scope := dictScope{tenant: tenantID, kind: kind}
	c.mu.RLock()
	id, ok := c.ids[scope][string(value)]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
		return id
	}
	return c.register(scope, value)
}

// InternTenant returns the ID for a tenant name, which lives in the
// instance-global tenant namespace.
func (c *Cache) InternTenant(name string) uint32 {
	return c.Intern(GlobalTenant, KindTenant, name)
}

// register handles the miss path: one Registrar call, then a cache store on
// success or an __other__ fallback on failure.
func (c *Cache) register(scope dictScope, value []byte) uint32 {
	c.misses.Add(1)
	id, err := c.reg.Register(scope.tenant, scope.kind, value)
	if err != nil {
		if errors.Is(err, ErrDictFull) {
			c.overflows.Add(1)
		} else {
			c.errors.Add(1)
		}
		return c.reg.OtherID(scope.tenant, scope.kind)
	}
	c.mu.Lock()
	inner := c.ids[scope]
	if inner == nil {
		inner = make(map[string]uint32)
		c.ids[scope] = inner
	}
	inner[string(value)] = id
	c.mu.Unlock()
	return id
}

// Len returns the number of cached entries across every scope. Diagnostic only.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, inner := range c.ids {
		n += len(inner)
	}
	return n
}

// Stats returns a snapshot of the cache counters.
func (c *Cache) Stats() CacheStats {
	return CacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Overflows: c.overflows.Load(),
		Errors:    c.errors.Load(),
	}
}

// DimPair is one operator-configured dimension, already reduced to dictionary
// IDs. The hot path never carries the strings.
type DimPair struct {
	KeyID   uint32
	ValueID uint32
}

// AppendCanonicalDims appends the canonical encoding of pairs to dst: pairs
// sorted by KeyID (ValueID breaks ties so duplicate keys still encode
// deterministically), each pair written as two varints. The same set of pairs
// in any order produces byte-identical output.
//
// pairs is sorted in place — the caller's slice is scratch space on the hot
// path, not a value to preserve.
func AppendCanonicalDims(dst []byte, pairs []DimPair) []byte {
	if len(pairs) == 0 {
		return dst
	}
	slices.SortFunc(pairs, func(a, b DimPair) int {
		switch {
		case a.KeyID != b.KeyID:
			if a.KeyID < b.KeyID {
				return -1
			}
			return 1
		case a.ValueID != b.ValueID:
			if a.ValueID < b.ValueID {
				return -1
			}
			return 1
		default:
			return 0
		}
	})
	for _, p := range pairs {
		dst = binary.AppendUvarint(dst, uint64(p.KeyID))
		dst = binary.AppendUvarint(dst, uint64(p.ValueID))
	}
	return dst
}

// dimTupleScratch sizes the inline canonicalization buffer. Ten dimensions at
// the worst-case ten varint bytes per ID covers any sane AGGREGATE_METRIC_DIMS
// configuration without touching the heap.
const dimTupleScratch = 200

// InternDims canonicalizes pairs and interns the encoding as KindDimTuple,
// returning the DimsID for a SeriesKey. An empty set yields 0, the "no
// configured dims" sentinel. pairs is sorted in place.
func (c *Cache) InternDims(tenantID uint32, pairs []DimPair) uint32 {
	if len(pairs) == 0 {
		return 0
	}
	var scratch [dimTupleScratch]byte
	enc := AppendCanonicalDims(scratch[:0], pairs)
	return c.InternBytes(tenantID, KindDimTuple, enc)
}

// DictEntry is one dictionary row as held by MemRegistrar.
type DictEntry struct {
	ID       uint32
	TenantID uint32
	Kind     Kind
	Value    []byte
}

// MemRegistrarOptions configures a MemRegistrar.
type MemRegistrarOptions struct {
	// Limits caps the number of entries per (tenant, kind). A missing or
	// zero entry means unlimited. __other__ entries are exempt.
	Limits map[Kind]int
}

// MemRegistrar is the Phase 1 in-memory Registrar. IDs are provisional: they
// are minted from a process-local counter and do not survive a restart, which
// is exactly why nothing may persist a SeriesKey until the durable registrar of
// #173 lands. It is safe for concurrent use.
type MemRegistrar struct {
	mu     sync.Mutex
	next   uint32
	limits map[Kind]int
	ids    map[dictScope]map[string]uint32
	counts map[dictScope]int
	other  map[dictScope]uint32
	byID   map[uint32]DictEntry
}

// NewMemRegistrar returns an in-memory registrar. A nil opts means no limits.
func NewMemRegistrar(opts *MemRegistrarOptions) *MemRegistrar {
	r := &MemRegistrar{
		next:   1, // 0 is the "none" sentinel and is never minted.
		ids:    make(map[dictScope]map[string]uint32),
		counts: make(map[dictScope]int),
		other:  make(map[dictScope]uint32),
		byID:   make(map[uint32]DictEntry),
	}
	if opts != nil && len(opts.Limits) > 0 {
		r.limits = make(map[Kind]int, len(opts.Limits))
		for k, v := range opts.Limits {
			r.limits[k] = v
		}
	}
	return r
}

// Register implements Registrar.
func (r *MemRegistrar) Register(tenantID uint32, kind Kind, value []byte) (uint32, error) {
	if !kind.Valid() {
		return 0, fmt.Errorf("aggregate: register: invalid dictionary kind %d", uint8(kind))
	}
	scope := dictScope{tenant: tenantID, kind: kind}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.ids[scope][string(value)]; ok {
		return id, nil
	}
	if limit, ok := r.limits[kind]; ok && limit > 0 && r.counts[scope] >= limit {
		return 0, ErrDictFull
	}
	id, err := r.mintLocked(scope, value)
	if err != nil {
		return 0, err
	}
	r.counts[scope]++
	return id, nil
}

// OtherID implements Registrar. The overflow entry is created on first demand
// and bypasses the capacity cap.
func (r *MemRegistrar) OtherID(tenantID uint32, kind Kind) uint32 {
	scope := dictScope{tenant: tenantID, kind: kind}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.other[scope]; ok {
		return id
	}
	id, err := r.mintLocked(scope, []byte(OtherValue))
	if err != nil {
		// The ID space is exhausted (2^32 entries). Nothing useful is left to
		// return; 0 is the "unresolved" sentinel and the caller's series
		// degrades rather than the write failing.
		return 0
	}
	r.other[scope] = id
	return id
}

// mintLocked allocates the next ID and records the entry. r.mu must be held.
func (r *MemRegistrar) mintLocked(scope dictScope, value []byte) (uint32, error) {
	if r.next == math.MaxUint32 {
		return 0, ErrDictFull
	}
	id := r.next
	r.next++
	inner := r.ids[scope]
	if inner == nil {
		inner = make(map[string]uint32)
		r.ids[scope] = inner
	}
	inner[string(value)] = id
	r.byID[id] = DictEntry{
		ID:       id,
		TenantID: scope.tenant,
		Kind:     scope.kind,
		Value:    slices.Clone(value),
	}
	return id, nil
}

// Lookup resolves an ID back to its entry. Presentation and test use only — the
// hot path never reverses a dictionary ID.
func (r *MemRegistrar) Lookup(id uint32) (DictEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	return e, ok
}

// Count returns the number of capacity-consuming entries in a (tenant, kind)
// namespace, excluding the __other__ entry.
func (r *MemRegistrar) Count(tenantID uint32, kind Kind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[dictScope{tenant: tenantID, kind: kind}]
}
