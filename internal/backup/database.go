package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	schemamigrate "github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	_ "github.com/glebarez/go-sqlite"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

var mainTables = []string{
	"traces",
	"spans",
	"metric_buckets",
	"logs",
	"investigations",
	"drain_templates",
	schemamigrate.LedgerTable,
}

var aggregateOwnerTables = []string{
	"aggregate_baseline",
	"aggregate_buckets",
	"aggregate_delta_log",
	"aggregate_log_template",
	"aggregate_series",
	"aggregate_dict",
	"aggregate_meta",
}

type tableCount struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

func mainSourceIdentity(cfg Config) (string, error) {
	if normalizeDriver(cfg.DBDriver) == "sqlite" {
		path, err := sqlitePath(cfg.DBDSN)
		if err != nil {
			return "", err
		}
		return identity(path), nil
	}
	if strings.TrimSpace(cfg.DBDSN) == "" {
		return "", fmt.Errorf("DB_DSN is required for %s", normalizeDriver(cfg.DBDriver))
	}
	return identity(normalizeDriver(cfg.DBDriver) + "\x00" + cfg.DBDSN), nil
}

func sqlitePath(dsn string) (string, error) {
	if strings.TrimSpace(dsn) == "" {
		dsn = "OtelContext.db"
	}
	if dsn == ":memory:" || strings.Contains(dsn, "mode=memory") || strings.HasPrefix(dsn, "file::memory:") {
		return "", errors.New("in-memory SQLite databases cannot be backed up")
	}
	plain := dsn
	if strings.HasPrefix(dsn, "file:") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse SQLite DSN: %w", err)
		}
		switch {
		case parsed.Path != "":
			plain = parsed.Path
		case parsed.Opaque != "":
			plain = parsed.Opaque
		default:
			plain = strings.TrimPrefix(strings.SplitN(dsn, "?", 2)[0], "file:")
		}
	} else if index := strings.IndexByte(plain, '?'); index >= 0 {
		plain = plain[:index]
	}
	if plain == "" {
		return "", errors.New("SQLite DSN does not name a file")
	}
	return resolved(plain)
}

func inspectMain(ctx context.Context, cfg Config) (MainOwner, error) {
	driver := normalizeDriver(cfg.DBDriver)
	if driver == "sqlite" {
		path, err := sqlitePath(cfg.DBDSN)
		if err != nil {
			return MainOwner{}, err
		}
		if _, err := requireRegular(path); err != nil {
			return MainOwner{}, fmt.Errorf("inspect main SQLite database: %w", err)
		}
	}
	db, err := storage.NewDatabase(driver, cfg.DBDSN)
	if err != nil {
		return MainOwner{}, fmt.Errorf("open main database for backup inspection: %w", err)
	}
	defer closeGORM(db)
	return inspectMainDB(ctx, driver, cfg.DBDSN, db)
}

func inspectMainDB(ctx context.Context, driver, dsn string, db *gorm.DB) (MainOwner, error) {
	status, err := schemamigrate.Inspect(ctx, db, driver)
	if err != nil {
		return MainOwner{}, fmt.Errorf("inspect main migration status: %w", err)
	}
	if schemamigrate.SupportsVersioned(driver) {
		if !status.Usable() {
			return MainOwner{}, fmt.Errorf("main migration state is %s, require exact before backup", status.State)
		}
	} else if status.State != schemamigrate.StateUnverified {
		return MainOwner{}, fmt.Errorf("unexpected preview migration state %s", status.State)
	}
	counts, err := mainTableCounts(ctx, db)
	if err != nil {
		return MainOwner{}, err
	}
	lifecycle, err := hashJSON(struct {
		Driver      string       `json:"driver"`
		Migration   string       `json:"migration_fingerprint"`
		TableCounts []tableCount `json:"table_counts"`
	}{driver, status.Fingerprint, counts})
	if err != nil {
		return MainOwner{}, err
	}
	engineVersion, err := databaseVersion(ctx, db, driver)
	if err != nil {
		return MainOwner{}, err
	}
	if err := validateEngineProfile(driver, engineVersion); err != nil {
		return MainOwner{}, err
	}
	id, err := mainSourceIdentity(Config{DBDriver: driver, DBDSN: dsn})
	if err != nil {
		return MainOwner{}, err
	}
	applied := make([]AppliedMigration, 0, len(status.Applied))
	for _, migration := range status.Applied {
		applied = append(applied, AppliedMigration{Version: migration.Version, Name: migration.Name, Checksum: migration.Checksum})
	}
	return MainOwner{
		Adapter:              driver,
		EngineVersion:        engineVersion,
		SourceIdentity:       id,
		MigrationState:       string(status.State),
		MigrationVersion:     status.ActualVersion,
		ExpectedMigration:    status.ExpectedVersion,
		MigrationFingerprint: status.Fingerprint,
		AppliedMigrations:    applied,
		LifecycleFingerprint: lifecycle,
	}, nil
}

