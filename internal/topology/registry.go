package topology

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Resource registry bounds (#279). Constants, mirroring the SignalStore caps:
// a shared __other__ host would be exactly the cross-host merge the registry
// exists to avoid, so overflow is counted and dropped, never collapsed.
const (
	// RegistryMaxHostsPerTenant bounds distinct non-empty hosts per tenant.
	RegistryMaxHostsPerTenant = 1000
	// RegistryMaxEntriesPerTenant bounds service-host(-workload) entries per tenant.
	RegistryMaxEntriesPerTenant = 10000
	// RegistryIdleTTL evicts an entry no signal has touched for this long.
	RegistryIdleTTL = 24 * time.Hour
	// RegistryFlushInterval is the dirty-tick period used by Run.
	RegistryFlushInterval = 30 * time.Second
	// registryDirtyGranularity bounds how often a live entry is rewritten for
	// last_seen alone: an entry touched every second is persisted once a minute.
	registryDirtyGranularity = int64(60)
)

// Overflow kinds, also the metric label values.
const (
	RegistryKindHost = "host"
	RegistryKindPair = "pair"
)

// Signal is one bit of ResourceEntry.Signals.
type Signal uint8

const (
	SignalTraces Signal = 1 << iota
	SignalLogs
	SignalMetrics
)

// ResourceKey identifies one registered resource. Host is host.id else
// host.name; Workload is k8s.pod.uid else container.id else process.pid. Both
// may be empty: a resource without a host registers the service alone.
type ResourceKey struct {
	Tenant   string
	Service  string
	Host     string
	Workload string
}

// ResourceEntry is one row of a read-only registry snapshot.
type ResourceEntry struct {
	ResourceKey
	// Kind names the slot that filled Workload (pod|container|process), or "".
	Kind     string
	Signals  Signal
	LastSeen time.Time
}

type registryEntry struct {
	kind        string
	signals     Signal
	lastSeen    int64 // unix seconds
	persistedAt int64 // last_seen value the store holds, unix seconds
	dirty       bool
}

type tenantState struct {
	entries int
	hosts   map[string]int // non-empty host -> entries referencing it
}

// Registry is the bounded in-memory resource registry. Registration is one
// mutex acquisition and one map lookup; the already-present path allocates
// nothing.
type Registry struct {
	mu       sync.Mutex
	entries  map[ResourceKey]*registryEntry
	tenants  map[string]*tenantState
	evicted  map[ResourceKey]struct{} // evicted since the last flush
	overflow map[string]map[string]uint64
	metrics  *telemetry.Metrics
}

// NewRegistry returns an empty registry. metrics may be nil.
func NewRegistry(metrics *telemetry.Metrics) *Registry {
	return &Registry{
		entries:  make(map[ResourceKey]*registryEntry),
		tenants:  make(map[string]*tenantState),
		evicted:  make(map[ResourceKey]struct{}),
		overflow: make(map[string]map[string]uint64),
		metrics:  metrics,
	}
}

// Register records that signal arrived from the resource at now. It reports
// false when a per-tenant bound refused a new entry.
func (r *Registry) Register(tenant, service, host, workload, kind string, signal Signal, now time.Time) bool {
	key := ResourceKey{Tenant: tenant, Service: service, Host: host, Workload: workload}
	sec := now.Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[key]; ok {
		if e.signals&signal == 0 {
			e.signals |= signal
			e.dirty = true
		}
		if sec > e.lastSeen {
			e.lastSeen = sec
			if sec-e.persistedAt >= registryDirtyGranularity {
				e.dirty = true
			}
		}
		return true
	}
	e, ok := r.admit(key, kind, signal, sec)
	if ok {
		e.dirty = true
	}
	return ok
}

// admit inserts a new entry under the per-tenant bounds. Caller holds mu.
func (r *Registry) admit(key ResourceKey, kind string, signal Signal, sec int64) (*registryEntry, bool) {
	ts := r.tenants[key.Tenant]
	if ts == nil {
		ts = &tenantState{hosts: make(map[string]int)}
	}
	if ts.entries >= RegistryMaxEntriesPerTenant {
		r.recordOverflow(key.Tenant, RegistryKindPair)
		return nil, false
	}
	if key.Host != "" {
		if _, seen := ts.hosts[key.Host]; !seen && len(ts.hosts) >= RegistryMaxHostsPerTenant {
			r.recordOverflow(key.Tenant, RegistryKindHost)
			return nil, false
		}
		ts.hosts[key.Host]++
	}
	ts.entries++
	r.tenants[key.Tenant] = ts
	e := &registryEntry{kind: kind, signals: signal, lastSeen: sec, persistedAt: sec}
	r.entries[key] = e
	delete(r.evicted, key)
	return e, true
}

func (r *Registry) recordOverflow(tenant, kind string) {
	byKind := r.overflow[tenant]
	if byKind == nil {
		byKind = make(map[string]uint64)
		r.overflow[tenant] = byKind
	}
	byKind[kind]++
	r.metrics.RecordResourceRegistryOverflow(tenant, kind)
}

// Overflow returns how many registrations the bound of kind refused for tenant.
func (r *Registry) Overflow(tenant, kind string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.overflow[tenant][kind]
}

// Evict drops entries idle past RegistryIdleTTL at now and returns how many.
// Their rows are deleted by the next Flush.
func (r *Registry) Evict(now time.Time) int {
	cutoff := now.Add(-RegistryIdleTTL).Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	evicted := 0
	for key, e := range r.entries {
		if e.lastSeen >= cutoff {
			continue
		}
		delete(r.entries, key)
		r.evicted[key] = struct{}{}
		evicted++
		ts := r.tenants[key.Tenant]
		if ts == nil {
			continue
		}
		ts.entries--
		if key.Host != "" {
			if ts.hosts[key.Host]--; ts.hosts[key.Host] <= 0 {
				delete(ts.hosts, key.Host)
			}
		}
		if ts.entries <= 0 {
			delete(r.tenants, key.Tenant)
			r.metrics.SetResourceRegistryEntries(key.Tenant, RegistryKindPair, 0)
			r.metrics.SetResourceRegistryEntries(key.Tenant, RegistryKindHost, 0)
		}
	}
	return evicted
}

// Snapshot returns every live entry ordered by key.
func (r *Registry) Snapshot() []ResourceEntry {
	r.mu.Lock()
	out := make([]ResourceEntry, 0, len(r.entries))
	for key, e := range r.entries {
		out = append(out, ResourceEntry{ResourceKey: key, Kind: e.kind, Signals: e.signals, LastSeen: time.Unix(e.lastSeen, 0).UTC()})
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].ResourceKey, out[j].ResourceKey
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Workload < b.Workload
	})
	return out
}

