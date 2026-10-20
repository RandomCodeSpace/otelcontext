package aggregate

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// LogSummaryPoint counts all accepted logs in a five-minute bucket, including
// bodies that were not retained as raw exemplars.
type LogSummaryPoint struct {
	WindowStart time.Time `json:"window_start"`
	Count       int64     `json:"count"`
}
type LogSummaryResult struct {
	IdentityCoverage  IdentityCoverage  `json:"identity_coverage"`
	Points            []LogSummaryPoint `json:"points"`
	Coverage          Coverage          `json:"coverage"`
	Epoch             string            `json:"epoch"`
	Revision          uint64            `json:"revision"`
	ResolutionSeconds int               `json:"resolution_seconds"`
}

// QueryLogSummary uses the same ownership split as QueryBuckets so finalized
// history and mutable windows never double count. No raw-log scan is needed.
func (e *Engine) QueryLogSummary(ctx context.Context, q Query) (*LogSummaryResult, error) {
	if q.Start.IsZero() || !q.End.After(q.Start) || q.End.Sub(q.Start) > 7*24*time.Hour {
		return nil, fmt.Errorf("log summary requires a forward range of at most seven days")
	}
	q.Signal = SignalLog
	filter := serviceFilter(q.Services)
	counts := map[int64]uint64{}
	var identity IdentityCoverage
	matches := func(service string) bool {
		if filter == nil {
			return true
		}
		_, ok := filter[service]
		return ok
	}
	own, sel, hasStore, err := e.plan(ctx, q, func(window int64, key SeriesKey, d *AggregateDelta) {
		if identityMatchesFilter(e.serviceName(key), filter) && e.identityLimited(key) {
			identity.LogPoints += d.LogCount
		}
		if matches(e.serviceName(key)) {
			counts[window] += d.LogCount
		}
	})
	if err != nil {
		return nil, err
	}
	if hasStore {
		by := GroupByWindow | GroupBySignal
		if filter != nil {
			by |= GroupByService
		}
		sums, err := e.sumStore(ctx, sel, by)
		if err != nil {
			return nil, err
		}
		for _, row := range sums {
			if identityMatchesFilter(e.serviceNameByID(row.ServiceID), filter) {
				identity.LogPoints += row.IdentityCount
			}
			if filter == nil || matches(e.serviceNameByID(row.ServiceID)) {
				counts[row.WindowStart] += row.LogCount
			}
		}
	}
	points := make([]LogSummaryPoint, 0, len(counts))
	for window, count := range counts {
		points = append(points, LogSummaryPoint{WindowStart: time.Unix(window, 0).UTC(), Count: asInt64(count)})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].WindowStart.Before(points[j].WindowStart) })
	return &LogSummaryResult{Points: points, Coverage: identity.coverage(), IdentityCoverage: identity, Epoch: own.Epoch, Revision: own.Revision, ResolutionSeconds: int(WindowSize / time.Second)}, nil
}