func inspectSQLiteMainArtifact(ctx context.Context, path string) (MainOwner, error) {
	if err := sqliteIntegrity(ctx, path); err != nil {
		return MainOwner{}, err
	}
	db, err := gorm.Open(sqlite.Open(readOnlySQLiteDSN(path)), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		return MainOwner{}, fmt.Errorf("open SQLite snapshot read-only: %w", err)
	}
	defer closeGORM(db)
	return inspectMainDB(ctx, "sqlite", path, db)
}

func mainTableCounts(ctx context.Context, db *gorm.DB) ([]tableCount, error) {
	counts := make([]tableCount, 0, len(mainTables))
	for _, table := range mainTables {
		if !db.WithContext(ctx).Migrator().HasTable(table) {
			counts = append(counts, tableCount{Table: table})
			continue
		}
		var count int64
		if err := db.WithContext(ctx).Table(table).Count(&count).Error; err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts = append(counts, tableCount{Table: table, Rows: count})
	}
	return counts, nil
}

func databaseVersion(ctx context.Context, db *gorm.DB, driver string) (string, error) {
	query := "SELECT sqlite_version()"
	switch driver {
	case "postgres":
		query = "SHOW server_version"
	case "mysql":
		query = "SELECT VERSION()"
	case "mssql":
		query = "SELECT CONVERT(varchar(128), SERVERPROPERTY('ProductVersion'))"
	}
	var version string
	if err := db.WithContext(ctx).Raw(query).Scan(&version).Error; err != nil {
		return "", fmt.Errorf("read %s engine version: %w", driver, err)
	}
	return strings.TrimSpace(version), nil
}

func validateEngineProfile(driver, version string) error {
	prefix := ""
	switch driver {
	case "sqlite":
		return nil
	case "postgres":
		prefix = "16."
	case "mysql":
		prefix = "8.4."
	case "mssql":
		prefix = "16."
	default:
		return fmt.Errorf("unsupported backup adapter %q", driver)
	}
	if !strings.HasPrefix(strings.TrimSpace(version), prefix) {
		return fmt.Errorf("unsupported %s engine version %q: require %s", driver, version, prefix)
	}
	return nil
}

func closeGORM(db *gorm.DB) {
	if db == nil {
		return
	}
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

func vacuumSQLite(ctx context.Context, sourceDSN, target string) error {
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("SQLite snapshot target already exists: %s", target)
	} else if !os.IsNotExist(err) {
		return err
	}
	db, err := sql.Open("sqlite", sourceDSN)
	if err != nil {
		return fmt.Errorf("open SQLite source: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		_ = db.Close()
		_ = os.Remove(target)
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	if err := db.Close(); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("close SQLite source after VACUUM INTO: %w", err)
	}
	file, err := os.Open(target) // #nosec G304 -- newly captured staging artifact.
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readOnlySQLiteDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro"
}

func sqliteIntegrity(ctx context.Context, path string) error {
	if _, err := requireRegular(path); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", readOnlySQLiteDSN(path))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("SQLite integrity_check: %w", err)
	}
	var results []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			_ = rows.Close()
			return err
		}
		results = append(results, result)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(results) != 1 || results[0] != "ok" {
		return fmt.Errorf("SQLite integrity_check returned %q", results)
	}
	foreignKeys, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("SQLite foreign_key_check: %w", err)
	}
	defer func() { _ = foreignKeys.Close() }()
	if foreignKeys.Next() {
		return errors.New("SQLite foreign_key_check returned violations")
	}
	return foreignKeys.Err()
}

