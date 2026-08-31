package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const postgresAdvisoryLockKey int64 = 5716286928489378676 // "OTELMIGT"

// StateError carries the read-only status that made an operation fail closed.
type StateError struct {
	Operation string
	Status    Status
}

func (e *StateError) Error() string {
	action := "run `otelcontext migrate status` and inspect the reported schema"
	switch e.Status.State {
	case StateEmpty, StateBehind:
		action = "run `otelcontext migrate up`"
	case StateUnmanaged:
		action = "run `otelcontext migrate baseline --from v0.3.1` or `--from v0.4.0-beta.2` after matching the source release"
	case StateAhead:
		action = "run the newer signed binary that owns this schema or restore the pre-migration backup"
	case StateDirty:
		action = "restore the pre-migration backup or resolve the recorded dirty migration before retrying"
	}
	return fmt.Sprintf("%s refused: main schema state=%s expected=%d actual=%d: %s; %s",
		e.Operation, e.Status.State, e.Status.ExpectedVersion, e.Status.ActualVersion, e.Status.Detail, action)
}

// RequireExact performs the production startup compatibility gate.
func RequireExact(ctx context.Context, db *gorm.DB, driver string) (Status, error) {
	status, err := Inspect(ctx, db, driver)
	if err != nil {
		return status, err
	}
	if !status.Usable() {
		return status, &StateError{Operation: "startup", Status: status}
	}
	return status, nil
}

// BaselineResult records the no-repair bridge from a published release.
type BaselineResult struct {
	Release           string
	RecordedVersion   int
	BeforeFingerprint string
	AfterFingerprint  string
	Status            Status
}

type runner struct {
	registry []migration
}

func newRunner(driver string) (*runner, error) {
	registry, err := registryFor(driver)
	if err != nil {
		return nil, err
	}
	return &runner{registry: registry}, nil
}

// Up installs an empty supported database or applies every pending migration.
func Up(ctx context.Context, db *gorm.DB, driver string) (Status, error) {
	driver = NormalizeDriver(driver)
	if !SupportsVersioned(driver) {
		status, _ := Inspect(ctx, db, driver)
		return status, &StateError{Operation: "migrate up", Status: status}
	}
	r, err := newRunner(driver)
	if err != nil {
		return Status{}, err
	}
	return r.up(ctx, db, driver)
}

func (r *runner) up(ctx context.Context, db *gorm.DB, driver string) (Status, error) {
	status, err := Inspect(ctx, db, driver)
	if err != nil {
		return status, err
	}
	switch status.State {
	case StateExact:
		return status, nil
	case StateEmpty, StateBehind:
		// Continue below.
	default:
		return status, &StateError{Operation: "migrate up", Status: status}
	}
	for {
		applied, err := r.applyNext(ctx, db, driver)
		if err != nil {
			return status, err
		}
		status, err = Inspect(ctx, db, driver)
		if err != nil {
			return status, err
		}
		if status.State == StateExact {
			return status, nil
		}
		if status.State != StateBehind || !applied {
			return status, &StateError{Operation: "migrate up", Status: status}
		}
	}
}

