package topology

import (
	"context"
	"sort"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// Host projection bounds (#288). The registry already bounds hosts and
// service-host pairs per tenant; these bound what one response carries.
const (
	// MaxHostsPerNode caps the hosts list stamped on one service-map node.
	MaxHostsPerNode = 20
	// MaxServicesPerHost caps the services list carried by one host.
	MaxServicesPerHost = 100
)

// Node kinds. A host entity (IsHostEntity) is rendered as a host by every
// consumer: it is never a service, never carries a CALLS edge and never
// enters a service count.
const (
	KindService = "service"
	KindHost    = "host"
)

// NodeKind returns the kind of the named node.
func NodeKind(service string) string {
	if IsHostEntity(service) {
		return KindHost
	}
	return KindService
}

// Names renders the signal bits as sorted names.
func (s Signal) Names() []string {
	out := []string{}
	if s&SignalLogs != 0 {
		out = append(out, "logs")
	}
	if s&SignalMetrics != 0 {
		out = append(out, "metrics")
	}
	if s&SignalTraces != 0 {
		out = append(out, "traces")
	}
	return out
}

// Host is one host with the services observed on it. Services is sorted,
// never nil, capped at MaxServicesPerHost and never lists a host entity;
// ServiceCount is the uncapped total.
type Host struct {
	Name         string    `json:"name"`
	ServiceCount int       `json:"service_count"`
	Services     []string  `json:"services"`
	LastSeen     time.Time `json:"last_seen"`
	Signals      []string  `json:"signals"`
}

// HostProjection is one tenant's host view derived from one registry
// snapshot: hosts with their services, and per service its hosts.
type HostProjection struct {
	// Hosts is sorted by name and never nil.
	Hosts     []Host
	byService map[string][]string
}

// Host returns the named host.
func (p HostProjection) Host(name string) (Host, bool) {
	i := sort.Search(len(p.Hosts), func(i int) bool { return p.Hosts[i].Name >= name })
	if i < len(p.Hosts) && p.Hosts[i].Name == name {
		return p.Hosts[i], true
	}
	return Host{}, false
}

// ServiceHosts returns how many hosts service was observed on and the first
// MaxHostsPerNode of them, sorted.
func (p HostProjection) ServiceHosts(service string) (int, []string) {
	all := p.byService[service]
	n := min(len(all), MaxHostsPerNode)
	return len(all), append(make([]string, 0, n), all[:n]...)
}

// ProjectHosts folds registry entries of one tenant into a HostProjection.
// Entries without a host contribute nothing.
func ProjectHosts(entries []ResourceEntry) HostProjection {
	type hostAcc struct {
		host     Host
		signals  Signal
		services map[string]struct{}
	}
	hosts := make(map[string]*hostAcc)
	byService := make(map[string]map[string]struct{})
	for _, e := range entries {
		if e.Host == "" {
			continue
		}
		acc := hosts[e.Host]
		if acc == nil {
			acc = &hostAcc{host: Host{Name: e.Host}, services: make(map[string]struct{})}
			hosts[e.Host] = acc
		}
		acc.signals |= e.Signals
		if e.LastSeen.After(acc.host.LastSeen) {
			acc.host.LastSeen = e.LastSeen
		}
		if byService[e.Service] == nil {
			byService[e.Service] = make(map[string]struct{})
		}
		byService[e.Service][e.Host] = struct{}{}
		if !IsHostEntity(e.Service) {
			acc.services[e.Service] = struct{}{}
		}
	}
	out := HostProjection{Hosts: make([]Host, 0, len(hosts)), byService: make(map[string][]string, len(byService))}
	for _, acc := range hosts {
		services := sortedKeys(acc.services)
		acc.host.ServiceCount = len(services)
		acc.host.Services = services[:min(len(services), MaxServicesPerHost)]
		acc.host.Signals = acc.signals.Names()
		out.Hosts = append(out.Hosts, acc.host)
	}
	sort.Slice(out.Hosts, func(i, j int) bool { return out.Hosts[i].Name < out.Hosts[j].Name })
	for service, set := range byService {
		out.byService[service] = sortedKeys(set)
	}
	return out
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HostReader is the single reader of the resource registry for every
// topology consumer. Both providers embed it, so legacy, shadow and
// aggregate answer host questions identically. Without a registry it
// projects nothing.
type HostReader struct {
	registry *Registry
}

// SetRegistry wires the registry read by Hosts.
func (h *HostReader) SetRegistry(r *Registry) { h.registry = r }

// Hosts projects the registry for the tenant on ctx. It is built per call:
// the registry snapshot is already sorted and bounded per tenant.
func (h *HostReader) Hosts(ctx context.Context) HostProjection {
	if h == nil || h.registry == nil {
		return ProjectHosts(nil)
	}
	return ProjectHosts(h.registry.TenantSnapshot(storage.TenantFromContext(ctx)))
}

// stampHosts annotates every node with its kind and hosts and drops any edge
// touching a host entity.
func (h *HostReader) stampHosts(ctx context.Context, snap *Snapshot) {
	hosts := h.Hosts(ctx)
	for i := range snap.Nodes {
		node := &snap.Nodes[i]
		node.Kind = NodeKind(node.Name)
		node.HostCount, node.Hosts = hosts.ServiceHosts(node.Name)
	}
	edges := snap.Edges[:0]
	for _, edge := range snap.Edges {
		if IsHostEntity(edge.Source) || IsHostEntity(edge.Target) {
			continue
		}
		edges = append(edges, edge)
	}
	snap.Edges = edges
}
