package storage

import (
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"
)

// dedupeSpansForUniqueIndex collapses any pre-existing duplicate spans on
// (tenant_id, trace_id, span_id) before AutoMigrate adds the composite
// uniqueIndex. Without this, CREATE UNIQUE INDEX would fail on databases
// that accumulated duplicates from earlier DLQ replays (or any pre-RAN-65
// non-idempotent ingest path), aborting startup.
//
// Strategy across drivers: keep the lowest primary-key row per
// (tenant_id, trace_id, span_id), delete the rest. Idempotent — a fresh
// database (or one that never collected duplicates) is a no-op.
//
// Must run BEFORE db.AutoMigrate(...) in AutoMigrateModels; once the
// unique constraint is in place, the dedupe is unnecessary because new
// inserts collapse via OnConflict.DoNothing.
func dedupeSpansForUniqueIndex(db *gorm.DB, driver string) error {
	if db == nil {
		return nil
	}
	driver = strings.ToLower(driver)

	// Skip if the spans table doesn't exist yet — fresh databases have
	// nothing to dedupe and the upcoming AutoMigrate will create the
	// table with the uniqueIndex already in place.
	if !db.Migrator().HasTable("spans") {
		return nil
	}

	// Skip if the unique index already exists (idempotent re-runs).
	if db.Migrator().HasIndex("spans", "idx_spans_tenant_trace_span") {
		return nil
	}

	var deleted int64
	switch driver {
	case "sqlite", "":
		// SQLite supports DELETE with subquery on same table.
		res := db.Exec(`DELETE FROM spans WHERE id NOT IN (
			SELECT MIN(id) FROM spans GROUP BY tenant_id, trace_id, span_id
		)`)
		if res.Error != nil {
			return fmt.Errorf("dedupe spans (sqlite): %w", res.Error)
		}
		deleted = res.RowsAffected

	case "postgres", "postgresql":
		// USING self-join keeps the lowest id per (tenant, trace, span).
		res := db.Exec(`DELETE FROM spans a USING spans b
			WHERE a.id > b.id
			  AND a.tenant_id = b.tenant_id
			  AND a.trace_id = b.trace_id
			  AND a.span_id = b.span_id`)
		if res.Error != nil {
			return fmt.Errorf("dedupe spans (postgres): %w", res.Error)
		}
		deleted = res.RowsAffected

	case "mysql":
		// MySQL forbids referencing the target table directly inside a
		// DELETE subquery; route through a temp table to keep the
		// "minimum id wins" semantics portable.
		if err := db.Exec(`CREATE TEMPORARY TABLE _spans_dedupe_keep AS
			SELECT MIN(id) AS id FROM spans GROUP BY tenant_id, trace_id, span_id`).Error; err != nil {
			return fmt.Errorf("dedupe spans (mysql temp table): %w", err)
		}
		defer db.Exec("DROP TEMPORARY TABLE IF EXISTS _spans_dedupe_keep")
		res := db.Exec(`DELETE FROM spans
			WHERE id NOT IN (SELECT id FROM _spans_dedupe_keep)`)
		if res.Error != nil {
			return fmt.Errorf("dedupe spans (mysql): %w", res.Error)
		}
		deleted = res.RowsAffected

	case "sqlserver", "mssql":
		// T-SQL: ROW_NUMBER() over the dedupe key, then delete duplicates.
		res := db.Exec(`WITH dups AS (
			SELECT id, ROW_NUMBER() OVER (
				PARTITION BY tenant_id, trace_id, span_id ORDER BY id
			) AS rn FROM spans
		) DELETE FROM spans WHERE id IN (SELECT id FROM dups WHERE rn > 1)`)
		if res.Error != nil {
			return fmt.Errorf("dedupe spans (mssql): %w", res.Error)
		}
		deleted = res.RowsAffected

	default:
		return nil
	}

	if deleted > 0 {
		log.Printf("🧹 Deduplicated %d duplicate span row(s) before adding uniqueIndex idx_spans_tenant_trace_span", deleted)
	}
	return nil
}
