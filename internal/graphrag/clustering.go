package graphrag

// Log clustering is handled inline in processLog() via simpleHash.
// The vectordb.Index (TF-IDF engine) provides the actual similarity search.
// LogClusterNodes in SignalStore group similar error logs by their hash.
//
// For more sophisticated clustering (cosine > 0.85), callers can use
// vectorIdx.Search() to find similar logs and correlate with LogClusterNodes.

// SimilarErrors finds log clusters similar to a given cluster using the vector index.
func (g *GraphRAG) SimilarErrors(clusterID string, k int) []LogClusterNode {
	if k <= 0 {
		k = 10
	}

	g.SignalStore.mu.RLock()
	cluster, ok := g.SignalStore.LogClusters[clusterID]
	g.SignalStore.mu.RUnlock()
	if !ok {
		return nil
	}

	// Use vectordb to find similar logs
	if g.vectorIdx == nil {
		return nil
	}
	results := g.vectorIdx.Search(cluster.Template, k*2) // over-fetch to filter

	// Map results back to log clusters
	seen := map[string]bool{clusterID: true}
	var similar []LogClusterNode

	g.SignalStore.mu.RLock()
	defer g.SignalStore.mu.RUnlock()

	for _, r := range results {
		// Find cluster for this log's service
		for _, lc := range g.SignalStore.LogClusters {
			if seen[lc.ID] {
				continue
			}
			// Check if this cluster matches the search result's service
			for _, e := range g.SignalStore.Edges {
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
