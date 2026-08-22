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
	bounds  Bounds
	counts  map[dictScope]int
	// kindCounts is the instance-wide census behind bounds.InstanceKind: it
	// is what stops a thousand tenants, each just under its own per-tenant
	// cap, from adding up to an unbounded dictionary (#200 Q3).
	kindCounts map[Kind]int
	// tenants counts admitted tenant identities against Bounds.MaxTenants.
	tenants int
	// byID is the reverse index used by the read path (Resolver). IDs are
	// minted from one process-wide counter, so a single flat map is enough:
	// an ID is unique across every (tenant, kind) scope.
	byID map[uint32]DictEntry
	// touched records every ID handed out since the last completed sweep. It
	// is the race-closer for GC (#200 Q2): an ID returned to a hot-path
	// goroutine that has not yet reached the Cache map is invisible to every
	// other root, and collecting it would strand a live identity.
	touched map[uint32]struct{}
	// fenced holds IDs the maintenance barrier has taken out of service.
	// Register refuses them (the Cache routes to __other__) until the sweep
	// either commits — and the entries are removed — or fails and the fence
	// is released with memory untouched.
	fenced map[uint32]struct{}
}

// NewDurableRegistrar builds a registrar warmed from the store's dictionary so
// IDs survive restart. limits caps entries per (tenant, kind); a missing or
// zero entry means unlimited, and __other__ entries are always exempt.
func NewDurableRegistrar(store Store, limits map[Kind]int) (*DurableRegistrar, error) {
	return NewDurableRegistrarWithBounds(store, Bounds{PerTenantKind: limits})
}

// NewDurableRegistrarWithBounds is NewDurableRegistrar with the full #200 Q3
// bound set: encoded-value length caps, per-(tenant, kind) counts, and the
// instance-wide backstops behind them.
//
// The preload is exact, not truncated. LoadDict is asked for one row more than
// the supported bound and a full page fails startup: a silent LIMIT is how a
// registrar comes up believing a value is unregistered, mints a second ID for
// it, and splits one series into two that no query can reunite.
func NewDurableRegistrarWithBounds(store Store, b Bounds) (*DurableRegistrar, error) {
	r := &DurableRegistrar{
		next:       1, // 0 is the "none" sentinel and is never minted.
		ids:        make(map[dictScope]map[string]uint32),
		other:      make(map[dictScope]uint32),
		pending:    make(map[uint32]DictRow),
		bounds:     b.withDefaults(),
		counts:     make(map[dictScope]int),
		kindCounts: make(map[Kind]int),
		byID:       make(map[uint32]DictEntry),
		touched:    make(map[uint32]struct{}),
	}
	if store == nil {
		return r, nil
	}
	rows, err := store.LoadDict(MaxDictRows + 1)
	if err != nil {
		return nil, err
	}
	if len(rows) > MaxDictRows {
		return nil, &PreloadError{Table: "aggregate_dict", Rows: len(rows), Max: MaxDictRows}
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
		r.byID[row.ID] = DictEntry{ID: row.ID, TenantID: row.TenantID, Kind: row.Kind, Value: slices.Clone(row.Value)}
		if value == OtherValue {
			r.other[scope] = row.ID
		} else {
			r.counts[scope]++
			r.kindCounts[row.Kind]++
			if row.Kind == KindTenant {
				r.tenants++
			}
		}
		if row.ID >= r.next {
			r.next = row.ID + 1
		}
	}
	// Reseed from the durable high-watermark, never below it. MAX(id)+1 alone
	// stops being safe the moment GC can delete the highest ID: the next boot
	// would re-mint a number that finalized buckets, alias rows or an
	// operator's saved query may still name (#200 Q1).
	if wm, ok := store.(WatermarkStore); ok {
		dictWM, _, err := wm.Watermarks()
		if err != nil {
			return nil, err
		}
		if dictWM > r.next {
			r.next = dictWM
		}
	}
	return r, nil
}

// PreloadError reports a warm-up load whose retained row count exceeds the
// bound this build supports. It is fatal at startup by design: the alternative
// is a silently truncated identity map.
type PreloadError struct {
	Table string
	Rows  int
	Max   int
}

func (e *PreloadError) Error() string {
	return fmt.Sprintf("aggregate store: %s holds more than %d retained rows (%d+); "+
		"this build cannot warm an identity map that large — raise the bound in a build that supports it "+
		"or let the dictionary GC reduce it with an older binary", e.Table, e.Max, e.Rows)
}

// Next reports the ID the registrar would mint next. It is the value persisted
// as the dictionary high-watermark.
func (r *DurableRegistrar) Next() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
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
	if len(value) > r.bounds.valueCap(kind) {
		// Defensive: the Cache already routes over-length values. A registrar
		// that accepted one would put a value in the dictionary that the hot
		// path can never look up again.
		return 0, ErrDictFull
	}
	scope := dictScope{tenant: tenantID, kind: kind}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.ids[scope][string(value)]; ok {
		if _, blocked := r.fenced[id]; blocked {
			return 0, ErrDictFull
		}
		r.touched[id] = struct{}{}
		return id, nil
	}
	if err := r.capsLocked(scope, kind); err != nil {
		return 0, err
	}
	id, err := r.mintLocked(scope, value)
	if err != nil {
		return 0, err
	}
	r.counts[scope]++
	r.kindCounts[kind]++
	if kind == KindTenant {
		r.tenants++
	}
	return id, nil
}

