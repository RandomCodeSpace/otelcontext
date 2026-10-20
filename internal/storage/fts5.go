package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"gorm.io/gorm"
)

// fts5LogsTable is the FTS5 virtual table mirroring `logs.body` and
// `logs.service_name`. It is an external-content table keyed on `logs.id` so it
// stores no extra copy of the body — instead, INSERT/DELETE/UPDATE on `logs`
// are mirrored via the triggers installed in setupSQLiteFTS5.
const fts5LogsTable = "logs_fts"

// sqliteLogFTSConnection accepts the migration runner's locked connection and
// GORM's transaction without giving either path its own copy of the FTS DDL.
type sqliteLogFTSConnection interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type sqliteLogFTSObject struct {
	name string
	kind string
	ddl  string
}

// Keep these definitions compatible with the existing AutoMigrate index.
// Porter provides stemming, unicode61 handles case and punctuation, and
// remove_diacritics handles accented text in the modernc SQLite build.
var sqliteLogFTSObjects = []sqliteLogFTSObject{
	{
		name: fts5LogsTable,
		kind: "table",
		ddl: `CREATE VIRTUAL TABLE IF NOT EXISTS ` + fts5LogsTable + ` USING fts5(
            body,
            service_name,
            content='logs',
            content_rowid='id',
            tokenize='porter unicode61 remove_diacritics 2'
        )`,
	},
	{
		name: "logs_ai",
		kind: "trigger",
		ddl: `CREATE TRIGGER IF NOT EXISTS logs_ai AFTER INSERT ON logs BEGIN
            INSERT INTO ` + fts5LogsTable + `(rowid, body, service_name) VALUES (new.id, new.body, new.service_name);
        END`,
	},
	{
		name: "logs_ad",
		kind: "trigger",
		ddl: `CREATE TRIGGER IF NOT EXISTS logs_ad AFTER DELETE ON logs BEGIN
            INSERT INTO ` + fts5LogsTable + `(` + fts5LogsTable + `, rowid, body, service_name) VALUES ('delete', old.id, old.body, old.service_name);
        END`,
	},
	{
		name: "logs_au",
		kind: "trigger",
		ddl: `CREATE TRIGGER IF NOT EXISTS logs_au AFTER UPDATE ON logs BEGIN
            INSERT INTO ` + fts5LogsTable + `(` + fts5LogsTable + `, rowid, body, service_name) VALUES ('delete', old.id, old.body, old.service_name);
            INSERT INTO ` + fts5LogsTable + `(rowid, body, service_name) VALUES (new.id, new.body, new.service_name);
        END`,
	},
}

// CheckSQLiteLogFTS verifies the optional log-search schema without DDL or a
// corpus scan. It does not perform a full index integrity check.
func CheckSQLiteLogFTS(ctx context.Context, db sqliteLogFTSConnection) error {
	missing, err := missingSQLiteLogFTSObjects(ctx, db)
	if err != nil {
		return err
	}
	if len(missing) != 0 {
		names := make([]string, len(missing))
		for i, object := range missing {
			names[i] = object.name
		}
		return fmt.Errorf("missing SQLite log FTS objects: %s", strings.Join(names, ", "))
	}
	return nil
}

