package topology

import (
	"context"
	"errors"
	"maps"
	"sort"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

const graphReadRows = 50000

type InstanceQuery struct {
	Start, End  time.Time
	Environment *string // nil selects all, empty selects Unknown only
	Mode        string
	ServiceID   string // opaque service group identity, never an ambiguous name
	Hops, Limit int
}

type InstanceNode struct {
	ID              string   `json:"id"`
	ServiceID       string   `json:"service_id"`
	Name            string   `json:"name"`
	Namespace       string   `json:"namespace"`
	Environment     string   `json:"environment"`
	Instance        string   `json:"instance,omitempty"`
	Identity        string   `json:"identity"`
	Hosts           []string `json:"hosts"`
	Instances       []string `json:"instances"`
	InstanceCount   int      `json:"instance_count"`
	UnresolvedCount int      `json:"unresolved_count"`
	Spans           int64    `json:"spans"`
	Requests        int64    `json:"requests"`
	Errors          int64    `json:"errors"`
	Rate            float64  `json:"request_rate_rps"`
	AvgLatencyMS    float64  `json:"avg_latency_ms"`
	ErrorRate       float64  `json:"error_rate"`
	duration        int64
}

type InstanceEdge struct {
	ID           string  `json:"id"`
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	Calls        int64   `json:"calls"`
	Errors       int64   `json:"errors"`
	Rate         float64 `json:"request_rate_rps"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
	duration     int64
}

type InstanceMetadata struct {
	Start          time.Time         `json:"start"`
	End            time.Time         `json:"end"`
	ResolutionS    int               `json:"resolution_seconds"`
	Coverage       string            `json:"coverage"`
	Note           string            `json:"note"`
	Truncated      bool              `json:"truncated"`
	AvailableNodes int               `json:"available_nodes"`
	ReadRows       int               `json:"read_rows"`
	PendingSpans   int               `json:"pending_parent_spans"`
	Dropped        uint64            `json:"process_dropped"`
	Unmatched      uint64            `json:"process_unmatched"`
	DropReasons    map[string]uint64 `json:"process_drop_reasons"`
	MissReasons    map[string]uint64 `json:"process_unmatched_reasons"`
	LastLoss       *time.Time        `json:"last_loss,omitempty"`
	LastFlush      time.Time         `json:"last_flush"`
	FlushFailed    bool              `json:"flush_failed"`
}

type InstanceSnapshot struct {
	Nodes []InstanceNode   `json:"nodes"`
	Edges []InstanceEdge   `json:"edges"`
	Meta  InstanceMetadata `json:"meta"`
}

type InstanceCatalog struct {
	Services     []InstanceNode `json:"services"`
	Environments []string       `json:"environments"`
	ServiceNames int            `json:"service_names"`
	Instances    int            `json:"instances"`
	Unresolved   int            `json:"unresolved"`
	Hosts        int            `json:"hosts"`
	Truncated    bool           `json:"truncated"`
}

func serviceID(row storage.TopologyEntity) string {
	return identityHash("service", row.TenantID, row.Environment, row.Namespace, row.Service)
}

func appendUnique(values []string, value string, cap int) []string {
	if value == "" {
		return values
	}
	for _, v := range values {
		if v == value {
			return values
		}
	}
	if len(values) < cap {
		values = append(values, value)
		sort.Strings(values)
	}
	return values
}

func graphNode(row storage.TopologyEntity, mode string) InstanceNode {
	n := InstanceNode{ID: row.ID, ServiceID: serviceID(row), Name: row.Service,
		Namespace: row.Namespace, Environment: row.Environment, Instance: row.Instance,
		Identity: row.Identity, Hosts: []string{}, Instances: []string{}}
	if mode == "services" {
		n.ID, n.Instance, n.Identity = n.ServiceID, "", "service"
	}
	return n
}

func addIdentity(n *InstanceNode, row storage.TopologyEntity) {
	n.Hosts = appendUnique(n.Hosts, row.Host, 20)
	if row.Identity == "instance_id" || row.Identity == "host_workload" {
		n.InstanceCount++
		n.Instances = appendUnique(n.Instances, row.ID, 20)
	} else {
		n.UnresolvedCount++
	}
}

// Catalog reads bounded identity metadata, not raw telemetry or aggregate
// history. It is safe to call independently of the expensive graph read.
func (g *InstanceGraph) Catalog(tenant string, start, end time.Time) InstanceCatalog {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := InstanceCatalog{Services: []InstanceNode{}, Environments: []string{}}
	groups := map[string]*InstanceNode{}
	envs, hosts, names := map[string]bool{}, map[string]bool{}, map[string]bool{}
	cutoff := g.now().Add(-g.retention).Unix()
	for _, e := range g.entities {
		r := e.row
		if r.TenantID != tenant || r.Source != "" || r.LastSeen < cutoff ||
			(!start.IsZero() && r.LastSeen < start.Unix()) || (!end.IsZero() && !time.Unix(r.FirstSeen, 0).Before(end)) {
			continue
		}
		n := graphNode(r, "services")
		if groups[n.ID] == nil {
			groups[n.ID] = &n
		}
		addIdentity(groups[n.ID], r)
		envs[r.Environment], names[identityHash(r.Namespace, r.Service)] = true, true
		if r.Host != "" {
			hosts[r.Host] = true
		}
		if r.Identity == "instance_id" || r.Identity == "host_workload" {
			out.Instances++
		} else {
			out.Unresolved++
		}
	}
	for _, n := range groups {
		out.Services = append(out.Services, *n)
	}
	sort.Slice(out.Services, func(i, j int) bool {
		a, b := out.Services[i], out.Services[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	if len(out.Services) > 1000 {
		out.Services, out.Truncated = out.Services[:1000], true
	}
	for env := range envs {
		out.Environments = append(out.Environments, env)
	}
	sort.Strings(out.Environments)
	out.ServiceNames, out.Hosts = len(names), len(hosts)
	return out
}

func (g *InstanceGraph) validateQuery(q *InstanceQuery) error {
	if q.Mode != "services" && q.Mode != "instances" {
		return errors.New("mode must be services or instances")
	}
	if q.Start.IsZero() || q.End.IsZero() || !q.End.After(q.Start) || q.End.Sub(q.Start) > 24*time.Hour {
		return errors.New("topology range must be positive and at most 24 hours")
	}
	if q.Limit < 1 || q.Limit > 100 || q.Hops < 1 || q.Hops > 2 ||
		len(q.ServiceID) > 64 || (q.Environment != nil && len(*q.Environment) > RegistryMaxValueBytes) {
		return errors.New("invalid topology scope or bounds")
	}
	q.Start = q.Start.Truncate(GraphWindow)
	q.End = q.End.Add(GraphWindow - time.Nanosecond).Truncate(GraphWindow)
	cutoff := g.now().Add(-g.retention).Add(GraphWindow - time.Nanosecond).Truncate(GraphWindow)
	if q.Start.Before(cutoff) {
		q.Start = cutoff
	}
	return nil
}

// Snapshot materializes at most 50,000 stored rows plus the bounded pending
// map. Scope is applied before storage reads, with at most 400 bind variables
// per query. The gate excludes a concurrent flush, preventing double counts
// between durable rows and pending values without blocking ingestion on I/O.
func (g *InstanceGraph) Snapshot(ctx context.Context, tenant string, q InstanceQuery) (InstanceSnapshot, error) {
	out := InstanceSnapshot{Nodes: []InstanceNode{}, Edges: []InstanceEdge{}}
	if err := g.validateQuery(&q); err != nil {
		return out, err
	}
	if err := g.acquire(ctx); err != nil {
		return out, err
	}
	defer func() { <-g.gate }()
	g.mu.Lock()
	entities := make(map[string]storage.TopologyEntity)
	for id, e := range g.entities {
		r := e.row
		if r.TenantID == tenant && r.LastSeen >= q.Start.Unix() && r.FirstSeen < q.End.Unix() &&
			(q.Environment == nil || r.Environment == *q.Environment) {
			entities[id] = r
		}
	}
	pending := make([]storage.TopologyBucket, 0, len(g.pending))
	for _, b := range g.pending {
		if b.WindowStart >= q.Start.Unix() && b.WindowStart < q.End.Unix() {
			pending = append(pending, b)
		}
	}
	meta := InstanceMetadata{Start: q.Start, End: q.End, ResolutionS: int(GraphWindow.Seconds()), Coverage: "observed",
		Note:    "Best-effort pre-sampling topology; five-minute buckets. Flushes every five seconds; a crash may lose the unflushed interval. Parent joins expire after five minutes and do not survive restart. Counters are deduplicated only within that correlation window. Host placement is the latest reported value.",
		Dropped: g.dropped, Unmatched: g.unmatched, LastFlush: g.lastFlush, FlushFailed: g.flushErr,
		DropReasons: maps.Clone(g.drops), MissReasons: maps.Clone(g.misses)}
	// Counters are process-lifetime; only a loss inside the requested range
	// limits this answer's coverage.
	lostInRange := !g.lastLoss.IsZero() && !g.lastLoss.Before(q.Start)
	if !g.lastLoss.IsZero() {
		last := g.lastLoss
		meta.LastLoss = &last
	}
	for _, children := range g.waiting {
		meta.PendingSpans += len(children)
	}
	g.mu.Unlock()
	if !q.End.After(q.Start) {
		out.Meta = meta
		return out, nil
	}
	// Environment filtering may exclude one end of a cross-environment call.
	// Never produce a dangling edge or relabel that endpoint.
	for id, r := range entities {
		if r.Source != "" && (entities[r.Source].ID == "" || entities[r.Target].ID == "") {
			delete(entities, id)
		}
	}
	if q.ServiceID != "" {
		groups := map[string]bool{q.ServiceID: true}
		for range q.Hops {
			next := make(map[string]bool, len(groups))
			for id := range groups {
				next[id] = true
			}
			for _, r := range entities {
				if r.Source == "" {
					continue
				}
				a, b := serviceID(entities[r.Source]), serviceID(entities[r.Target])
				if groups[a] || groups[b] {
					next[a], next[b] = true, true
				}
			}
			groups = next
		}
		for id, r := range entities {
			if r.Source == "" && !groups[serviceID(r)] {
				delete(entities, id)
			}
		}
		for id, r := range entities {
			if r.Source != "" && (entities[r.Source].ID == "" || entities[r.Target].ID == "") {
				delete(entities, id)
			}
		}
	}
	ids := make([]string, 0, len(entities))
	for id := range entities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	totals := make(map[string]storage.TopologyBucket)
	fold := func(b storage.TopologyBucket) {
		if _, ok := entities[b.EntityID]; !ok {
			return
		}
		t := totals[b.EntityID]
		t.Spans += b.Spans
		t.Requests += b.Requests
		t.Errors += b.Errors
		t.DurationMicros += b.DurationMicros
		totals[b.EntityID] = t
	}
	readCapped := false
	for offset := 0; offset < len(ids); offset += 400 {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		end := min(offset+400, len(ids))
		var rows []storage.TopologyBucket
		if err := g.db.WithContext(ctx).Where("entity_id IN ? AND window_start >= ? AND window_start < ?", ids[offset:end], q.Start.Unix(), q.End.Unix()).
			Order("entity_id, window_start").Limit(graphReadRows + 1 - meta.ReadRows).Find(&rows).Error; err != nil {
			return out, err
		}
		for _, b := range rows {
			meta.ReadRows++
			if meta.ReadRows > graphReadRows {
				meta.ReadRows, meta.Truncated, readCapped = graphReadRows, true, true
				break
			}
			fold(b)
		}
		if meta.Truncated {
			break
		}
	}
	for _, b := range pending {
		fold(b)
	}
	nodes := map[string]*InstanceNode{}
	instanceToNode := map[string]string{}
	for id, r := range entities {
		if r.Source != "" {
			continue
		}
		n := graphNode(r, q.Mode)
		if nodes[n.ID] == nil {
			nodes[n.ID] = &n
		}
		merged := nodes[n.ID]
		addIdentity(merged, r)
		t := totals[id]
		merged.Spans += t.Spans
		merged.Requests += t.Requests
		merged.Errors += t.Errors
		merged.duration += t.DurationMicros
		instanceToNode[id] = n.ID
	}
	seconds := q.End.Sub(q.Start).Seconds()
	for _, n := range nodes {
		n.Rate = float64(n.Requests) / seconds
		if n.Requests > 0 {
			n.AvgLatencyMS = float64(n.duration) / float64(n.Requests) / 1000
			n.ErrorRate = float64(n.Errors) / float64(n.Requests)
		}
		out.Nodes = append(out.Nodes, *n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		// The focus node must not be cut by a high-volume neighbor.
		a, b := out.Nodes[i], out.Nodes[j]
		if (a.ServiceID == q.ServiceID) != (b.ServiceID == q.ServiceID) {
			return a.ServiceID == q.ServiceID
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.ID < b.ID
	})
	meta.AvailableNodes = len(out.Nodes)
	if len(out.Nodes) > q.Limit {
		out.Nodes, meta.Truncated = out.Nodes[:q.Limit], true
	}
	visible := make(map[string]bool)
	for _, n := range out.Nodes {
		visible[n.ID] = true
	}
	edges := map[string]*InstanceEdge{}
	for id, r := range entities {
		if r.Source == "" || totals[id].Requests == 0 {
			continue
		}
		a, b := instanceToNode[r.Source], instanceToNode[r.Target]
		if !visible[a] || !visible[b] {
			continue
		}
		edgeID := identityHash("view-edge", a, b)
		if edges[edgeID] == nil {
			edges[edgeID] = &InstanceEdge{ID: edgeID, Source: a, Target: b}
		}
		e, t := edges[edgeID], totals[id]
		e.Calls += t.Requests
		e.Errors += t.Errors
		e.duration += t.DurationMicros
	}
	for _, e := range edges {
		e.Rate = float64(e.Calls) / seconds
		e.ErrorRate = float64(e.Errors) / float64(e.Calls)
		e.AvgLatencyMS = float64(e.duration) / float64(e.Calls) / 1000
		out.Edges = append(out.Edges, *e)
	}
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].Calls != out.Edges[j].Calls {
			return out.Edges[i].Calls > out.Edges[j].Calls
		}
		return out.Edges[i].ID < out.Edges[j].ID
	})
	if len(out.Edges) > 200 {
		out.Edges, meta.Truncated = out.Edges[:200], true
	}
	// Node and edge limits are presentation (meta.truncated); coverage is
	// limited only when observed data is missing from the totals.
	if readCapped || lostInRange || meta.FlushFailed {
		meta.Coverage = "limited"
	}
	out.Meta = meta
	return out, nil
}