// capsLocked applies the per-(tenant, kind), instance-wide and tenant-identity
// caps. r.mu must be held.
func (r *DurableRegistrar) capsLocked(scope dictScope, kind Kind) error {
	if kind == KindTenant && r.tenants >= r.bounds.MaxTenants {
		return ErrDictFull
	}
	if limit, ok := r.bounds.PerTenantKind[kind]; ok && r.counts[scope] >= limit {
		return ErrDictFull
	}
	if limit, ok := r.bounds.InstanceKind[kind]; ok && r.kindCounts[kind] >= limit {
		return ErrDictFull
	}
	return nil
}

// OtherID implements Registrar. The overflow entry bypasses the capacity cap:
// a quota must never block creation of the entry that absorbs quota violations.
func (r *DurableRegistrar) OtherID(tenantID uint32, kind Kind) uint32 {
	if kind == KindTenant {
		// There is no __other__ tenant, by decision (#200 Q3): a shared
		// overflow tenant is exactly the cross-tenant merge the cap exists to
		// prevent. Callers get 0 and must refuse the point.
		return 0
	}
	scope := dictScope{tenant: tenantID, kind: kind}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.other[scope]; ok {
		r.touched[id] = struct{}{}
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
	row := DictRow{ID: id, TenantID: scope.tenant, Kind: scope.kind, Value: slices.Clone(value)}
	r.pending[id] = row
	r.byID[id] = DictEntry(row)
	r.touched[id] = struct{}{}
	return id, nil
}

// Lookup implements Resolver. The entry is visible as soon as the ID is minted,
// before the row is durable: a query that resolves a name the next commit will
// persist is correct, and one that cannot resolve it at all is not.
func (r *DurableRegistrar) Lookup(id uint32) (DictEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, blocked := r.fenced[id]; blocked {
		return DictEntry{}, false
	}
	e, ok := r.byID[id]
	return e, ok
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
	// touched records IDs handed out since the last completed sweep, for the
	// same reason the registrar keeps one (#200 Q2).
	touched map[SeriesID]struct{}
}

// newSeriesRegistry warms a registry from the store.
func newSeriesRegistry(store Store) (*seriesRegistry, error) {
	r := &seriesRegistry{
		next:    1,
		ids:     make(map[SeriesKey]SeriesID),
		keys:    make(map[SeriesID]SeriesKey),
		pending: make(map[SeriesID]SeriesRow),
		touched: make(map[SeriesID]struct{}),
	}
	if store == nil {
		return r, nil
	}
	rows, err := store.LoadSeries(MaxSeriesRows + 1)
	if err != nil {
		return nil, err
	}
	if len(rows) > MaxSeriesRows {
		return nil, &PreloadError{Table: "aggregate_series", Rows: len(rows), Max: MaxSeriesRows}
	}
	for _, row := range rows {
		r.ids[row.Key] = row.ID
		r.keys[row.ID] = row.Key
		if row.ID >= r.next {
			r.next = row.ID + 1
		}
	}
	if wm, ok := store.(WatermarkStore); ok {
		_, seriesWM, err := wm.Watermarks()
		if err != nil {
			return nil, err
		}
		if seriesWM > r.next {
			r.next = seriesWM
		}
	}
	return r, nil
}

// Resolve returns the series ID for key, minting and staging one on first use.
func (r *seriesRegistry) Resolve(key SeriesKey) SeriesID {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.ids[key]; ok {
		r.touched[id] = struct{}{}
		return id
	}
	id := r.next
	r.next++
	r.ids[key] = id
	r.keys[id] = key
	r.pending[id] = SeriesRow{ID: id, Key: key}
	r.touched[id] = struct{}{}
	return id
}

// Next reports the ID the registry would mint next — the value persisted as
// the series high-watermark.
func (r *seriesRegistry) Next() SeriesID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
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
var (
	_ Registrar = (*DurableRegistrar)(nil)
	_ Resolver  = (*DurableRegistrar)(nil)
	_ Resolver  = (*MemRegistrar)(nil)
)

// --- GC hooks (#200 Q1, Q2) -------------------------------------------------

// Roots returns the dictionary IDs the registrar itself keeps alive: every
// staged (not yet durable) registration, every pre-created __other__ sentinel,
// and every ID handed out since the last completed sweep.
//
// The __other__ entries are unconditional roots. They absorb quota violations,
// so a sweep that collected one because nothing referenced it this hour would
// force the next overflow to mint a second sentinel for the same namespace.
func (r *DurableRegistrar) Roots() map[uint32]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uint32]struct{}, len(r.pending)+len(r.other)+len(r.touched))
	for id := range r.pending {
		out[id] = struct{}{}
	}
	for _, id := range r.other {
		out[id] = struct{}{}
	}
	for id := range r.touched {
		out[id] = struct{}{}
	}
	return out
}

