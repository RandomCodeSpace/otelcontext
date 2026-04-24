package graphrag

// Log clustering is now performed by the Drain template miner (see drain.go).
// processLog() in builder.go calls GraphRAG.clusterLog() which delegates to
// the shared *Drain instance. The vectordb.Index (TF-IDF) is still used for
// SimilarErrors — similarity search across mined templates.

import (
	"context"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// clusterLog runs the log body through Drain and upserts a LogClusterNode
// into the supplied tenant's SignalStore. Returns the service-scoped cluster ID.
//
// The cluster ID is service-scoped and derived from the Drain template ID,
// so it remains stable for any future ingestion of the same template shape.
// Drain's internal template ID may change when tokens generalize to
// wildcards; the LogClusterNode's TemplateID field is updated to track this.
//
// The Drain miner itself is shared across tenants — its templates describe
// log shape, not content, and the per-tenant SignalStore keeps the actual
// cluster nodes isolated.
func (g *GraphRAG) clusterLog(stores *tenantStores, service, body, severity string, ts time.Time) string {
	if g.drain == nil {
		// Fallback: legacy hash-based clustering.
		clusterID := fmt.Sprintf("lc_%s_%x", service, simpleHash(body))
		stores.signals.UpsertLogCluster(clusterID, body, severity, service, ts)
		return clusterID
	}

	tpl := g.drain.Match(body, ts)
	if tpl == nil {
		return ""
	}
	// Service-scoped stable cluster ID derived from the first-seen template
	// tokens. We use the current template ID as a deterministic suffix; when
	// Drain merges (tokens generalize), the ID may shift — acceptable since
	// it occurs only on the first few ingestions of a pattern.
	clusterID := fmt.Sprintf("lc_%s_%x", service, tpl.ID)
	stores.signals.UpsertLogClusterWithTemplate(
		clusterID,
		tpl.TemplateString(),
		severity,
		service,
		tpl.ID,
		tpl.Tokens,
		tpl.Sample,
		ts,
	)
	return clusterID
}

// SimilarErrors finds log clusters similar to a given cluster using the vector
// index, scoped to the tenant carried on ctx. Cross-tenant hits are impossible
// because the underlying vectordb partitions docs per tenant and this lookup
// resolves the SignalStore through storesFor(ctx).
func (g *GraphRAG) SimilarErrors(ctx context.Context, clusterID string, k int) []LogClusterNode {
	if k <= 0 {
		k = 10
	}

	stores := g.storesFor(ctx)

	stores.signals.mu.RLock()
	cluster, ok := stores.signals.LogClusters[clusterID]
	stores.signals.mu.RUnlock()
	if !ok {
		return nil
	}

	// Use vectordb to find similar logs based on the mined template.
	if g.vectorIdx == nil {
		return nil
	}
	query := cluster.Template
	if query == "" && len(cluster.TemplateTokens) > 0 {
		query = joinTokens(cluster.TemplateTokens)
	}
	// vectordb.Index.Search takes the tenant string directly; we resolve it
	// from ctx via the same storage helper used by storesFor so both sides
	// agree on coercion rules (empty → DefaultTenantID).
	tenant := storage.TenantFromContext(ctx)
	results := g.vectorIdx.Search(tenant, query, k*2) // over-fetch to filter

	// Map results back to log clusters.
	seen := map[string]bool{clusterID: true}
	var similar []LogClusterNode

	stores.signals.mu.RLock()
	defer stores.signals.mu.RUnlock()

	for _, r := range results {
		for _, lc := range stores.signals.LogClusters {
			if seen[lc.ID] {
				continue
			}
			for _, e := range stores.signals.Edges {
				if e.Type == EdgeEmittedBy && e.FromID == lc.ID && e.ToID == r.ServiceName {
					seen[lc.ID] = true
					similar = append(similar, *lc)
					break
				}
			}
			if len(similar) >= k {
				break
			}
		}
		if len(similar) >= k {
			break
		}
	}

	return similar
}

// joinTokens is a tiny helper to avoid importing strings in this file's
// hot path; equivalent to strings.Join(tokens, " ").
func joinTokens(tokens []string) string {
	n := 0
	for _, t := range tokens {
		n += len(t) + 1
	}
	b := make([]byte, 0, n)
	for i, t := range tokens {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, t...)
	}
	return string(b)
}
