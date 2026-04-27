package storage

import (
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"
)

// fts5LogsTable is the FTS5 virtual table mirroring `logs.body` and
// `logs.service_name`. It is an external-content table keyed on `logs.id` so it
// stores no extra copy of the body — instead, INSERT/DELETE/UPDATE on `logs`
// are mirrored via the triggers installed in setupSQLiteFTS5.
const fts5LogsTable = "logs_fts"

// setupSQLiteFTS5 provisions the FTS5 virtual table for log search on SQLite
// and the AFTER INSERT/DELETE/UPDATE triggers that keep it in sync with the
// `logs` base table. The implementation is idempotent: it tolerates an
// existing virtual table left over from a previous boot, repairs missing
// triggers, and runs an initial backfill via the `rebuild` command so that
// rows present in `logs` before the FTS table existed (e.g. migrating an
// older OtelContext.db) are included in the BM25 index.
//
// Tokenizer rationale: `porter unicode61 remove_diacritics 2` chosen for:
//   - unicode61: case-insensitive, splits on whitespace+punctuation
//   - remove_diacritics 2: strips accents (latency vs latência both match)
//   - porter: English stemming so "panic" matches "panicked"/"panicking"
//
// All three are pure-SQLite — they do not require external linkage and work
// on the modernc.org/sqlite (glebarez) build used in this project.
func setupSQLiteFTS5(db *gorm.DB) error {
	create := `CREATE VIRTUAL TABLE IF NOT EXISTS ` + fts5LogsTable + ` USING fts5(
		body,
		service_name,
		content='logs',
		content_rowid='id',
		tokenize='porter unicode61 remove_diacritics 2'
	)`
	if err := db.Exec(create).Error; err != nil {
		// FTS5 is included in the modernc.org/sqlite amalgamation by default;
		// if this fails, the build was compiled without FTS5. Surface the
		// failure so SearchLogs can fall back to LIKE rather than producing
		// a confusing "no such table" error later.
		return fmt.Errorf("create fts5 virtual table: %w", err)
	}

	triggers := []struct {
		name string
		ddl  string
	}{
		{
			name: "logs_ai",
			ddl: `CREATE TRIGGER IF NOT EXISTS logs_ai AFTER INSERT ON logs BEGIN
				INSERT INTO ` + fts5LogsTable + `(rowid, body, service_name) VALUES (new.id, new.body, new.service_name);
			END`,
		},
		{
			name: "logs_ad",
			ddl: `CREATE TRIGGER IF NOT EXISTS logs_ad AFTER DELETE ON logs BEGIN
				INSERT INTO ` + fts5LogsTable + `(` + fts5LogsTable + `, rowid, body, service_name) VALUES ('delete', old.id, old.body, old.service_name);
			END`,
		},
		{
			name: "logs_au",
			ddl: `CREATE TRIGGER IF NOT EXISTS logs_au AFTER UPDATE ON logs BEGIN
				INSERT INTO ` + fts5LogsTable + `(` + fts5LogsTable + `, rowid, body, service_name) VALUES ('delete', old.id, old.body, old.service_name);
				INSERT INTO ` + fts5LogsTable + `(rowid, body, service_name) VALUES (new.id, new.body, new.service_name);
			END`,
		},
	}
	for _, tr := range triggers {
		if err := db.Exec(tr.ddl).Error; err != nil {
			return fmt.Errorf("create trigger %s: %w", tr.name, err)
		}
	}

	// Backfill any rows already present in `logs` but not yet in the FTS index.
	// `rebuild` is a no-op on a fresh DB and cheap on a populated one — FTS5
	// streams the source rows once.
	if err := db.Exec(`INSERT INTO ` + fts5LogsTable + `(` + fts5LogsTable + `) VALUES ('rebuild')`).Error; err != nil {
		return fmt.Errorf("rebuild fts5 index: %w", err)
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

// fts5Available reports whether the given driver should use the FTS5 path. We
// only enable FTS5 on SQLite because Postgres has its own pg_trgm GIN path
// (see factory.go) and MySQL/SQL Server are out of scope.
func fts5Available(driver string) bool {
	return strings.ToLower(driver) == "sqlite"
}