// Fence takes candidate IDs out of service. Register refuses a fenced ID (the
// Cache routes the value to __other__) and Lookup reports it unknown, but no
// map entry moves: a failed DELETE must be able to release the fence and leave
// memory byte-for-byte as it was.
func (r *DurableRegistrar) Fence(ids map[uint32]struct{}) {
	if len(ids) == 0 {
		return
	}
	r.mu.Lock()
	if r.fenced == nil {
		r.fenced = make(map[uint32]struct{}, len(ids))
	}
	for id := range ids {
		r.fenced[id] = struct{}{}
	}
	r.mu.Unlock()
}

// Unfence releases every fenced ID without changing anything else.
func (r *DurableRegistrar) Unfence() {
	r.mu.Lock()
	r.fenced = nil
	r.mu.Unlock()
}

// Revalidate drops from candidates every ID the registrar can still hand out.
// It runs INSIDE the maintenance barrier, under the registrar mutex, so an ID
// a concurrent Register returned either lands in touched before this runs (and
// survives) or blocks until the sweep has decided (and gets re-minted).
func (r *DurableRegistrar) Revalidate(candidates map[uint32]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range candidates {
		_, staged := r.pending[id]
		_, used := r.touched[id]
		if staged || used {
			delete(candidates, id)
		}
	}
	for _, id := range r.other {
		delete(candidates, id)
	}
}

// Forget removes swept IDs from the forward, reverse and count maps and clears
// the fence. It runs only after the DELETE has committed.
func (r *DurableRegistrar) Forget(ids map[uint32]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range ids {
		entry, ok := r.byID[id]
		if !ok {
			continue
		}
		scope := dictScope{tenant: entry.TenantID, kind: entry.Kind}
		if inner := r.ids[scope]; inner != nil {
			delete(inner, string(entry.Value))
			if len(inner) == 0 {
				delete(r.ids, scope)
			}
		}
		delete(r.byID, id)
		if r.counts[scope] > 0 {
			r.counts[scope]--
			if r.counts[scope] == 0 {
				delete(r.counts, scope)
			}
		}
		if r.kindCounts[entry.Kind] > 0 {
			r.kindCounts[entry.Kind]--
		}
		if entry.Kind == KindTenant && r.tenants > 0 {
			r.tenants--
		}
	}
	r.fenced = nil
	r.touched = make(map[uint32]struct{})
}

// ClearTouched resets the "handed out since the last sweep" set without
// deleting anything. A sweep that collected nothing still has to clear it, or
// the set grows for the life of the process.
func (r *DurableRegistrar) ClearTouched() {
	r.mu.Lock()
	r.touched = make(map[uint32]struct{})
	r.mu.Unlock()
}

// Lock and Unlock expose the registry mutex to the maintenance barrier. The
// series registry is the writer's own structure — the barrier already runs on
// the writer goroutine — so it is held across the sweep rather than fenced.
func (r *seriesRegistry) Lock()   { r.mu.Lock() }
func (r *seriesRegistry) Unlock() { r.mu.Unlock() }

// rootsLocked returns the series IDs the registry keeps alive. r.mu must be
// held.
func (r *seriesRegistry) rootsLocked() map[SeriesID]struct{} {
	out := make(map[SeriesID]struct{}, len(r.pending)+len(r.touched))
	for id := range r.pending {
		out[id] = struct{}{}
	}
	for id := range r.touched {
		out[id] = struct{}{}
	}
	return out
}

// Roots returns the series IDs the registry keeps alive.
func (r *seriesRegistry) Roots() map[SeriesID]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rootsLocked()
}

// revalidateLocked drops from candidates every series the registry can still
// resolve to. r.mu must be held.
func (r *seriesRegistry) revalidateLocked(candidates map[SeriesID]struct{}) {
	for id := range candidates {
		_, staged := r.pending[id]
		_, used := r.touched[id]
		if staged || used {
			delete(candidates, id)
		}
	}
}

// forgetLocked removes swept series from both directions of the map. r.mu must
// be held, and the DELETE must already have committed.
func (r *seriesRegistry) forgetLocked(ids map[SeriesID]struct{}) {
	for id := range ids {
		key, ok := r.keys[id]
		if !ok {
			continue
		}
		delete(r.keys, id)
		if cur, ok := r.ids[key]; ok && cur == id {
			delete(r.ids, key)
		}
	}
	r.touched = make(map[SeriesID]struct{})
}

// clearTouchedLocked resets the touched set. r.mu must be held.
func (r *seriesRegistry) clearTouchedLocked() {
	r.touched = make(map[SeriesID]struct{})
}
