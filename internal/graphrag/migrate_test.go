package graphrag

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// newTestGraphRAGDB stands up an in-memory SQLite DB ready for the GraphRAG
// migrations. Local helper so the migrate tests don't depend on storage's
// _test-only fixtures.
func newTestGraphRAGDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

// newTestGraphRAGWithDB returns a GraphRAG wired to a real (in-memory SQLite)
// repo so tenant-scoped read tests can exercise the actual GORM query path.
// The returned *gorm.DB is the same handle the GraphRAG uses, suitable for
// seeding rows directly.
func newTestGraphRAGWithDB(t *testing.T) (*GraphRAG, *gorm.DB) {
	t.Helper()
	db := newTestGraphRAGDB(t)
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	g := New(repo, nil, nil, DefaultConfig())
	t.Cleanup(func() { g.Stop() })
	return g, db
}

// TestAutoMigrateGraphRAG_CreatesTenantCompositeIndexes asserts that the
// composite indexes declared on the three persisted GraphRAG models are
// actually materialised on SQLite. Mirrors
// storage.TestAutoMigrate_CreatesTenantCompositeIndexes for the GraphRAG side.
func TestAutoMigrateGraphRAG_CreatesTenantCompositeIndexes(t *testing.T) {
	db := newTestGraphRAGDB(t)
	if err := AutoMigrateGraphRAG(db); err != nil {
		t.Fatalf("AutoMigrateGraphRAG: %v", err)
	}
	expected := []struct {
		table string
		index string
	}{
		{"investigations", "idx_investigations_tenant_created"},
	}
	for _, tc := range expected {
		var count int
		if err := db.Raw(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name=? AND name=?",
			tc.table, tc.index,
		).Scan(&count).Error; err != nil {
			t.Fatalf("sqlite_master query for %s.%s: %v", tc.table, tc.index, err)
		}
		if count != 1 {
			t.Errorf("expected composite index %s on table %s (count=%d)", tc.index, tc.table, count)
		}
	}
}

// TestAutoMigrateGraphRAG_DrainTemplatesCompositePK asserts the PK on
// drain_templates is the composite (tenant_id, id) on a fresh install.
// Without this the same Drain template hash from two tenants would collide
// once a per-tenant Drain miner ships.
func TestAutoMigrateGraphRAG_DrainTemplatesCompositePK(t *testing.T) {
	db := newTestGraphRAGDB(t)
	if err := AutoMigrateGraphRAG(db); err != nil {
		t.Fatalf("AutoMigrateGraphRAG: %v", err)
	}
	type col struct {
		Name string `gorm:"column:name"`
		Pk   int    `gorm:"column:pk"`
	}
	var cols []col
	if err := db.Raw(`PRAGMA table_info('drain_templates')`).Scan(&cols).Error; err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	pk := map[string]bool{}
	for _, c := range cols {
		if c.Pk > 0 {
			pk[c.Name] = true
		}
	}
	if !pk["tenant_id"] || !pk["id"] {
		t.Fatalf("drain_templates PK should be composite (tenant_id, id); got %+v", pk)
	}
}

// TestAutoMigrateGraphRAG_IsIdempotent calls AutoMigrateGraphRAG repeatedly
// to assert the backfill UPDATEs and PK-promotion path are safe to re-run.
func TestAutoMigrateGraphRAG_IsIdempotent(t *testing.T) {
	db := newTestGraphRAGDB(t)
	for i := 0; i < 3; i++ {
		if err := AutoMigrateGraphRAG(db); err != nil {
			t.Fatalf("AutoMigrateGraphRAG pass %d: %v", i, err)
		}
	}
}