func inspectAggregate(ctx context.Context, path string) (AggregateOwner, error) {
	inspection, err := aggregate.InspectSQLiteStore(path)
	if err != nil {
		return AggregateOwner{}, err
	}
	if !inspection.Usable() {
		return AggregateOwner{}, fmt.Errorf("aggregate store state is %s, require exact before backup: %s", inspection.State, inspection.Detail)
	}
	absolutePath, err := sqlitePath(path)
	if err != nil {
		return AggregateOwner{}, err
	}
	if err := sqliteIntegrity(ctx, absolutePath); err != nil {
		return AggregateOwner{}, err
	}
	db, err := sql.Open("sqlite", readOnlySQLiteDSN(absolutePath))
	if err != nil {
		return AggregateOwner{}, err
	}
	defer func() { _ = db.Close() }()
	counts := make([]tableCount, 0, len(aggregateOwnerTables))
	for _, table := range aggregateOwnerTables {
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil { // #nosec G202 -- fixed table allowlist.
			return AggregateOwner{}, fmt.Errorf("count %s: %w", table, err)
		}
		counts = append(counts, tableCount{Table: table, Rows: count})
	}
	meta := make(map[string]string)
	rows, err := db.QueryContext(ctx, "SELECT key, value FROM aggregate_meta ORDER BY key")
	if err != nil {
		return AggregateOwner{}, err
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return AggregateOwner{}, err
		}
		meta[key] = value
	}
	if err := rows.Close(); err != nil {
		return AggregateOwner{}, err
	}
	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type metaEntry struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	metaEntries := make([]metaEntry, 0, len(keys))
	for _, key := range keys {
		metaEntries = append(metaEntries, metaEntry{key, meta[key]})
	}
	lifecycle, err := hashJSON(struct {
		Tables []tableCount `json:"tables"`
		Meta   []metaEntry  `json:"meta"`
	}{counts, metaEntries})
	if err != nil {
		return AggregateOwner{}, err
	}
	dictWatermark, err := strconv.ParseUint(meta[aggregate.MetaDictWatermark], 10, 64)
	if err != nil {
		return AggregateOwner{}, fmt.Errorf("invalid aggregate dictionary watermark: %w", err)
	}
	seriesWatermark, err := strconv.ParseUint(meta[aggregate.MetaSeriesWatermark], 10, 64)
	if err != nil {
		return AggregateOwner{}, fmt.Errorf("invalid aggregate series watermark: %w", err)
	}
	return AggregateOwner{
		SourceIdentity:       identity(absolutePath),
		StoreUUID:            inspection.StoreUUID,
		SchemaVersion:        inspection.ActualSchemaVersion,
		SeriesKeyVersion:     inspection.ActualSeriesVersion,
		SketchCodecVersion:   inspection.ActualSketchVersion,
		DictHighWatermark:    dictWatermark,
		SeriesHighWatermark:  seriesWatermark,
		LifecycleFingerprint: lifecycle,
		Integrity:            "PRAGMA integrity_check=ok; PRAGMA foreign_key_check=0 rows",
	}, nil
}

func compareMain(expected, actual MainOwner) error {
	if expected.Adapter != actual.Adapter || expected.MigrationState != actual.MigrationState || expected.MigrationVersion != actual.MigrationVersion || expected.ExpectedMigration != actual.ExpectedMigration || expected.MigrationFingerprint != actual.MigrationFingerprint || expected.LifecycleFingerprint != actual.LifecycleFingerprint {
		return fmt.Errorf("restored main database fingerprint mismatch: expected migration=%s/%d lifecycle=%s, got migration=%s/%d lifecycle=%s",
			expected.MigrationState, expected.MigrationVersion, expected.LifecycleFingerprint,
			actual.MigrationState, actual.MigrationVersion, actual.LifecycleFingerprint)
	}
	return nil
}

func compareAggregate(expected, actual AggregateOwner) error {
	if expected.StoreUUID != actual.StoreUUID || expected.SchemaVersion != actual.SchemaVersion || expected.SeriesKeyVersion != actual.SeriesKeyVersion || expected.SketchCodecVersion != actual.SketchCodecVersion || expected.DictHighWatermark != actual.DictHighWatermark || expected.SeriesHighWatermark != actual.SeriesHighWatermark || expected.LifecycleFingerprint != actual.LifecycleFingerprint {
		return fmt.Errorf("restored aggregate fingerprint mismatch: expected uuid=%s schema=%d series=%d codec=%d lifecycle=%s, got uuid=%s schema=%d series=%d codec=%d lifecycle=%s",
			expected.StoreUUID, expected.SchemaVersion, expected.SeriesKeyVersion, expected.SketchCodecVersion, expected.LifecycleFingerprint,
			actual.StoreUUID, actual.SchemaVersion, actual.SeriesKeyVersion, actual.SketchCodecVersion, actual.LifecycleFingerprint)
	}
	return nil
}

func validateMigrationCompatibility(main MainOwner) error {
	switch main.Adapter {
	case "sqlite", "postgres":
		if main.MigrationState != string(schemamigrate.StateExact) || main.MigrationVersion != schemamigrate.CurrentVersion || main.ExpectedMigration != schemamigrate.CurrentVersion {
			return fmt.Errorf("unsupported main migration state=%s actual=%d expected=%d", main.MigrationState, main.MigrationVersion, main.ExpectedMigration)
		}
	case "mysql", "mssql":
		if main.MigrationState != string(schemamigrate.StateUnverified) || main.MigrationVersion != 0 {
			return fmt.Errorf("unsupported preview migration state=%s actual=%d", main.MigrationState, main.MigrationVersion)
		}
	default:
		return fmt.Errorf("unsupported backup adapter %q", main.Adapter)
	}
	return nil
}

func ensureSQLiteFresh(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Lstat(candidate); err == nil {
			return fmt.Errorf("restore target is not fresh: %s already exists", candidate)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func publishSQLiteCopy(source, target string) error {
	if err := ensureSQLiteFresh(target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	partial := target + ".restore.partial"
	if _, err := os.Lstat(partial); err == nil {
		return fmt.Errorf("restore staging path already exists: %s", partial)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := copyRegular(source, partial, 0o600); err != nil {
		return err
	}
	if err := sqliteIntegrity(context.Background(), partial); err != nil {
		_ = os.Remove(partial)
		return err
	}
	if err := os.Rename(partial, target); err != nil {
		_ = os.Remove(partial)
		return err
	}
	return syncDir(filepath.Dir(target))
}
