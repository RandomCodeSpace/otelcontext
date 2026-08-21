package aggregate

import (
	"fmt"
	"math"
	"slices"
	"sync"
)

// Durable identity: dictionary entries and series rows whose IDs are owned by
// the database (ADR 0001, #162's first atomicity invariant).
//
// The hot path needs an ID synchronously — the reducer cannot build a SeriesKey
// without one — but #162 requires that a registration and the first delta that
// references it become durable together. Both hold because an ID is MINTED
// in memory and the row is STAGED: every group commit drains all staged rows,
// so the registration row is always in the same transaction as the first delta
// that could reference it, or in an earlier one.
//
// A crash between minting and committing loses the ID, not correctness: nothing
// committed ever referenced it. On restart the counter reseeds from MAX(id) and
// the number may be minted again for a different value, which is safe for
// exactly the same reason.

// DurableRegistrar is the Registrar backed by the aggregate store. It replaces
// MemRegistrar whenever the store is enabled — the seam in dict.go exists for
// this and needs no other change.
type DurableRegistrar struct {
	mu      sync.Mutex
	next    uint32
	ids     map[dictScope]map[string]uint32
	other   map[dictScope]uint32
	pending map[uint32]DictRow
	limits  map[Kind]int
	counts  map[dictScope]int
}

// NewDurableRegistrar builds a registrar warmed from the store's dictionary so
// IDs survive restart. limits caps entries per (tenant, kind); a missing or
// zero entry means unlimited, and __other__ entries are always exempt.
func NewDurableRegistrar(store Store, limits map[Kind]int) (*DurableRegistrar, error) {
	r := &DurableRegistrar{
		next:    1, // 0 is the "none" sentinel and is never minted.
		ids:     make(map[dictScope]map[string]uint32),
		other:   make(map[dictScope]uint32),
		pending: make(map[uint32]DictRow),
		counts:  make(map[dictScope]int),
	}
	if len(limits) > 0 {
		r.limits = make(map[Kind]int, len(limits))
		for k, v := range limits {
			r.limits[k] = v
		}
	}
	if store == nil {
		return r, nil
	}
	rows, err := store.LoadDict(0)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		scope := dictScope{tenant: row.TenantID, kind: row.Kind}
		inner := r.ids[scope]
		if inner == nil {
			inner = make(map[string]uint32)
			r.ids[scope] = inner
		}
		value := string(row.Value)
		inner[value] = row.ID
		if value == OtherValue {
			r.other[scope] = row.ID
		} else {
			r.counts[scope]++
		}
		if row.ID >= r.next {
			r.next = row.ID + 1
		}
	}
	return r, nil
}

// Register implements Registrar.
func (r *DurableRegistrar) Register(tenantID uint32, kind Kind, value []byte) (uint32, error) {
	if !kind.Valid() {
		return 0, fmt.Errorf("aggregate: register: invalid dictionary kind %d", uint8(kind))
	}
	if len(value) == 0 {
		// An empty value cannot be a durable identity: it resolves back to
		// nothing and every empty value in a namespace would be the same
		// entry. The Cache turns this into the __other__ ID, which is the
		// correct home for an identity we cannot name.
		return 0, ErrDictFull
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

// OtherID implements Registrar. The overflow entry bypasses the capacity cap:
// a quota must never block creation of the entry that absorbs quota violations.
func (r *DurableRegistrar) OtherID(tenantID uint32, kind Kind) uint32 {
	scope := dictScope{tenant: tenantID, kind: kind}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.other[scope]; ok {
		return id
	}
	id, err := r.mintLocked(scope, []byte(OtherValue))
	if err != nil {
		return 0
	}
	r.other[scope] = id
	return id
}

// mintLocked allocates an ID and stages its row. r.mu must be held.
func (r *DurableRegistrar) mintLocked(scope dictScope, value []byte) (uint32, error) {
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
	r.pending[id] = DictRow{ID: id, TenantID: scope.tenant, Kind: scope.kind, Value: slices.Clone(value)}
	return id, nil
}

// DrainPending returns the staged rows for inclusion in the next group commit.
// They stay staged until Committed confirms them, so a failed commit re-offers
// them instead of stranding an ID that no row backs.
func (r *DurableRegistrar) DrainPending() []DictRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return nil
	}
	out := make([]DictRow, 0, len(r.pending))
	for _, row := range r.pending {
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b DictRow) int { return int(a.ID) - int(b.ID) })
	return out
}

// Committed marks drained rows durable.
func (r *DurableRegistrar) Committed(rows []DictRow) {
	if len(rows) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		delete(r.pending, row.ID)
	}
}

// PendingCount returns how many registrations are staged but not yet durable.
func (r *DurableRegistrar) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// seriesRegistry mints and stages series IDs. It follows the registrar's
// pattern exactly, one level up: the ID is needed to write a delta row, the row
// that defines it rides the same transaction.
//
// Unlike the dictionary, series identity is resolved by the WRITER, not the hot
// path — the writer already holds the whole (SeriesKey -> delta) map, so no
// ingest goroutine ever pays for series resolution.
type seriesRegistry struct {
	mu      sync.Mutex
	next    SeriesID
	ids     map[SeriesKey]SeriesID
	keys    map[SeriesID]SeriesKey
	pending map[SeriesID]SeriesRow
}

// newSeriesRegistry warms a registry from the store.
func newSeriesRegistry(store Store) (*seriesRegistry, error) {
	r := &seriesRegistry{
		next:    1,
		ids:     make(map[SeriesKey]SeriesID),
		keys:    make(map[SeriesID]SeriesKey),
		pending: make(map[SeriesID]SeriesRow),
	}
	if store == nil {
		return r, nil
	}
	rows, err := store.LoadSeries(0)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		r.ids[row.Key] = row.ID
		r.keys[row.ID] = row.Key
		if row.ID >= r.next {
			r.next = row.ID + 1
		}
	}
	return r, nil
}

// Resolve returns the series ID for key, minting and staging one on first use.
func (r *seriesRegistry) Resolve(key SeriesKey) SeriesID {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.ids[key]; ok {
		return id
	}
	id := r.next
	r.next++
	r.ids[key] = id
	r.keys[id] = key
	r.pending[id] = SeriesRow{ID: id, Key: key}
	return id
}

// Key resolves an ID back to its identity from the in-memory map.
func (r *seriesRegistry) Key(id SeriesID) (SeriesKey, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.keys[id]
	return k, ok
}

// DrainPending returns the staged series rows for the next group commit.
func (r *seriesRegistry) DrainPending() []SeriesRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return nil
	}
	out := make([]SeriesRow, 0, len(r.pending))
	for _, row := range r.pending {
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b SeriesRow) int { return int(a.ID - b.ID) })
	return out
}

// Committed marks drained series rows durable.
func (r *seriesRegistry) Committed(rows []SeriesRow) {
	if len(rows) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		delete(r.pending, row.ID)
	}
}

// compile-time assertion that the durable registrar can replace the in-memory
// one wherever dict.go expects a Registrar.
var _ Registrar = (*DurableRegistrar)(nil)