// TestAutoMigrateGraphRAG_BackfillsLegacyRows pre-populates the three
// GraphRAG tables with rows that have empty tenant_id and asserts the backfill
// pass fills DefaultTenantID on every one. Mimics the upgrade path for an
// existing install whose first boot brings in the tenant_id column.
func TestAutoMigrateGraphRAG_BackfillsLegacyRows(t *testing.T) {
	db := newTestGraphRAGDB(t)
	// Run once so the tables exist with the new schema.
	if err := AutoMigrateGraphRAG(db); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// Insert rows with empty tenant_id directly via raw SQL — Investigation and
	// DrainTemplateRow's GORM defaults would otherwise fill the column on
	// insert.
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO investigations (tenant_id, id, created_at, status, severity, trigger_service, trigger_operation, error_message, root_service, root_operation, causal_chain, trace_ids, error_logs, anomalous_metrics, affected_services, span_chain) VALUES ('', 'inv_legacy', ?, 'detected', 'warning', 'svc', 'op', 'boom', 'svc', 'op', '[]', '[]', '[]', '[]', '[]', '[]')`, now).Error; err != nil {
		t.Fatalf("seed legacy investigation: %v", err)
	}
	// Drain rows: tenant_id is part of the PK so we must give it *something*
	// — empty string is allowed by SQLite. The backfill is expected to fix it.
	if err := db.Exec(`INSERT INTO drain_templates (tenant_id, id, tokens, count, first_seen, last_seen, sample) VALUES ('', 1, '["a","b"]', 1, ?, ?, 'sample')`, now, now).Error; err != nil {
		t.Fatalf("seed legacy drain row: %v", err)
	}

	// Re-run migration; backfill should populate every empty tenant_id.
	if err := AutoMigrateGraphRAG(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	for _, tbl := range graphRAGTables {
		var stragglers int
		if err := db.Raw(`SELECT COUNT(*) FROM ` + tbl + ` WHERE tenant_id IS NULL OR tenant_id = ''`).Scan(&stragglers).Error; err != nil {
			t.Fatalf("count empty tenant in %s: %v", tbl, err)
		}
		if stragglers != 0 {
			t.Errorf("%s: %d rows still have empty tenant_id after backfill", tbl, stragglers)
		}
		var defaults int
		if err := db.Raw(`SELECT COUNT(*) FROM `+tbl+` WHERE tenant_id = ?`, storage.DefaultTenantID).Scan(&defaults).Error; err != nil {
			t.Fatalf("count default tenant in %s: %v", tbl, err)
		}
		if defaults < 1 {
			t.Errorf("%s: backfill produced no DefaultTenantID rows", tbl)
		}
	}
}

// TestSaveLoadDrainTemplates_TenantIsolation asserts that Save with one tenant
// and Load with another returns nothing — the per-tenant scoping is enforced
// at the DB layer, not just at the in-memory miner.
func TestSaveLoadDrainTemplates_TenantIsolation(t *testing.T) {
	db := newTestGraphRAGDB(t)
	if err := AutoMigrateGraphRAG(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tplsA := []Template{{ID: 0xA1, Tokens: []string{"a", "b"}, Count: 1, FirstSeen: time.Now(), LastSeen: time.Now()}}
	tplsB := []Template{{ID: 0xB2, Tokens: []string{"c", "d"}, Count: 1, FirstSeen: time.Now(), LastSeen: time.Now()}}
	// Identical hash across tenants is the interesting case — confirms the
	// composite PK lets the same template ID coexist in two tenants.
	tplsBSameID := []Template{{ID: 0xA1, Tokens: []string{"x", "y"}, Count: 1, FirstSeen: time.Now(), LastSeen: time.Now()}}

	if err := SaveDrainTemplates(db, "tenant-a", tplsA); err != nil {
		t.Fatalf("save tenant-a: %v", err)
	}
	if err := SaveDrainTemplates(db, "tenant-b", tplsB); err != nil {
		t.Fatalf("save tenant-b: %v", err)
	}
	if err := SaveDrainTemplates(db, "tenant-b", tplsBSameID); err != nil {
		t.Fatalf("save tenant-b colliding id: %v", err)
	}

	loadedA, err := LoadDrainTemplates(db, "tenant-a")
	if err != nil {
		t.Fatalf("load tenant-a: %v", err)
	}
	if len(loadedA) != 1 || loadedA[0].ID != 0xA1 || loadedA[0].Tokens[0] != "a" {
		t.Errorf("tenant-a load mismatch: %+v", loadedA)
	}

	loadedB, err := LoadDrainTemplates(db, "tenant-b")
	if err != nil {
		t.Fatalf("load tenant-b: %v", err)
	}
	if len(loadedB) != 2 {
		t.Errorf("tenant-b should have two templates (distinct + colliding-id); got %d", len(loadedB))
	}

	loadedC, err := LoadDrainTemplates(db, "tenant-empty")
	if err != nil {
		t.Fatalf("load tenant-empty: %v", err)
	}
	if len(loadedC) != 0 {
		t.Errorf("tenant-empty should have no templates; got %d", len(loadedC))
	}
}

// TestGraphRAG_GetInvestigations_TenantScoped seeds investigations under two
// tenants and asserts each tenant only sees its own rows via ctx scoping.
func TestGraphRAG_GetInvestigations_TenantScoped(t *testing.T) {
	g, db := newTestGraphRAGWithDB(t)
	if err := AutoMigrateGraphRAG(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now()
	rows := []Investigation{
		{TenantID: "acme", ID: "inv-acme-1", CreatedAt: now, Status: "detected", Severity: "critical", TriggerService: "orders"},
		{TenantID: "acme", ID: "inv-acme-2", CreatedAt: now.Add(time.Second), Status: "detected", Severity: "warning", TriggerService: "payments"},
		{TenantID: "globex", ID: "inv-globex-1", CreatedAt: now, Status: "detected", Severity: "critical", TriggerService: "orders"},
	}
	for _, r := range rows {
		if err := db.Create(&r).Error; err != nil {
			t.Fatalf("seed %s: %v", r.ID, err)
		}
	}

	acmeCtx := storage.WithTenantContext(context.Background(), "acme")
	globexCtx := storage.WithTenantContext(context.Background(), "globex")

	acme, err := g.GetInvestigations(acmeCtx, "", "", "", 100)
	if err != nil {
		t.Fatalf("acme list: %v", err)
	}
	if len(acme) != 2 {
		t.Errorf("acme should see 2 rows; got %d", len(acme))
	}
	for _, r := range acme {
		if r.TenantID != "acme" {
			t.Errorf("acme list leaked %s row: %s", r.TenantID, r.ID)
		}
	}

	globex, err := g.GetInvestigations(globexCtx, "", "", "", 100)
	if err != nil {
		t.Fatalf("globex list: %v", err)
	}
	if len(globex) != 1 || globex[0].ID != "inv-globex-1" {
		t.Errorf("globex should see only inv-globex-1; got %+v", globex)
	}

	// Cross-tenant ID lookup must miss — id-guessing should not leak.
	if _, err := g.GetInvestigation(acmeCtx, "inv-globex-1"); err == nil {
		t.Errorf("acme ctx should NOT find globex investigation")
	}
	got, err := g.GetInvestigation(globexCtx, "inv-globex-1")
	if err != nil {
		t.Fatalf("globex own lookup: %v", err)
	}
	if got.TenantID != "globex" {
		t.Errorf("expected globex row; got tenant=%q", got.TenantID)
	}
}
