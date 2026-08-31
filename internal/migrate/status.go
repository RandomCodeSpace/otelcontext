package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// State is the operator-visible compatibility state of the main database.
type State string

const (
	StateEmpty        State = "empty"
	StateUnmanaged    State = "unmanaged"
	StateExact        State = "exact"
	StateBehind       State = "behind"
	StateAhead        State = "ahead"
	StateDirty        State = "dirty"
	StateIncompatible State = "incompatible"
	StateUnverified   State = "unverified"
)

// Exit codes are stable so deployment scripts do not need to parse prose.
const (
	ExitOK           = 0
	ExitUsage        = 2
	ExitEmpty        = 10
	ExitUnmanaged    = 11
	ExitBehind       = 12
	ExitAhead        = 13
	ExitDirty        = 14
	ExitIncompatible = 15
	ExitUnverified   = 16
)

// Applied records one ledger entry without exposing the database row type.
type Applied struct {
	Version     int
	Name        string
	Checksum    string
	StartedAt   time.Time
	CompletedAt *time.Time
	Dirty       bool
}

// Status is the complete read-only compatibility result for the main database.
type Status struct {
	Driver          string
	State           State
	ExpectedVersion int
	ActualVersion   int
	Fingerprint     string
	Detail          string
	Applied         []Applied
}

// Usable reports whether this binary can safely use the inspected schema.
func (s Status) Usable() bool { return s.State == StateExact }

// ExitCode maps a state to its stable command exit code.
func (s Status) ExitCode() int {
	switch s.State {
	case StateExact:
		return ExitOK
	case StateEmpty:
		return ExitEmpty
	case StateUnmanaged:
		return ExitUnmanaged
	case StateBehind:
		return ExitBehind
	case StateAhead:
		return ExitAhead
	case StateDirty:
		return ExitDirty
	case StateUnverified:
		return ExitUnverified
	default:
		return ExitIncompatible
	}
}

type ledgerRow struct {
	Version     int        `gorm:"column:version"`
	Name        string     `gorm:"column:name"`
	Checksum    string     `gorm:"column:checksum"`
	StartedAt   time.Time  `gorm:"column:started_at"`
	CompletedAt *time.Time `gorm:"column:completed_at"`
	Dirty       bool       `gorm:"column:dirty"`
}

type indexSpec struct {
	columns []string
	unique  bool
}

type tableSpec struct {
	columns []string
	primary []string
	indexes map[string]indexSpec
}

var ownedTableNames = []string{
	"traces",
	"spans",
	"metric_buckets",
	"logs",
	"investigations",
	"drain_templates",
}

