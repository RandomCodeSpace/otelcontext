package storage

import (
	"strings"
	"testing"
	"time"
)

func TestBatchedDeleteSQL(t *testing.T) {
	cases := []struct {
		driver string
		want   string
	}{
		{"postgres", "DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp < ? ORDER BY id LIMIT ?)"},
		{"sqlite", "DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp < ? ORDER BY timestamp, id LIMIT ?)"},
		{"mysql", "DELETE FROM logs WHERE id IN (SELECT id FROM (SELECT id FROM logs WHERE timestamp < ? ORDER BY id LIMIT ?) AS purge_batch)"},
		{"mssql", "DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp < ? ORDER BY id OFFSET 0 ROWS FETCH NEXT ? ROWS ONLY)"},
		{"sqlserver", "DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp < ? ORDER BY id OFFSET 0 ROWS FETCH NEXT ? ROWS ONLY)"},
	}
	for _, c := range cases {
		if got := batchedDeleteSQL(c.driver, "logs", "timestamp < ?"); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.driver, got, c.want)
		}
	}
}

// The empty-expiry case must seek into the age index rather than scanning
// every retained row to satisfy an id-ordered LIMIT.
func TestBatchedDeleteSQL_SQLiteAgeIndexPlan(t *testing.T) {
	repo := newTestRepo(t)
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	for _, c := range []struct{ table, column, index string }{
		{"traces", "timestamp", "idx_traces_timestamp"},
		{"logs", "timestamp", "idx_logs_timestamp"},
		{"spans", "start_time", "idx_spans_start_time"},
	} {
		t.Run(c.table, func(t *testing.T) {
			var plan []struct{ Detail string }
			query := batchedDeleteSQL("sqlite", c.table, c.column+" < ?")
			if err := repo.db.Raw("EXPLAIN QUERY PLAN "+query, cutoff, 10).Scan(&plan).Error; err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range plan {
				if strings.Contains(row.Detail, "SCAN "+c.table) {
					t.Errorf("unbounded retention scan: %s", row.Detail)
				}
				if strings.Contains(row.Detail, "USE TEMP B-TREE") {
					t.Errorf("retention ordering requires a sort: %s", row.Detail)
				}
				if strings.Contains(row.Detail, c.index+" ("+c.column+"<?)") {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing age-index search: %+v", plan)
			}
		})
	}
}

func TestBatchedDeleteSQL_SQLiteOrphanSpanPlan(t *testing.T) {
	repo := newTestRepo(t)
	var plan []struct{ Detail string }
	query := batchedDeleteSQL("sqlite", "spans", expiredOrphanSpansPredicate)
	if err := repo.db.Raw("EXPLAIN QUERY PLAN "+query, time.Now().UTC(), 10).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	spans, parents := false, false
	for _, row := range plan {
		if strings.Contains(row.Detail, "SCAN spans") || strings.Contains(row.Detail, "SCAN traces") {
			t.Errorf("unbounded retention scan: %s", row.Detail)
		}
		if strings.Contains(row.Detail, "USE TEMP B-TREE") {
			t.Errorf("retention ordering requires a sort: %s", row.Detail)
		}
		spans = spans || strings.Contains(row.Detail, "idx_spans_start_time (start_time<?)")
		parents = parents || strings.Contains(row.Detail, "idx_traces_tenant_trace_id (tenant_id=? AND trace_id=?)")
	}
	if !spans || !parents {
		t.Fatalf("missing span range or parent identity index: %+v", plan)
	}
}
