package storage

import (
	"fmt"
	"strings"
)

// batchedDeleteSQL returns a DELETE that removes at most one batch of rows
// from table matching predicate, oldest id first. The statement takes the
// predicate's arguments followed by the batch size, in that order, on every
// driver.
//
// PostgreSQL and SQLite accept a LIMIT inside the id subquery. MySQL refuses a
// subquery that reads the table being deleted from (error 1093) unless it is
// wrapped in a derived table. SQL Server has no LIMIT and only allows ORDER BY
// in a subquery together with OFFSET/FETCH.
func batchedDeleteSQL(driver, table, predicate string) string {
	switch strings.ToLower(driver) {
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