func schemaSpec(version int) map[string]tableSpec {
	traceColumns := []string{"id", "tenant_id", "trace_id", "service_name", "duration", "status", "timestamp", "created_at", "updated_at", "deleted_at"}
	spanIndexes := map[string]indexSpec{
		"idx_spans_operation_name":       {columns: []string{"operation_name"}},
		"idx_spans_tenant_trace_span":    {columns: []string{"tenant_id", "trace_id", "span_id"}, unique: true},
		"idx_spans_tenant_service_start": {columns: []string{"tenant_id", "service_name", "start_time"}},
		"idx_spans_tenant_trace":         {columns: []string{"tenant_id", "trace_id"}},
	}
	if version >= 2 {
		traceColumns = append(traceColumns[:7], append([]string{"truncated", "retained_span_count", "observed_span_count"}, traceColumns[7:]...)...)
		spanIndexes["idx_spans_tenant_status_start"] = indexSpec{columns: []string{"tenant_id", "status", "start_time"}}
	} else {
		spanIndexes["idx_spans_status"] = indexSpec{columns: []string{"status"}}
	}
	return map[string]tableSpec{
		"traces": {
			columns: traceColumns,
			primary: []string{"id"},
			indexes: map[string]indexSpec{
				"idx_traces_deleted_at":      {columns: []string{"deleted_at"}},
				"idx_traces_timestamp":       {columns: []string{"timestamp"}},
				"idx_traces_duration":        {columns: []string{"duration"}},
				"idx_traces_tenant_trace_id": {columns: []string{"tenant_id", "trace_id"}, unique: true},
				"idx_traces_tenant_service":  {columns: []string{"tenant_id", "service_name"}},
				"idx_traces_tenant_ts":       {columns: []string{"tenant_id", "timestamp"}},
			},
		},
		"spans": {
			columns: []string{"id", "tenant_id", "trace_id", "span_id", "parent_span_id", "operation_name", "start_time", "end_time", "duration", "service_name", "status", "attributes_json"},
			primary: []string{"id"},
			indexes: spanIndexes,
		},
		"metric_buckets": {
			columns: []string{"id", "tenant_id", "name", "service_name", "time_bucket", "min", "max", "sum", "count", "attributes_json"},
			primary: []string{"id"},
			indexes: map[string]indexSpec{
				"idx_metric_buckets_time_bucket":    {columns: []string{"time_bucket"}},
				"idx_metrics_tenant_service_bucket": {columns: []string{"tenant_id", "service_name", "time_bucket"}},
				"idx_metrics_tenant_name_bucket":    {columns: []string{"tenant_id", "name", "time_bucket"}},
			},
		},
		"logs": {
			columns: []string{"id", "tenant_id", "trace_id", "span_id", "severity", "body", "service_name", "attributes_json", "ai_insight", "timestamp"},
			primary: []string{"id"},
			indexes: map[string]indexSpec{
				"idx_logs_timestamp":       {columns: []string{"timestamp"}},
				"idx_logs_trace_id":        {columns: []string{"trace_id"}},
				"idx_logs_tenant_severity": {columns: []string{"tenant_id", "severity"}},
				"idx_logs_tenant_service":  {columns: []string{"tenant_id", "service_name"}},
				"idx_logs_tenant_ts":       {columns: []string{"tenant_id", "timestamp"}},
			},
		},
		"investigations": {
			columns: []string{"tenant_id", "id", "created_at", "status", "severity", "trigger_service", "trigger_operation", "error_message", "root_service", "root_operation", "causal_chain", "trace_ids", "error_logs", "anomalous_metrics", "affected_services", "span_chain"},
			primary: []string{"id"},
			indexes: map[string]indexSpec{
				"idx_investigations_trigger_service": {columns: []string{"trigger_service"}},
				"idx_investigations_tenant_created":  {columns: []string{"tenant_id", "created_at"}},
			},
		},
		"drain_templates": {
			columns: []string{"tenant_id", "id", "tokens", "count", "first_seen", "last_seen", "sample"},
			primary: []string{"tenant_id", "id"},
			indexes: map[string]indexSpec{
				"idx_drain_templates_last_seen":  {columns: []string{"last_seen"}},
				"idx_drain_templates_first_seen": {columns: []string{"first_seen"}},
			},
		},
	}
}

// Inspect reads the ledger and required relational structure without mutating it.
// A migration can commit between those two reads, so managed states are retried
// unless the ledger still matches the snapshot used for structural validation.
func Inspect(ctx context.Context, db *gorm.DB, driver string) (Status, error) {
	for attempt := 0; attempt < 3; attempt++ {
		status, err := inspectOnce(ctx, db, driver)
		if err != nil {
			return status, err
		}
		if len(status.Applied) == 0 {
			if (status.State == StateEmpty || status.State == StateUnmanaged) && db.WithContext(ctx).Migrator().HasTable(LedgerTable) {
				continue
			}
			return status, nil
		}
		stable, err := ledgerMatchesStatus(ctx, db, status)
		if err != nil {
			return status, err
		}
		if stable {
			return status, nil
		}
	}
	return Status{}, errors.New("migration ledger changed repeatedly during read-only inspection; retry status")
}

