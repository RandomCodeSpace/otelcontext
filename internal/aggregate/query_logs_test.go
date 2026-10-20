package aggregate

import (
	"context"
	"testing"
	"time"
)

func TestQueryLogSummaryOwnershipAndTenant(t *testing.T) {
	f := newQueryFixture(t)
	store := newStubStore()
	f.engine.SetStore(store)
	old := WindowStart(f.now.Add(-6 * 24 * time.Hour))
	key := f.logKey("api", "error <*>")
	store.put(1, key, old, &AggregateDelta{LogCount: 23})
	store.put(2, f.logKey("other", "info <*>"), old, &AggregateDelta{LogCount: 100})
	foreign := key
	foreign.TenantID = 999
	store.put(3, foreign, old, &AggregateDelta{LogCount: 1000})
	// A stale disk copy of a mutable bucket must not be included.
	store.put(1, key, f.window(), &AggregateDelta{LogCount: 500})
	f.apply(key, f.window(), &AggregateDelta{LogCount: 7})
	q := Query{Tenant: "default", Start: f.now.Add(-7*24*time.Hour + time.Minute), End: f.now, Services: []string{"api"}}
	result, err := f.engine.QueryLogSummary(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Points) != 2 || result.Points[0].Count != 23 || result.Points[1].Count != 7 || result.ResolutionSeconds != 300 {
		t.Fatalf("incorrect summary: %+v", result)
	}
	for _, r := range store.readRanges {
		if r[1] > f.window() {
			t.Fatalf("disk scan overlapped mutable history: %v", r)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.engine.QueryLogSummary(canceled, q); err == nil {
		t.Fatal("cancellation ignored")
	}
	q.Start = q.End.Add(-8 * 24 * time.Hour)
	if _, err := f.engine.QueryLogSummary(context.Background(), q); err == nil {
		t.Fatal("oversized query accepted")
	}
}

func TestQueryLogSummaryFromSQLiteHistory(t *testing.T) {
	f := newQueryFixture(t)
	store := newTestStore(t)
	f.engine.SetStore(store)
	old := WindowStart(f.now.Add(-6 * 24 * time.Hour))
	named := f.logKey("api", "known")
	overflow := f.logKey(OtherValue, "overflow")
	batch := &GroupBatch{
		Dicts: []DictRow{
			{ID: named.ServiceID, TenantID: f.tenantID, Kind: KindService, Value: []byte("api")},
			{ID: named.NameID, TenantID: f.tenantID, Kind: KindLogTemplate, Value: []byte("known")},
			{ID: overflow.ServiceID, TenantID: f.tenantID, Kind: KindService, Value: []byte(OtherValue)},
			{ID: overflow.NameID, TenantID: f.tenantID, Kind: KindLogTemplate, Value: []byte("overflow")},
		},
		Series: []SeriesRow{{ID: 1, Key: named}, {ID: 2, Key: overflow}},
		Deltas: []DeltaRow{
			{SeriesID: 1, WindowStart: old, Delta: &AggregateDelta{Count: 5, LogCount: 5}},
			{SeriesID: 2, WindowStart: old, Delta: &AggregateDelta{Count: 9, LogCount: 9}},
		},
	}
	if err := store.CommitGroup(batch); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeWindow(old); err != nil {
		t.Fatal(err)
	}
	q := Query{Tenant: "default", Start: f.now.Add(-7*24*time.Hour + time.Minute), End: f.now}
	for _, service := range []string{"", "api", OtherValue} {
		q.Services = nil
		if service != "" {
			q.Services = []string{service}
		}
		res, err := f.engine.QueryLogSummary(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if res.Coverage != CoverageLimited || res.IdentityCoverage.LogPoints != 9 {
			t.Fatalf("service=%q lost disk identity coverage: %+v", service, res)
		}
		want := int64(14)
		if service == "api" {
			want = 5
		}
		if service == OtherValue {
			want = 9
		}
		if len(res.Points) != 1 || res.Points[0].Count != want {
			t.Fatalf("service=%q points=%+v", service, res.Points)
		}
	}
}

func TestQueryLogSummaryCollapsedIdentityCoverage(t *testing.T) {
	f := newQueryFixture(t)
	f.apply(f.logKey("api", "known"), f.window(), &AggregateDelta{Count: 5, LogCount: 5})
	f.apply(f.logKey(OtherValue, "overflow"), f.window(), &AggregateDelta{Count: 9, LogCount: 9})
	q := f.rangeQuery()
	for _, service := range []string{"", "api", OtherValue} {
		q.Services = nil
		if service != "" {
			q.Services = []string{service}
		}
		res, err := f.engine.QueryLogSummary(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if res.Coverage != CoverageLimited || res.IdentityCoverage.LogPoints != 9 {
			t.Fatalf("service=%q coverage=%+v", service, res)
		}
		want := int64(14)
		if service == "api" {
			want = 5
		}
		if service == OtherValue {
			want = 9
		}
		if len(res.Points) != 1 || res.Points[0].Count != want {
			t.Fatalf("service=%q points=%+v", service, res.Points)
		}
	}
}
