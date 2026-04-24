package storage

import (
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"
)

// dropLegacyTraceIDUniqueIndex removes any pre-RAN-21 standalone UNIQUE index
// that covers only traces.trace_id. From RAN-21 onward uniqueness is the
// composite idx_traces_tenant_trace_id on (tenant_id, trace_id); a surviving
// standalone unique index would silently block cross-tenant trace_id reuse.
//
// Discovery is structure-based (not name-based) because the legacy name varied
// across drivers and GORM versions. The composite index — which lists two
// columns — is never matched and therefore never dropped.
//
// Fresh databases never contain the legacy index, so this is a safe no-op on
// first boot. Invoked once per AutoMigrateModels call and is idempotent.
func dropLegacyTraceIDUniqueIndex(db *gorm.DB, driver string) error {
	if db == nil {
		return nil
	}
	driver = strings.ToLower(driver)
	names, err := findLegacyTraceIDUniqueIndexes(db, driver)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if err := dropIndexOnTraces(db, driver, name); err != nil {
			return fmt.Errorf("drop legacy trace_id unique index %q: %w", name, err)
		}
		log.Printf("🧹 Dropped legacy single-column unique index on traces.trace_id: %s", name)
	}
	return nil
}

// findLegacyTraceIDUniqueIndexes returns every UNIQUE index on the traces table
// whose single indexed column is trace_id. The composite RAN-21 index
// (tenant_id, trace_id) is excluded because it covers two columns.
func findLegacyTraceIDUniqueIndexes(db *gorm.DB, driver string) ([]string, error) {
	switch driver {
	case "sqlite", "":
		// Enumerate every unique index on traces, then inspect its column list
		// via PRAGMA index_info. SQLite auto-creates indexes for UNIQUE table
		// constraints with names prefixed "sqlite_autoindex_" — those also
		// surface here and are handled identically. Aliased to is_unique
		// because SQLite treats "unique" as a reserved keyword even as an
		// output alias.
		type idxRow struct {
			Name     string `gorm:"column:name"`
			IsUnique int    `gorm:"column:is_unique"`
		}
		var idxs []idxRow
		if err := db.Raw(`SELECT name, "unique" AS is_unique FROM pragma_index_list('traces')`).Scan(&idxs).Error; err != nil {
			return nil, fmt.Errorf("pragma_index_list(traces): %w", err)
		}
		type colRow struct {
			Name string `gorm:"column:name"`
		}
		var out []string
		for _, ix := range idxs {
			if ix.IsUnique != 1 {
				continue
			}
			var cols []colRow
			if err := db.Raw(fmt.Sprintf("SELECT name FROM pragma_index_info('%s')", ix.Name)).Scan(&cols).Error; err != nil {
				return nil, fmt.Errorf("pragma_index_info(%s): %w", ix.Name, err)
			}
			if len(cols) == 1 && cols[0].Name == "trace_id" {
				out = append(out, ix.Name)
			}
		}
		return out, nil

	case "postgres", "postgresql":
		// pg_index.indkey is an int2vector of attnums; join against
		// pg_attribute to resolve column names. Filter to UNIQUE, non-primary
		// indexes on the traces table covering exactly one column = trace_id.
		var rows []indexNameRow
		const q = `
SELECT c.relname AS name
FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
JOIN pg_class t ON t.oid = i.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
WHERE t.relname = 'traces'
  AND n.nspname = ANY (current_schemas(false))
  AND i.indisunique
  AND NOT i.indisprimary
  AND i.indnatts = 1
  AND (
    SELECT attname FROM pg_attribute
    WHERE attrelid = t.oid AND attnum = i.indkey[0]
  ) = 'trace_id'`
		if err := db.Raw(q).Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("pg_index lookup: %w", err)
		}
		return flattenIndexNames(rows), nil

	case "mysql":
		// information_schema.STATISTICS has one row per (index, seq_in_index).
		// Group by index, count columns, and keep indexes where the sole
		// column is trace_id and NON_UNIQUE=0.
		var rows []indexNameRow
		const q = `
SELECT INDEX_NAME AS name
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = DATABASE()
  AND TABLE_NAME   = 'traces'
  AND NON_UNIQUE   = 0
  AND INDEX_NAME  <> 'PRIMARY'
GROUP BY INDEX_NAME
HAVING COUNT(*) = 1
   AND MAX(COLUMN_NAME) = 'trace_id'`
		if err := db.Raw(q).Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("information_schema.STATISTICS lookup: %w", err)
		}
		return flattenIndexNames(rows), nil

	case "sqlserver", "mssql":
		// sys.indexes + sys.index_columns: one row per indexed column; keep
		// unique, non-primary-key indexes on dbo.traces whose only column is
		// trace_id.
		var rows []indexNameRow
		const q = `
SELECT i.name AS name
FROM sys.indexes i
JOIN sys.objects t ON t.object_id = i.object_id
WHERE t.name = 'traces'
  AND i.is_unique = 1
  AND i.is_primary_key = 0
  AND (
    SELECT COUNT(*) FROM sys.index_columns ic
    WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id
  ) = 1
  AND EXISTS (
    SELECT 1 FROM sys.index_columns ic
    JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
    WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id AND c.name = 'trace_id'
  )`
		if err := db.Raw(q).Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("sys.indexes lookup: %w", err)
		}
		return flattenIndexNames(rows), nil
	}
	return nil, nil
}

// indexNameRow is a single-column scan target shared by the non-SQLite branches.
// Keeping it package-level (rather than redeclared per-branch) lets
// flattenIndexNames project through a single concrete type.
type indexNameRow struct {
	Name string `gorm:"column:name"`
}

func flattenIndexNames(rows []indexNameRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// dropIndexOnTraces removes an index by name, per-driver. GORM's Migrator
// handles SQLite/Postgres/MySQL cleanly; for SQL Server we must qualify the
// DROP with the table name.
func dropIndexOnTraces(db *gorm.DB, driver, name string) error {
	switch driver {
	case "sqlserver", "mssql":
		// T-SQL: DROP INDEX name ON table
		return db.Exec(fmt.Sprintf("DROP INDEX [%s] ON [traces]", name)).Error
	default:
		return db.Migrator().DropIndex(&Trace{}, name)
	}
}
