package aggregate

// IdentityCoverage counts accepted points represented by an overflow identity.
// Counts remain exact without identity filters; a named query cannot recover
// the original names or dimensions of these points. Counts are scoped to the
// query range, or to retained windows for a topology projection.
type IdentityCoverage struct {
	TracePoints  uint64 `json:"trace_points"`
	EdgePoints   uint64 `json:"edge_points"`
	MetricPoints uint64 `json:"metric_points"`
	LogPoints    uint64 `json:"log_points"`
}

func (c IdentityCoverage) Limited() bool {
	return c.TracePoints != 0 || c.EdgePoints != 0 || c.MetricPoints != 0 || c.LogPoints != 0
}

func (c *IdentityCoverage) add(signal Signal, count uint64) {
	switch signal {
	case SignalTraceOp:
		c.TracePoints += count
	case SignalServiceEdge:
		c.EdgePoints += count
	case SignalMetric:
		c.MetricPoints += count
	case SignalLog:
		c.LogPoints += count
	}
}

func (c IdentityCoverage) coverage() Coverage {
	if c.Limited() {
		return CoverageLimited
	}
	return CoverageFull
}

func identityMatchesFilter(name string, filter map[string]struct{}) bool {
	if filter == nil || name == OtherValue {
		return true
	}
	_, ok := filter[name]
	return ok
}

// identityLimited is independent of the current limiter census, so it also
// detects overflow in retained history after restart.
func (e *Engine) identityLimited(key SeriesKey) bool {
	for _, id := range [...]uint32{key.ServiceID, key.NameID, key.DimsID} {
		if id != 0 && e.nameByID(id) == OtherValue {
			return true
		}
	}
	return false
}

// selectedServiceIDs narrows disk reads before decoding sketches. Include the
// unknown-service bucket to disclose potentially matching unnamed telemetry;
// the caller's ordinary filter still excludes it from the named result.
func (e *Engine) selectedServiceIDs(tenantID uint32, services []string) []uint32 {
	if len(services) == 0 {
		return nil
	}
	if _, ok := e.cache.reg.(identityFinder); !ok {
		return nil // write-only registrars retain the Go-side filter
	}
	ids := []uint32{0} // no known matches must not turn into an unfiltered read
	seen := make(map[uint32]struct{}, len(services)+1)
	for _, name := range append(append([]string(nil), services...), OtherValue) {
		id, ok := e.cache.FindID(tenantID, KindService, name)
		if !ok {
			continue
		}
		if _, exists := seen[id]; !exists {
			ids = append(ids, id)
			seen[id] = struct{}{}
		}
	}
	return ids
}
