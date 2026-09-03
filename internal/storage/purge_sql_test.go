package storage

import "testing"

func TestBatchedDeleteSQL(t *testing.T) {
	cases := []struct {
		driver string
		want   string
	}{
		{"postgres", "DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp < ? ORDER BY id LIMIT ?)"},
		{"sqlite", "DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp < ? ORDER BY id LIMIT ?)"},
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
