package storage

import (
	"testing"
	"time"
)

// upgradeTrace builds a trace row for the upgrade tests.
func upgradeTrace(status, service string, ts time.Time, duration int64) Trace {
	return Trace{
		TenantID:    DefaultTenantID,
		TraceID:     "abc123",
		ServiceName: service,
		Duration:    duration,
		Status:      status,
		Timestamp:   ts,
	}
}

// loadUpgradeTraces returns every persisted row for the fixed trace ID.
func loadUpgradeTraces(t *testing.T, repo *Repository) []Trace {
	t.Helper()
	var traces []Trace
	if err := repo.db.Where("trace_id = ?", "abc123").Find(&traces).Error; err != nil {
		t.Fatalf("query traces: %v", err)
	}
	return traces
}

func TestBatchCreateTraces_ErrorUpgradesPersistedStatus(t *testing.T) {
	repo := newTestRepo(t)
	first := time.Now().UTC().Truncate(time.Second)

	if err := repo.BatchCreateTraces([]Trace{upgradeTrace("STATUS_CODE_UNSET", "checkout", first, 4000)}); err != nil {
		t.Fatalf("BatchCreateTraces(unset): %v", err)
	}
	if err := repo.BatchCreateTraces([]Trace{upgradeTrace(StatusCodeError, "payments", first.Add(time.Second), 900)}); err != nil {
		t.Fatalf("BatchCreateTraces(error): %v", err)
	}

	traces := loadUpgradeTraces(t, repo)
	if len(traces) != 1 {
		t.Fatalf("trace rows = %d, want 1", len(traces))
	}
	if traces[0].Status != StatusCodeError {
		t.Fatalf("status = %q, want %q", traces[0].Status, StatusCodeError)
	}
	// Only status upgrades; the first writer still owns the rest of the row.
	if traces[0].ServiceName != "checkout" {
		t.Fatalf("service = %q, want checkout", traces[0].ServiceName)
	}
	if traces[0].Duration != 4000 {
		t.Fatalf("duration = %d, want 4000", traces[0].Duration)
	}
	if !traces[0].Timestamp.Equal(first) {
		t.Fatalf("timestamp = %v, want %v", traces[0].Timestamp, first)
	}
}

func TestBatchCreateTraces_NeverDowngradesError(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()

	if err := repo.BatchCreateTraces([]Trace{upgradeTrace(StatusCodeError, "checkout", now, 4000)}); err != nil {
		t.Fatalf("BatchCreateTraces(error): %v", err)
	}
	for _, status := range []string{"STATUS_CODE_UNSET", "STATUS_CODE_OK"} {
		if err := repo.BatchCreateTraces([]Trace{upgradeTrace(status, "payments", now, 10)}); err != nil {
			t.Fatalf("BatchCreateTraces(%s): %v", status, err)
		}
		traces := loadUpgradeTraces(t, repo)
		if len(traces) != 1 {
			t.Fatalf("trace rows = %d, want 1", len(traces))
		}
		if traces[0].Status != StatusCodeError {
			t.Fatalf("status = %q after %s write, want it to stay %q", traces[0].Status, status, StatusCodeError)
		}
	}
}

// TestBatchCreateTraces_InBatchDuplicates_ErrorWins covers the in-batch dedup:
// duplicate (tenant, trace) rows collapse to one and an ERROR anywhere in the
// batch wins, regardless of ordering.
func TestBatchCreateTraces_InBatchDuplicates_ErrorWins(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string][]Trace{
		"error_last": {
			upgradeTrace("STATUS_CODE_UNSET", "checkout", now, 4000),
			upgradeTrace("STATUS_CODE_OK", "checkout", now, 4000),
			upgradeTrace(StatusCodeError, "payments", now, 900),
		},
		"error_first": {
			upgradeTrace(StatusCodeError, "payments", now, 900),
			upgradeTrace("STATUS_CODE_OK", "checkout", now, 4000),
		},
	}
	for name, batch := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newTestRepo(t)
			if err := repo.BatchCreateTraces(batch); err != nil {
				t.Fatalf("BatchCreateTraces: %v", err)
			}
			traces := loadUpgradeTraces(t, repo)
			if len(traces) != 1 {
				t.Fatalf("trace rows = %d, want 1", len(traces))
			}
			if traces[0].Status != StatusCodeError {
				t.Fatalf("status = %q, want %q", traces[0].Status, StatusCodeError)
			}
		})
	}
}

func TestCreateTrace_UpgradeOnly(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()

	if err := repo.CreateTrace(upgradeTrace("STATUS_CODE_OK", "checkout", now, 4000)); err != nil {
		t.Fatalf("CreateTrace(ok): %v", err)
	}
	if err := repo.CreateTrace(upgradeTrace(StatusCodeError, "payments", now, 10)); err != nil {
		t.Fatalf("CreateTrace(error): %v", err)
	}
	if err := repo.CreateTrace(upgradeTrace("STATUS_CODE_OK", "payments", now, 10)); err != nil {
		t.Fatalf("CreateTrace(ok again): %v", err)
	}

	traces := loadUpgradeTraces(t, repo)
	if len(traces) != 1 {
		t.Fatalf("trace rows = %d, want 1", len(traces))
	}
	if traces[0].Status != StatusCodeError {
		t.Fatalf("status = %q, want %q", traces[0].Status, StatusCodeError)
	}
	if traces[0].ServiceName != "checkout" {
		t.Fatalf("service = %q, want checkout", traces[0].ServiceName)
	}
}

// TestBatchCreateAll_UpgradesTraceStatus exercises the transactional path used
// by the async ingest pipeline.
func TestBatchCreateAll_UpgradesTraceStatus(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()

	if err := repo.BatchCreateAll([]Trace{upgradeTrace("STATUS_CODE_UNSET", "checkout", now, 4000)}, nil, nil); err != nil {
		t.Fatalf("BatchCreateAll(unset): %v", err)
	}
	if err := repo.BatchCreateAll([]Trace{upgradeTrace(StatusCodeError, "payments", now, 900)}, nil, nil); err != nil {
		t.Fatalf("BatchCreateAll(error): %v", err)
	}

	traces := loadUpgradeTraces(t, repo)
	if len(traces) != 1 {
		t.Fatalf("trace rows = %d, want 1", len(traces))
	}
	if traces[0].Status != StatusCodeError {
		t.Fatalf("status = %q, want %q", traces[0].Status, StatusCodeError)
	}
}

// TestSplitTracesByStatus_CrossTenantIsolation asserts the dedup key includes
// the tenant, so the same trace ID under two tenants stays two rows.
func TestSplitTracesByStatus_CrossTenantIsolation(t *testing.T) {
	acme := upgradeTrace(StatusCodeError, "checkout", time.Now().UTC(), 10)
	acme.TenantID = "acme"
	beta := upgradeTrace(StatusCodeError, "checkout", time.Now().UTC(), 10)
	beta.TenantID = "beta"

	healthy, errored := splitTracesByStatus([]Trace{acme, beta})
	if len(healthy) != 0 {
		t.Fatalf("healthy = %d, want 0", len(healthy))
	}
	if len(errored) != 2 {
		t.Fatalf("errored = %d, want 2 (per-tenant rows must not collapse)", len(errored))
	}
}
