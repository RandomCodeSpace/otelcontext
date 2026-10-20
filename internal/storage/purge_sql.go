package storage

import (
	"fmt"
	"strings"
)

// batchedDeleteSQL returns a DELETE that removes at most one batch of rows
// from table matching predicate. SQLite orders retention batches by age so an
// empty expired range uses the time index instead of scanning retained rows
// for an id-ordered LIMIT. Other drivers retain their id ordering.
// The statement takes the predicate's arguments followed by the batch size,
// in that order, on every driver.
//
// PostgreSQL and SQLite accept a LIMIT inside the id subquery. MySQL refuses a
// subquery that reads the table being deleted from (error 1093) unless it is
// wrapped in a derived table. SQL Server has no LIMIT and only allows ORDER BY
// in a subquery together with OFFSET/FETCH.
func batchedDeleteSQL(driver, table, predicate string) string {
	switch strings.ToLower(driver) {
	case "sqlite", "":
		order := "id"
		switch table {
		case "traces", "logs", "promql_samples":
			order = "timestamp, id"
		case "promql_series":
			order = "last_timestamp, id"
		case "spans":
			order = "start_time, id"
		}
		return fmt.Sprintf(
			"DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s ORDER BY %s LIMIT ?)",
			table, table, predicate, order,
		)
	case "mysql":
		return fmt.Sprintf(
			"DELETE FROM %s WHERE id IN (SELECT id FROM (SELECT id FROM %s WHERE %s ORDER BY id LIMIT ?) AS purge_batch)",
			table, table, predicate,
		)
	case "mssql", "sqlserver":
		return fmt.Sprintf(
			"DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s ORDER BY id OFFSET 0 ROWS FETCH NEXT ? ROWS ONLY)",
			table, table, predicate,
		)
	default:
		return fmt.Sprintf(
			"DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s ORDER BY id LIMIT ?)",
			table, table, predicate,
		)
	}
}

// Match the trace's tenant-scoped identity with an indexed lookup. A global
// NOT IN materializes every trace ID and treats another tenant's trace as a
// live parent. The age guard preserves spans whose trace is still in flight.
const expiredOrphanSpansPredicate = "start_time < ? AND NOT EXISTS (SELECT 1 FROM traces WHERE traces.tenant_id = spans.tenant_id AND traces.trace_id = spans.trace_id)"