func (r *runner) applyNext(ctx context.Context, db *gorm.DB, driver string) (bool, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return false, fmt.Errorf("open migration connection: %w", err)
	}
	applied := false
	err = withLockedTransaction(ctx, sqlDB, driver, func(tx sqlExecutor) error {
		if err := ensureLedger(ctx, tx, driver); err != nil {
			return err
		}
		rows, err := loadRawLedger(ctx, tx)
		if err != nil {
			return err
		}
		actual, err := validateRawLedger(rows, r.registry)
		if err != nil {
			return err
		}
		if actual >= len(r.registry) {
			return nil
		}
		if actual == 0 {
			count, err := countOwnedTables(ctx, tx, driver)
			if err != nil {
				return err
			}
			if count != 0 {
				return errors.New("migrate up refused: non-empty database has no migration ledger; run migrate baseline")
			}
		}
		entry := r.registry[actual]
		for number, statement := range migrationStatements(entry.SQL) {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration %d (%s) statement %d: %w", entry.Version, entry.Name, number+1, err)
			}
		}
		now := time.Now().UTC()
		if err := insertLedgerRow(ctx, tx, driver, entry, now); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// Baseline validates a frozen released structure and records it without repair.
func Baseline(ctx context.Context, db *gorm.DB, driver, release string) (BaselineResult, error) {
	driver = NormalizeDriver(driver)
	result := BaselineResult{Release: release}
	if !SupportsVersioned(driver) {
		status, _ := Inspect(ctx, db, driver)
		result.Status = status
		return result, &StateError{Operation: "migrate baseline", Status: status}
	}
	baselineVersion := 0
	switch release {
	case "v0.3.1":
		baselineVersion = 1
	case "v0.4.0-beta.2":
		baselineVersion = 2
	default:
		return result, fmt.Errorf("unsupported baseline %q; supported releases are v0.3.1 and v0.4.0-beta.2", release)
	}
	status, err := Inspect(ctx, db, driver)
	if err != nil {
		return result, err
	}
	if status.State != StateUnmanaged {
		result.Status = status
		return result, &StateError{Operation: "migrate baseline", Status: status}
	}
	if err := validateSchema(ctx, db, driver, baselineVersion); err != nil {
		status.State = StateIncompatible
		status.Detail = fmt.Sprintf("database does not match frozen baseline %s: %v", release, err)
		result.Status = status
		return result, &StateError{Operation: "migrate baseline", Status: status}
	}
	result.BeforeFingerprint, err = SchemaFingerprint(ctx, db, driver, baselineVersion)
	if err != nil {
		return result, err
	}
	r, err := newRunner(driver)
	if err != nil {
		return result, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return result, err
	}
	err = withLockedTransaction(ctx, sqlDB, driver, func(tx sqlExecutor) error {
		if err := ensureLedger(ctx, tx, driver); err != nil {
			return err
		}
		rows, err := loadRawLedger(ctx, tx)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			return errors.New("migrate baseline refused: migration ledger appeared while waiting for the lock")
		}
		count, err := countOwnedTables(ctx, tx, driver)
		if err != nil {
			return err
		}
		if count != len(ownedTableNames) {
			return fmt.Errorf("migrate baseline refused: found %d of %d required tables after locking", count, len(ownedTableNames))
		}
		now := time.Now().UTC()
		for _, entry := range r.registry[:baselineVersion] {
			if err := insertLedgerRow(ctx, tx, driver, entry, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.RecordedVersion = baselineVersion
	result.AfterFingerprint, err = SchemaFingerprint(ctx, db, driver, baselineVersion)
	if err != nil {
		return result, err
	}
	if result.BeforeFingerprint != result.AfterFingerprint {
		return result, errors.New("baseline changed the product schema fingerprint; ledger transaction was not metadata-only")
	}
	result.Status, err = Inspect(ctx, db, driver)
	return result, err
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func withLockedTransaction(ctx context.Context, db *sql.DB, driver string, fn func(sqlExecutor) error) error {
	switch NormalizeDriver(driver) {
	case "sqlite":
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("acquire sqlite migration connection: %w", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return fmt.Errorf("begin sqlite immediate migration: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			}
		}()
		if err := fn(conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit sqlite migration: %w", err)
		}
		committed = true
		return nil
	case "postgres":
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin postgres migration: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", postgresAdvisoryLockKey); err != nil {
			return fmt.Errorf("acquire postgres migration lock: %w", err)
		}
		if err := fn(tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit postgres migration: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("transactional migrations are unverified for driver %q", driver)
	}
}

func ensureLedger(ctx context.Context, tx sqlExecutor, driver string) error {
	var statement string
	if NormalizeDriver(driver) == "postgres" {
		statement = `CREATE TABLE IF NOT EXISTS otelcontext_schema_migrations (
version bigint PRIMARY KEY,
name varchar(128) NOT NULL,
checksum char(64) NOT NULL,
started_at timestamptz NOT NULL,
completed_at timestamptz,
dirty boolean NOT NULL
)`
	} else {
		statement = `CREATE TABLE IF NOT EXISTS otelcontext_schema_migrations (
version integer PRIMARY KEY,
name text NOT NULL,
checksum text NOT NULL,
started_at datetime NOT NULL,
completed_at datetime,
dirty numeric NOT NULL
)`
	}
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	return nil
}

type rawLedgerRow struct {
	version    int
	name       string
	checksum   string
	dirty      bool
	incomplete bool
}

func loadRawLedger(ctx context.Context, tx sqlExecutor) ([]rawLedgerRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version, name, checksum, dirty, completed_at IS NULL
FROM otelcontext_schema_migrations ORDER BY version ASC`)
	if err != nil {
		return nil, fmt.Errorf("read migration ledger under lock: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []rawLedgerRow
	for rows.Next() {
		var row rawLedgerRow
		if err := rows.Scan(&row.version, &row.name, &row.checksum, &row.dirty, &row.incomplete); err != nil {
			return nil, fmt.Errorf("scan migration ledger under lock: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func validateRawLedger(rows []rawLedgerRow, registry []migration) (int, error) {
	if len(rows) > len(registry) {
		return 0, fmt.Errorf("migration ledger is ahead: %d entries, binary knows %d", len(rows), len(registry))
	}
	for i, row := range rows {
		want := registry[i]
		if row.version != i+1 || row.version != want.Version {
			return 0, fmt.Errorf("migration ledger gap at version %d", i+1)
		}
		if row.dirty || row.incomplete {
			return 0, fmt.Errorf("migration %d is dirty or incomplete", row.version)
		}
		if row.name != want.Name || row.checksum != want.Checksum {
			return 0, fmt.Errorf("migration %d metadata does not match the embedded registry", row.version)
		}
	}
	return len(rows), nil
}

func countOwnedTables(ctx context.Context, tx sqlExecutor, driver string) (int, error) {
	var count int
	var err error
	if NormalizeDriver(driver) == "postgres" {
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema = current_schema()
AND table_name IN ('traces','spans','metric_buckets','logs','investigations','drain_templates')`).Scan(&count)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table'
AND name IN ('traces','spans','metric_buckets','logs','investigations','drain_templates')`).Scan(&count)
	}
	if err != nil {
		return 0, fmt.Errorf("count OtelContext tables: %w", err)
	}
	return count, nil
}

func insertLedgerRow(ctx context.Context, tx sqlExecutor, driver string, entry migration, now time.Time) error {
	statement := `INSERT INTO otelcontext_schema_migrations
(version, name, checksum, started_at, completed_at, dirty) VALUES (?, ?, ?, ?, ?, ?)`
	if NormalizeDriver(driver) == "postgres" {
		statement = `INSERT INTO otelcontext_schema_migrations
(version, name, checksum, started_at, completed_at, dirty) VALUES ($1, $2, $3, $4, $5, $6)`
	}
	if _, err := tx.ExecContext(ctx, statement, entry.Version, entry.Name, entry.Checksum, now, now, false); err != nil {
		return fmt.Errorf("record migration %d (%s): %w", entry.Version, entry.Name, err)
	}
	return nil
}

// Description returns a compact stable line for logs and command output.
func (s Status) Description() string {
	actual := fmt.Sprint(s.ActualVersion)
	if s.ActualVersion == 0 {
		actual = "none"
	}
	parts := []string{
		"state=" + string(s.State),
		fmt.Sprintf("expected=%d", s.ExpectedVersion),
		"actual=" + actual,
	}
	if s.Fingerprint != "" {
		parts = append(parts, "fingerprint="+s.Fingerprint)
	}
	if s.Detail != "" {
		parts = append(parts, "detail="+fmt.Sprintf("%q", s.Detail))
	}
	return strings.Join(parts, " ")
}
