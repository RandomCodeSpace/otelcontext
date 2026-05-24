package graphrag

// GraphRAG persistence migrations.
//
// AutoMigrateGraphRAG runs the standard GORM AutoMigrate over the three
// persisted GraphRAG models, then performs two idempotent post-migration
// passes that prepare older databases for tenant-scoped reads:
//
//  1. backfillTenantIDs — sets tenant_id = DefaultTenantID on rows that pre-date
//     the column being added (or that somehow ended up empty). New rows always
//     receive the column default at insert time.
//
//  2. ensureDrainTemplatesCompositePK — promotes drain_templates' single-column
//     primary key (id) to the composite (tenant_id, id) when an older schema
//     is detected. The composite PK matters because the same Drain template
//     hash can legitimately recur across tenants once the in-memory miner is
//     partitioned per tenant; without it the second tenant's row would collide
//     on insert.
//
// Both passes are safe to call repeatedly. SQLite and PostgreSQL are
// supported explicitly; other dialects skip the PK promotion and log so an
// operator can apply the equivalent DDL by hand.

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

// graphRAGTables are the persisted tables that carry tenant_id after
// RAN-38. Order matches AutoMigrate order so log lines line up.
//
// `graph_snapshots` was dropped from the AutoMigrate slice on 2026-05-24;
// existing tables are left in place on operator databases (drop manually
// with `DROP TABLE graph_snapshots` to reclaim disk).
var graphRAGTables = []string{"investigations", "drain_templates"}

// AutoMigrateGraphRAG runs GORM auto-migration for GraphRAG models and
// applies tenant backfill + drain_templates composite-PK promotion. Safe to
// call repeatedly.
func AutoMigrateGraphRAG(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	if err := db.AutoMigrate(&Investigation{}, &DrainTemplateRow{}); err != nil {
		return fmt.Errorf("graphrag automigrate: %w", err)
	}
	if err := backfillTenantIDs(db); err != nil {
		return fmt.Errorf("graphrag tenant backfill: %w", err)
	}
	if err := ensureDrainTemplatesCompositePK(db); err != nil {
		// Non-fatal: new writes still carry tenant_id; only existing rows
		// retain the legacy single-column PK. Surfaced as a warn so an
		// operator can intervene on dialects we don't auto-migrate.
		slog.Warn("graphrag: drain_templates composite PK promotion skipped", "error", err)
	}
	return nil
}

// backfillTenantIDs sets tenant_id = DefaultTenantID for any row in the three
// GraphRAG tables that ended up with NULL or empty tenant_id. AutoMigrate
// already supplies a column default for new inserts; this pass covers the
// "added the column to a populated table" path on dialects that don't
// retroactively backfill via DEFAULT (and serves as belt-and-braces on those
// that do).
func backfillTenantIDs(db *gorm.DB) error {
	for _, tbl := range graphRAGTables {
		// Only touch tables that exist — fresh installs may race with a
		// half-migrated DB on first boot. HasTable is dialect-aware and
		// returns false rather than erroring on missing tables.
		if !db.Migrator().HasTable(tbl) {
			continue
		}
		sql := fmt.Sprintf(`UPDATE %s SET tenant_id = ? WHERE tenant_id IS NULL OR tenant_id = ''`, tbl) //nolint:gosec // table name is from a fixed allow-list
		if err := db.Exec(sql, storage.DefaultTenantID).Error; err != nil {
			return fmt.Errorf("backfill %s: %w", tbl, err)
		}
	}
	return nil
}

// ensureDrainTemplatesCompositePK promotes drain_templates' primary key from
// (id) to (tenant_id, id) on existing databases. Fresh installs already get
// the composite PK from AutoMigrate. On dialects this function does not
// understand it returns nil so AutoMigrateGraphRAG can move on.
func ensureDrainTemplatesCompositePK(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable("drain_templates") {
		return nil
	}
	switch db.Dialector.Name() {
	case "sqlite":
		return ensureDrainPKSQLite(db)
	case "postgres":
		return ensureDrainPKPostgres(db)
	default:
		// MySQL / MSSQL aren't covered by this ticket. Log so an operator
		// running those dialects can apply the equivalent DDL by hand.
		slog.Info("graphrag: drain_templates PK promotion skipped — unsupported dialect",
			"dialect", db.Dialector.Name())
		return nil
	}
}