func inspectOnce(ctx context.Context, db *gorm.DB, driver string) (Status, error) {
	driver = NormalizeDriver(driver)
	status := Status{Driver: driver, ExpectedVersion: CurrentVersion}
	if !SupportsVersioned(driver) {
		status.State = StateUnverified
		status.Detail = "versioned schema compatibility is unverified; keep DB_AUTOMIGRATE=true for this preview driver"
		return status, nil
	}
	registry, err := registryFor(driver)
	if err != nil {
		return status, err
	}
	db = db.WithContext(ctx)
	if !db.Migrator().HasTable(LedgerTable) {
		count := 0
		for _, table := range ownedTableNames {
			if db.Migrator().HasTable(table) {
				count++
			}
		}
		status.Fingerprint, _ = SchemaFingerprint(ctx, db, driver, CurrentVersion)
		if count == 0 {
			status.State = StateEmpty
			status.Detail = "no OtelContext relational schema or migration ledger is present"
		} else {
			status.State = StateUnmanaged
			status.Detail = "OtelContext tables are present without a migration ledger; validate and baseline a published release"
		}
		return status, nil
	}

	var rows []ledgerRow
	if err := db.Table(LedgerTable).Order("version ASC").Find(&rows).Error; err != nil {
		return status, fmt.Errorf("read %s: %w", LedgerTable, err)
	}
	if len(rows) == 0 {
		status.State = StateIncompatible
		status.Detail = "migration ledger exists but contains no entries"
		status.Fingerprint, _ = SchemaFingerprint(ctx, db, driver, CurrentVersion)
		return status, nil
	}
	status.Applied = make([]Applied, 0, len(rows))
	for _, row := range rows {
		status.Applied = append(status.Applied, Applied{
			Version: row.Version, Name: row.Name, Checksum: row.Checksum,
			StartedAt: row.StartedAt, CompletedAt: row.CompletedAt, Dirty: row.Dirty,
		})
		if row.Version > status.ActualVersion {
			status.ActualVersion = row.Version
		}
		if row.Dirty || row.CompletedAt == nil {
			status.State = StateDirty
			status.Detail = fmt.Sprintf("migration %d (%s) is incomplete", row.Version, row.Name)
			status.Fingerprint, _ = SchemaFingerprint(ctx, db, driver, minVersion(status.ActualVersion))
			return status, nil
		}
	}
	if status.ActualVersion > CurrentVersion || len(rows) > len(registry) {
		status.State = StateAhead
		status.Detail = fmt.Sprintf("database version %d is ahead of binary version %d", status.ActualVersion, CurrentVersion)
		status.Fingerprint, _ = SchemaFingerprint(ctx, db, driver, CurrentVersion)
		return status, nil
	}
	for i, row := range rows {
		if row.Version != i+1 {
			status.State = StateIncompatible
			status.Detail = fmt.Sprintf("migration ledger gap at version %d", i+1)
			return status, nil
		}
		expected := registry[i]
		if row.Name != expected.Name {
			status.State = StateIncompatible
			status.Detail = fmt.Sprintf("migration %d name mismatch: got %q, expected %q", row.Version, row.Name, expected.Name)
			return status, nil
		}
		if row.Checksum != expected.Checksum {
			status.State = StateIncompatible
			status.Detail = fmt.Sprintf("migration %d checksum mismatch", row.Version)
			return status, nil
		}
	}
	if status.ActualVersion < CurrentVersion {
		if err := validateSchema(ctx, db, driver, status.ActualVersion); err != nil {
			status.State = StateIncompatible
			status.Detail = "managed schema does not match its recorded version: " + err.Error()
			return status, nil
		}
		status.State = StateBehind
		status.Detail = fmt.Sprintf("database version %d is behind binary version %d", status.ActualVersion, CurrentVersion)
		status.Fingerprint, _ = SchemaFingerprint(ctx, db, driver, status.ActualVersion)
		return status, nil
	}
	if err := validateSchema(ctx, db, driver, CurrentVersion); err != nil {
		status.State = StateIncompatible
		status.Detail = "recorded schema is structurally incompatible: " + err.Error()
		return status, nil
	}
	status.State = StateExact
	status.Detail = "relational schema and embedded migration checksums are exact"
	status.Fingerprint, err = SchemaFingerprint(ctx, db, driver, CurrentVersion)
	if err != nil {
		return status, err
	}
	return status, nil
}

func ledgerMatchesStatus(ctx context.Context, db *gorm.DB, status Status) (bool, error) {
	var rows []ledgerRow
	if err := db.WithContext(ctx).Table(LedgerTable).Order("version ASC").Find(&rows).Error; err != nil {
		return false, fmt.Errorf("re-read %s: %w", LedgerTable, err)
	}
	if len(rows) != len(status.Applied) {
		return false, nil
	}
	for i, row := range rows {
		applied := status.Applied[i]
		if row.Version != applied.Version || row.Name != applied.Name || row.Checksum != applied.Checksum || row.Dirty != applied.Dirty || (row.CompletedAt == nil) != (applied.CompletedAt == nil) {
			return false, nil
		}
	}
	return true, nil
}

