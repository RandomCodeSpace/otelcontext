package graphrag

// Log clustering is performed by the Drain template miner (see drain.go).
// processLog() in builder.go calls GraphRAG.clusterLog() which delegates to
// the shared *Drain instance.

import (
	"fmt"
	"time"
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