// ensureDrainPKSQLite uses PRAGMA table_info to detect whether tenant_id is
// already part of the primary key. If not, it rebuilds the table via the
// canonical SQLite recipe (rename → create → copy → drop) inside a
// transaction. CreateTable here mirrors whatever GORM would build for a fresh
// install, so we don't have to hand-maintain a parallel CREATE TABLE.
func ensureDrainPKSQLite(db *gorm.DB) error {
	type pragmaCol struct {
		Cid       int    `gorm:"column:cid"`
		Name      string `gorm:"column:name"`
		Type      string `gorm:"column:type"`
		Notnull   int    `gorm:"column:notnull"`
		DfltValue any    `gorm:"column:dflt_value"`
		Pk        int    `gorm:"column:pk"`
	}
	var cols []pragmaCol
	if err := db.Raw(`PRAGMA table_info('drain_templates')`).Scan(&cols).Error; err != nil {
		return fmt.Errorf("pragma table_info: %w", err)
	}
	if len(cols) == 0 {
		// Table doesn't exist; AutoMigrate would have created it with the
		// composite PK already. Nothing to do.
		return nil
	}
	pkCols := map[string]bool{}
	for _, c := range cols {
		if c.Pk > 0 {
			pkCols[c.Name] = true
		}
	}
	if pkCols["tenant_id"] && pkCols["id"] {
		return nil // already composite
	}

	return db.Transaction(func(tx *gorm.DB) error {
		// Drop any leftover scratch from an interrupted earlier attempt.
		if err := tx.Exec(`DROP TABLE IF EXISTS drain_templates__legacy`).Error; err != nil {
			return fmt.Errorf("drop scratch: %w", err)
		}
		if err := tx.Exec(`ALTER TABLE drain_templates RENAME TO drain_templates__legacy`).Error; err != nil {
			return fmt.Errorf("rename legacy: %w", err)
		}
		// Recreate with the current GORM schema (composite PK + indexes).
		if err := tx.Migrator().CreateTable(&DrainTemplateRow{}); err != nil {
			return fmt.Errorf("create new table: %w", err)
		}
		// Copy rows over, defaulting tenant_id where the legacy table
		// didn't carry one (the column may have been added by AutoMigrate
		// before this pass ran).
		insert := `INSERT INTO drain_templates (tenant_id, id, tokens, count, first_seen, last_seen, sample)
SELECT COALESCE(NULLIF(tenant_id, ''), ?), id, tokens, count, first_seen, last_seen, sample
FROM drain_templates__legacy`
		if err := tx.Exec(insert, storage.DefaultTenantID).Error; err != nil {
			// Older schemas may not have the tenant_id column at all — fall
			// back to a tenant-less SELECT, defaulting every row.
			insertFallback := `INSERT INTO drain_templates (tenant_id, id, tokens, count, first_seen, last_seen, sample)
SELECT ?, id, tokens, count, first_seen, last_seen, sample
FROM drain_templates__legacy`
			if err2 := tx.Exec(insertFallback, storage.DefaultTenantID).Error; err2 != nil {
				return fmt.Errorf("copy rows: %w (fallback: %w)", err, err2)
			}
		}
		if err := tx.Exec(`DROP TABLE drain_templates__legacy`).Error; err != nil {
			return fmt.Errorf("drop legacy: %w", err)
		}
		slog.Info("graphrag: promoted drain_templates PK to (tenant_id, id) on SQLite")
		return nil
	})
}

// ensureDrainPKPostgres queries pg_index for the current PK column set and,
// when it doesn't already include tenant_id, drops the implicit
// drain_templates_pkey constraint and recreates it as a composite PK on
// (tenant_id, id). DROP/ADD runs inside one statement so the table is never
// without a PK between steps.
func ensureDrainPKPostgres(db *gorm.DB) error {
	var pkCols []string
	err := db.Raw(`
		SELECT a.attname::text
		FROM pg_index i
		JOIN pg_attribute a
		  ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = 'drain_templates'::regclass
		  AND i.indisprimary
	`).Scan(&pkCols).Error
	if err != nil {
		// Most likely the table doesn't exist yet — nothing to migrate.
		return nil
	}
	hasTenant, hasID := false, false
	for _, c := range pkCols {
		switch c {
		case "tenant_id":
			hasTenant = true
		case "id":
			hasID = true
		}
	}
	if hasTenant && hasID {
		return nil
	}
	if !hasID {
		// Defensive: an alien schema without `id` in the PK is not something
		// this migration should silently overwrite.
		return errors.New("drain_templates primary key has unexpected shape; manual migration required")
	}
	if err := db.Exec(`ALTER TABLE drain_templates DROP CONSTRAINT IF EXISTS drain_templates_pkey, ADD PRIMARY KEY (tenant_id, id)`).Error; err != nil {
		return fmt.Errorf("alter pk: %w", err)
	}
	slog.Info("graphrag: promoted drain_templates PK to (tenant_id, id) on PostgreSQL")
	return nil
}