func minVersion(version int) int {
	if version < 1 {
		return 1
	}
	if version > CurrentVersion {
		return CurrentVersion
	}
	return version
}

func validateSchema(ctx context.Context, db *gorm.DB, driver string, version int) error {
	if version < 1 || version > CurrentVersion {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	db = db.WithContext(ctx)
	spec := schemaSpec(version)
	tables := make([]string, 0, len(spec))
	for table := range spec {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		expected := spec[table]
		if !db.Migrator().HasTable(table) {
			return fmt.Errorf("missing table %s", table)
		}
		columnTypes, err := db.Migrator().ColumnTypes(table)
		if err != nil {
			return fmt.Errorf("inspect columns for %s: %w", table, err)
		}
		actualColumns := make([]string, 0, len(columnTypes))
		for _, column := range columnTypes {
			actualColumns = append(actualColumns, strings.ToLower(column.Name()))
		}
		sort.Strings(actualColumns)
		expectedColumns := append([]string(nil), expected.columns...)
		sort.Strings(expectedColumns)
		if !equalStrings(actualColumns, expectedColumns) {
			return fmt.Errorf("table %s columns got [%s], expected [%s]", table, strings.Join(actualColumns, ","), strings.Join(expectedColumns, ","))
		}
		primary, err := primaryKeyColumns(ctx, db, driver, table)
		if err != nil {
			return err
		}
		if !equalStrings(primary, expected.primary) {
			return fmt.Errorf("table %s primary key got [%s], expected [%s]", table, strings.Join(primary, ","), strings.Join(expected.primary, ","))
		}
		actualIndexes, err := readIndexes(ctx, db, driver, table)
		if err != nil {
			return fmt.Errorf("inspect indexes for %s: %w", table, err)
		}
		for name, want := range expected.indexes {
			got, ok := actualIndexes[name]
			if !ok {
				return fmt.Errorf("table %s is missing index %s", table, name)
			}
			if !equalStrings(got.columns, want.columns) || got.unique != want.unique {
				return fmt.Errorf("table %s index %s has columns [%s] unique=%t, expected [%s] unique=%t", table, name, strings.Join(got.columns, ","), got.unique, strings.Join(want.columns, ","), want.unique)
			}
		}
	}
	if version >= 2 {
		checks := []struct {
			table string
			query string
		}{
			{table: "investigations", query: `SELECT COUNT(*) FROM investigations WHERE tenant_id IS NULL OR tenant_id = ''`},
			{table: "drain_templates", query: `SELECT COUNT(*) FROM drain_templates WHERE tenant_id IS NULL OR tenant_id = ''`},
		}
		for _, check := range checks {
			var count int64
			if err := db.Raw(check.query).Scan(&count).Error; err != nil {
				return fmt.Errorf("validate %s tenant backfill: %w", check.table, err)
			}
			if count != 0 {
				return fmt.Errorf("table %s has %d rows without tenant_id", check.table, count)
			}
		}
	}
	return nil
}

func primaryKeyColumns(ctx context.Context, db *gorm.DB, driver, table string) ([]string, error) {
	switch NormalizeDriver(driver) {
	case "sqlite":
		type pragmaColumn struct {
			Name        string         `gorm:"column:name"`
			Default     sql.NullString `gorm:"column:dflt_value"`
			PrimaryRank int            `gorm:"column:pk"`
		}
		var columns []pragmaColumn
		if err := db.WithContext(ctx).Raw(`SELECT name, pk FROM pragma_table_info(?)`, table).Scan(&columns).Error; err != nil {
			return nil, fmt.Errorf("inspect primary key for %s: %w", table, err)
		}
		sort.Slice(columns, func(i, j int) bool { return columns[i].PrimaryRank < columns[j].PrimaryRank })
		var primary []string
		for _, column := range columns {
			if column.PrimaryRank > 0 {
				primary = append(primary, column.Name)
			}
		}
		return primary, nil
	case "postgres":
		var primary []string
		err := db.WithContext(ctx).Raw(`
SELECT a.attname::text
FROM pg_index i
JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
WHERE i.indrelid = to_regclass(?) AND i.indisprimary
ORDER BY k.ord`, table).Scan(&primary).Error
		if err != nil {
			return nil, fmt.Errorf("inspect primary key for %s: %w", table, err)
		}
		return primary, nil
	default:
		return nil, fmt.Errorf("primary-key inspection is unverified for driver %q", driver)
	}
}

// SchemaFingerprint hashes the required tables, columns, keys, and indexes.
// It deliberately excludes optional search indexes and the migration ledger.
func SchemaFingerprint(ctx context.Context, db *gorm.DB, driver string, version int) (string, error) {
	lines, err := schemaFingerprintLines(ctx, db, driver, version)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func schemaFingerprintLines(ctx context.Context, db *gorm.DB, driver string, version int) ([]string, error) {
	spec := schemaSpec(version)
	tables := make([]string, 0, len(spec))
	for table := range spec {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	var lines []string
	db = db.WithContext(ctx)
	for _, table := range tables {
		if !db.Migrator().HasTable(table) {
			lines = append(lines, "table:"+table+":missing")
			continue
		}
		columns, err := db.Migrator().ColumnTypes(table)
		if err != nil {
			return nil, err
		}
		for _, column := range columns {
			lines = append(lines, fmt.Sprintf("column:%s:%s:%s", table, strings.ToLower(column.Name()), strings.ToLower(column.DatabaseTypeName())))
		}
		primary, err := primaryKeyColumns(ctx, db, driver, table)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "primary:"+table+":"+strings.Join(primary, ","))
		byName, err := readIndexes(ctx, db, driver, table)
		if err != nil {
			return nil, err
		}
		indexNames := make([]string, 0, len(spec[table].indexes))
		for name := range spec[table].indexes {
			indexNames = append(indexNames, name)
		}
		sort.Strings(indexNames)
		for _, name := range indexNames {
			index, ok := byName[name]
			if !ok {
				lines = append(lines, "index:"+table+":"+name+":missing")
				continue
			}
			lines = append(lines, fmt.Sprintf("index:%s:%s:%s:%t", table, name, strings.Join(index.columns, ","), index.unique))
		}
	}
	sort.Strings(lines)
	return lines, nil
}

func readIndexes(ctx context.Context, db *gorm.DB, driver, table string) (map[string]indexSpec, error) {
	result := make(map[string]indexSpec)
	switch NormalizeDriver(driver) {
	case "sqlite":
		type indexListRow struct {
			Name   string `gorm:"column:name"`
			Unique int    `gorm:"column:unique"`
		}
		var list []indexListRow
		if err := db.WithContext(ctx).Raw(`SELECT name, "unique" FROM pragma_index_list(?)`, table).Scan(&list).Error; err != nil {
			return nil, err
		}
		for _, listed := range list {
			type indexColumn struct {
				Rank int    `gorm:"column:seqno"`
				Name string `gorm:"column:name"`
			}
			var columns []indexColumn
			if err := db.WithContext(ctx).Raw(`SELECT seqno, name FROM pragma_index_info(?)`, listed.Name).Scan(&columns).Error; err != nil {
				return nil, err
			}
			sort.Slice(columns, func(i, j int) bool { return columns[i].Rank < columns[j].Rank })
			names := make([]string, 0, len(columns))
			for _, column := range columns {
				names = append(names, column.Name)
			}
			result[listed.Name] = indexSpec{columns: names, unique: listed.Unique != 0}
		}
		return result, nil
	case "postgres":
		type indexColumn struct {
			IndexName string `gorm:"column:index_name"`
			Unique    bool   `gorm:"column:is_unique"`
			Column    string `gorm:"column:column_name"`
			Rank      int    `gorm:"column:column_rank"`
		}
		var rows []indexColumn
		err := db.WithContext(ctx).Raw(`
SELECT ci.relname::text AS index_name,
       i.indisunique AS is_unique,
       a.attname::text AS column_name,
       k.ord::integer AS column_rank
FROM pg_index i
JOIN pg_class t ON t.oid = i.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_class ci ON ci.oid = i.indexrelid
JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
WHERE n.nspname = current_schema() AND t.relname = ?
ORDER BY ci.relname, k.ord`, table).Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			index := result[row.IndexName]
			index.unique = row.Unique
			index.columns = append(index.columns, row.Column)
			result[row.IndexName] = index
		}
		return result, nil
	default:
		return nil, fmt.Errorf("index inspection is unverified for driver %q", driver)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