// publishGauges refreshes the per-tenant entry gauges.
func (r *Registry) publishGauges() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for tenant, ts := range r.tenants {
		r.metrics.SetResourceRegistryEntries(tenant, RegistryKindPair, ts.entries)
		r.metrics.SetResourceRegistryEntries(tenant, RegistryKindHost, len(ts.hosts))
	}
}

// registryRowID is the stable row identity of one key within its tenant.
func registryRowID(key ResourceKey) int64 {
	h := fnv.New64a()
	for _, part := range [...]string{key.Service, key.Host, key.Workload} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return int64(h.Sum64()) //nolint:gosec // intentional bit-reinterpret of FNV-64 for DB portability
}

// Load reloads persisted entries through the same bounds as Register. Rows
// already idle past the TTL are loaded too; the first tick evicts them and
// the following flush deletes their rows.
func (r *Registry) Load(ctx context.Context, db *gorm.DB) (int, error) {
	var rows []storage.ResourceRegistryEntry
	if err := db.WithContext(ctx).Find(&rows).Error; err != nil {
		return 0, fmt.Errorf("load resource registry: %w", err)
	}
	loaded := 0
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		key := ResourceKey{Tenant: row.TenantID, Service: row.ServiceName, Host: row.Host, Workload: row.Workload}
		if _, exists := r.entries[key]; exists {
			continue
		}
		if _, ok := r.admit(key, row.Kind, Signal(row.Signals), row.LastSeen.Unix()); ok { // #nosec G115 -- signals is a 3-bit mask
			loaded++
		}
	}
	return loaded, nil
}

// Flush upserts dirty entries and deletes evicted rows. A failed write leaves
// the affected entries dirty (or evicted) for the next tick.
func (r *Registry) Flush(ctx context.Context, db *gorm.DB) error {
	r.mu.Lock()
	var rows []storage.ResourceRegistryEntry
	var dirtyKeys []ResourceKey
	for key, e := range r.entries {
		if !e.dirty {
			continue
		}
		e.dirty = false
		e.persistedAt = e.lastSeen
		dirtyKeys = append(dirtyKeys, key)
		rows = append(rows, storage.ResourceRegistryEntry{
			TenantID:    key.Tenant,
			ID:          registryRowID(key),
			ServiceName: key.Service,
			Host:        key.Host,
			Workload:    key.Workload,
			Kind:        e.kind,
			Signals:     int64(e.signals),
			LastSeen:    time.Unix(e.lastSeen, 0).UTC(),
		})
	}
	deleteIDs := make(map[string][]int64)
	evictedKeys := make([]ResourceKey, 0, len(r.evicted))
	for key := range r.evicted {
		deleteIDs[key.Tenant] = append(deleteIDs[key.Tenant], registryRowID(key))
		evictedKeys = append(evictedKeys, key)
		delete(r.evicted, key)
	}
	r.mu.Unlock()

	db = db.WithContext(ctx)
	if len(rows) > 0 {
		err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"kind", "signals", "last_seen"}),
		}).CreateInBatches(&rows, 500).Error
		if err != nil {
			r.mu.Lock()
			for _, key := range dirtyKeys {
				if e, ok := r.entries[key]; ok {
					e.dirty = true
				}
			}
			r.mu.Unlock()
			return fmt.Errorf("flush resource registry: %w", err)
		}
	}
	for tenant, ids := range deleteIDs {
		if err := db.Where("tenant_id = ? AND id IN ?", tenant, ids).Delete(&storage.ResourceRegistryEntry{}).Error; err != nil {
			r.mu.Lock()
			for _, key := range evictedKeys {
				if _, live := r.entries[key]; !live {
					r.evicted[key] = struct{}{}
				}
			}
			r.mu.Unlock()
			return fmt.Errorf("delete evicted resource registry rows: %w", err)
		}
	}
	return nil
}

// Run evicts, publishes gauges and flushes on every tick until ctx is done.
// The caller flushes once more at shutdown.
func (r *Registry) Run(ctx context.Context, db *gorm.DB, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			r.Evict(now)
			r.publishGauges()
			if err := r.Flush(ctx, db); err != nil {
				slog.Warn("resource registry flush failed", "error", err)
			}
		}
	}
}