// EnsureSQLiteLogFTS provisions or repairs the optional SQLite log index. The
// caller must supply a transaction covering this entire call, and commit only
// on success. The versioned migration runner supplies its BEGIN IMMEDIATE
// connection; AutoMigrate supplies a GORM transaction. Driver and configuration
// checks belong to the caller.
//
// Missing triggers mean writes may have escaped indexing, so any repair also
// rebuilds from logs. A complete existing index performs only a schema read.
func EnsureSQLiteLogFTS(ctx context.Context, tx sqliteLogFTSConnection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	missing, err := missingSQLiteLogFTSObjects(ctx, tx)
	if err != nil || len(missing) == 0 {
		return err
	}
	for _, object := range missing {
		if _, err := tx.ExecContext(ctx, object.ddl); err != nil {
			return fmt.Errorf("create SQLite log FTS %s: %w", object.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+fts5LogsTable+`(`+fts5LogsTable+`) VALUES ('rebuild')`); err != nil {
		return fmt.Errorf("rebuild SQLite log FTS index: %w", err)
	}
	// Confirm the postcondition before the caller commits. This also checks
	// cancellation if a short rebuild finished before SQLite saw its interrupt.
	return CheckSQLiteLogFTS(ctx, tx)
}

func missingSQLiteLogFTSObjects(ctx context.Context, db sqliteLogFTSConnection) ([]sqliteLogFTSObject, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, type, sql FROM sqlite_master
        WHERE name COLLATE NOCASE IN ('logs_fts', 'logs_ai', 'logs_ad', 'logs_au')`)
	if err != nil {
		return nil, fmt.Errorf("inspect SQLite log FTS schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	objects := make(map[string]sqliteLogFTSObject)
	for rows.Next() {
		var object sqliteLogFTSObject
		if err := rows.Scan(&object.name, &object.kind, &object.ddl); err != nil {
			return nil, fmt.Errorf("read SQLite log FTS schema: %w", err)
		}
		objects[strings.ToLower(object.name)] = object
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read SQLite log FTS schema: %w", err)
	}
	var missing []sqliteLogFTSObject
	for _, expected := range sqliteLogFTSObjects {
		actual, found := objects[expected.name]
		if !found {
			missing = append(missing, expected)
		} else if actual.kind != expected.kind || normalizeSQLiteFTSDDL(actual.ddl) != normalizeSQLiteFTSDDL(expected.ddl) {
			// Do not replace an unknown schema or claim its triggers keep the
			// index synchronized. An operator must inspect the conflicting object.
			return nil, fmt.Errorf("incompatible SQLite log FTS object %s; inspect its definition before retrying migration", expected.name)
		}
	}
	return missing, ctx.Err()
}

func normalizeSQLiteFTSDDL(ddl string) string {
	// SQLite omits IF NOT EXISTS from sqlite_master. Ignore formatting, while
	// comparing the known DDL instead of accepting arbitrary same-name objects.
	return strings.ReplaceAll(strings.Join(strings.Fields(strings.ToLower(ddl)), " "), "if not exists ", "")
}

func setupSQLiteFTS5(db *gorm.DB) error {
	if err := db.Transaction(func(tx *gorm.DB) error {
		return EnsureSQLiteLogFTS(tx.Statement.Context, tx.Statement.ConnPool)
	}); err != nil {
		return err
	}
	log.Println("🔎 SQLite: FTS5 BM25 index ready on logs(body, service_name)")
	return nil
}

// fts5MatchExpr translates a free-form user search string into an FTS5 MATCH
// expression that approximates the previous LIKE %query% semantics:
//
//   - whitespace-separated terms are ANDed together
//   - each term is double-quoted so FTS5 treats internal punctuation as
//     literal token separators rather than query operators
//   - each term is suffixed with `*` for prefix match, so a search for "conn"
//     still hits "connection"; combined with the porter stemmer this also
//     covers inflectional matches like "panic" → "panicked"
//
// Returns the empty string for empty/whitespace-only input — the caller is
// expected to skip the WHERE-clause attachment in that case.
func fts5MatchExpr(input string) string {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		escaped := strings.ReplaceAll(f, `"`, `""`)
		parts = append(parts, `"`+escaped+`"*`)
	}
	return strings.Join(parts, " ")
}

// fts5Available reports whether the given driver should use the FTS5 path.
// FTS5 is only enabled when (a) the driver is SQLite (Postgres has its own
// pg_trgm GIN path; MySQL/SQL Server are out of scope) and (b) LOG_FTS_ENABLED
// is enabled. Default on; FTS5's inverted index typically consumes 30-40% of
// SQLite DB disk for log-heavy workloads, and the LIKE fallback at
// log_repo.go:105 keeps search_logs functional without it.
func fts5Available(driver string) bool {
	if !strings.EqualFold(driver, "sqlite") {
		return false
	}
	return config.LogFTSEnabledFromEnv()
}

// DropLogsFTS removes the FTS5 virtual table and its sync triggers, then runs
// VACUUM to reclaim freed pages. Used by /api/admin/drop_fts on existing
// SQLite deployments after LOG_FTS_ENABLED has been set to false, to recover
// the 30-40% of DB disk the inverted index occupied.
//
// VACUUM blocks writes for ~10-60 minutes on a multi-GB DB and cannot run
// inside a transaction. Idempotent — safe to call when the FTS5 table or
// triggers are already absent.
func (r *Repository) DropLogsFTS(ctx context.Context) error {
	if !strings.EqualFold(r.driver, "sqlite") {
		return fmt.Errorf("DropLogsFTS only supported on SQLite, got driver=%q", r.driver)
	}
	db := r.db.WithContext(ctx)
	for _, name := range []string{"logs_au", "logs_ad", "logs_ai"} {
		if err := db.Exec("DROP TRIGGER IF EXISTS " + name).Error; err != nil {
			return fmt.Errorf("drop trigger %s: %w", name, err)
		}
	}
	if err := db.Exec("DROP TABLE IF EXISTS " + fts5LogsTable).Error; err != nil {
		return fmt.Errorf("drop table %s: %w", fts5LogsTable, err)
	}
	if err := db.Exec("VACUUM").Error; err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	slog.Info("FTS5 logs index dropped and DB vacuumed")
	return nil
}
