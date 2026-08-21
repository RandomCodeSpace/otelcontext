package aggregate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Registers the "sqlite" database/sql driver. Same pure-Go engine the
	// main relational path uses through its GORM dialector — the aggregate
	// store deliberately does NOT go through GORM: it owns raw statements,
	// its own connections, and its own PRAGMA stanza (ADR 0003).
	_ "github.com/glebarez/go-sqlite"
)

// The SQLite aggregate store: data/aggregate.db, its own WAL, its own pool,
// its own PRAGMA stanza (ADR 0003).
//
// Why a separate file at all: SQLite allows exactly one writer per database
// file. Sharing OtelContext.db would serialize the durable-ACK group commit
// behind raw-exemplar, GraphRAG and legacy writes, which is the same as saying
// OTLP acknowledgment latency is a function of unrelated write bursts. No
// cross-file transaction is needed because every aggregate table is
// aggregate-owned.
//
// Connections: ONE writer connection (MaxOpenConns=1, plus an explicit mutex so
// the ordering is visible rather than emergent) and a small read pool. The
// writer mutex is also what makes FinalizeWindow's "materialize then delete
// exactly the incorporated rows" safe: no commit can interleave with it.

// Default store tuning.
const (
	// DefaultAggregateDBPath is the default AGGREGATE_DB_PATH.
	DefaultAggregateDBPath = "./data/aggregate.db"
	// defaultReadPoolSize is the read-connection count. Reads are dashboard
	// range scans and recovery replays, not a hot path.
	defaultReadPoolSize = 4
	// defaultBusyTimeoutMs bounds how long a statement waits on the writer
	// lock before erroring, matching the main DB's stanza.
	defaultBusyTimeoutMs = 5000
	// maxDictRows and maxSeriesRows bound the startup warm-up loads. Both are
	// far above the #158 caps; they exist so a corrupted or hostile file
	// cannot make startup allocate without bound.
	maxDictRows   = 2_000_000
	maxSeriesRows = 500_000
	// purgeWindowsPerPass bounds how many windows one PurgeBefore call
	// deletes, so retention never opens an unbounded transaction.
	purgeWindowsPerPass = 4096
)

// aggregateTables is every table this store owns. AGGREGATE_ALLOW_REBUILD drops
// exactly these and nothing else — the aggregate DB is its own file, but an
// operator who points AGGREGATE_DB_PATH at the wrong file deserves a store that
// only ever destroys its own tables.
var aggregateTables = []string{
	"aggregate_baseline",
	"aggregate_buckets",
	"aggregate_delta_log",
	"aggregate_series",
	"aggregate_dict",
	"aggregate_meta",
}

// StoreConfig configures the SQLite aggregate store.
type StoreConfig struct {
	// Path is the database file (AGGREGATE_DB_PATH). Empty takes
	// DefaultAggregateDBPath. ":memory:" and file: URIs are honoured for
	// tests.
	Path string
	// AllowRebuild permits DESTROYING and recreating the aggregate tables
	// when the on-disk schema is partial or version-mismatched
	// (AGGREGATE_ALLOW_REBUILD). Off by default: silent data loss is worse
	// than a refused startup.
	AllowRebuild bool
	// Synchronous is the SQLite synchronous mode ("NORMAL" or "FULL").
	// Empty takes NORMAL. See the stanza comment in open() for the durability
	// argument.
	Synchronous string
	// ReadPoolSize is the read-connection count. Zero takes the default.
	ReadPoolSize int
	// CacheSizeKB is the per-connection page cache in KB, applied as a
	// negative cache_size. Zero takes 32 MB — the aggregate working set is
	// the mutable delta log, not seven days of buckets.
	CacheSizeKB int
	// Metrics is the durable path's metric surface. nil disables recording.
	Metrics StoreMetrics
}

// SQLiteStore is the Store implementation on a dedicated SQLite file.
type SQLiteStore struct {
	path    string
	writer  *sql.DB
	reader  *sql.DB
	metrics StoreMetrics

	// writeMu serializes every write path. MaxOpenConns(1) already
	// serializes at the pool, but the mutex is what lets FinalizeWindow hold
	// "no commit interleaves" across its read-then-write phases.
	writeMu chan struct{}

	uuid string
}

// OpenSQLiteStore opens (creating if absent) the aggregate database.
//
// Schema handling per #162: absent schema is created at the current version; a
// partial schema, missing meta, or any version mismatch fails startup with an
// explicit error. There are no automatic migrations in v1.
func OpenSQLiteStore(cfg StoreConfig) (*SQLiteStore, error) {
	if cfg.Path == "" {
		cfg.Path = DefaultAggregateDBPath
	}
	if cfg.ReadPoolSize <= 0 {
		cfg.ReadPoolSize = defaultReadPoolSize
	}
	if cfg.CacheSizeKB <= 0 {
		cfg.CacheSizeKB = 32768
	}
	sync := strings.ToUpper(strings.TrimSpace(cfg.Synchronous))
	switch sync {
	case "":
		sync = "NORMAL"
	case "NORMAL", "FULL":
	default:
		return nil, fmt.Errorf("aggregate store: invalid synchronous mode %q (want NORMAL or FULL)", cfg.Synchronous)
	}

	if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." && !strings.HasPrefix(cfg.Path, ":") {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("aggregate store: create %s: %w", dir, err)
		}
	}

	writer, err := openAggregateDB(cfg.Path, sync, cfg.CacheSizeKB, true)
	if err != nil {
		return nil, err
	}
	reader, err := openAggregateDB(cfg.Path, sync, cfg.CacheSizeKB, false)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	reader.SetMaxOpenConns(cfg.ReadPoolSize)
	reader.SetMaxIdleConns(cfg.ReadPoolSize)
	reader.SetConnMaxLifetime(0)

	s := &SQLiteStore{
		path:    cfg.Path,
		writer:  writer,
		reader:  reader,
		metrics: cfg.Metrics,
		writeMu: make(chan struct{}, 1),
	}
	if s.metrics == nil {
		s.metrics = noopStoreMetrics{}
	}

	if err := verifyPragmas(writer, sync); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.ensureSchema(cfg.AllowRebuild); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// openAggregateDB opens one pool against the aggregate file with the store's
// PRAGMA stanza attached to the DSN, so EVERY connection the pool opens —
// including one replacing a broken connection — carries it. The driver applies
// _pragma values at connection setup and fails the connection if one is
// rejected, which is the fail-closed behaviour the main DB gets from its
// explicit Exec stanza (internal/storage/factory.go).
func openAggregateDB(path, synchronous string, cacheKB int, writer bool) (*sql.DB, error) {
	pragmas := []string{
		"journal_mode(WAL)",
		"synchronous(" + synchronous + ")",
		fmt.Sprintf("cache_size(-%d)", cacheKB),
		"temp_store(MEMORY)",
		fmt.Sprintf("busy_timeout(%d)", defaultBusyTimeoutMs),
		// The WAL is the durability boundary; cap it so a burst of group
		// commits cannot grow the file without bound between checkpoints.
		"wal_autocheckpoint(4000)",
		"journal_size_limit(67108864)",
		"foreign_keys(0)",
	}
	dsn := path + "?"
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	if writer {
		// Every writer transaction takes the write lock immediately rather
		// than starting deferred and upgrading — upgrades are what produce
		// SQLITE_BUSY under concurrent readers.
		q.Set("_txlock", "immediate")
	}
	dsn += q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: open %s: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("aggregate store: connect %s: %w", path, err)
	}
	return db, nil
}

// verifyPragmas reads back the two PRAGMAs the durability contract depends on.
// A SQLite build that silently ignored journal_mode=WAL would turn durable ACK
// into a rollback-journal lie, so this is fail-closed exactly like the main
// DB's stanza.
//
// synchronous=NORMAL is the default and is the honest match for the ACK
// contract as #160 states it: the contract is "no acknowledged aggregate loss
// on process or container crash" (emptyDir survives a container restart; heap
// does not). Under WAL, NORMAL fsyncs at checkpoint rather than at each COMMIT,
// which survives a process kill -9 — the release-gate test — but not a host
// power loss. FULL buys power-loss durability for one fsync per group commit;
// at a <=5 ms coalescing window that is up to 200 fsyncs/s on the 2-vCPU
// target, which is exactly the ACK p99 budget the gate measures. Operators who
// need power-loss durability set AGGREGATE_SYNCHRONOUS=FULL and pay for it.
func verifyPragmas(db *sql.DB, wantSync string) error {
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("aggregate store: read journal_mode: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		return fmt.Errorf("aggregate store: journal_mode is %q, want wal — durable ACK requires WAL", journal)
	}
	var sync int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
		return fmt.Errorf("aggregate store: read synchronous: %w", err)
	}
	want := 1 // NORMAL
	if strings.EqualFold(wantSync, "FULL") {
		want = 2
	}
	if sync != want {
		return fmt.Errorf("aggregate store: synchronous is %d, want %d (%s)", sync, want, wantSync)
	}
	return nil
}

// Path returns the database file path.
func (s *SQLiteStore) Path() string { return s.path }

// UUID returns the store's identity, minted at creation. It is what tells an
// operator "this is a different store", not "this is the same store rebuilt".
func (s *SQLiteStore) UUID() string { return s.uuid }

// lockWriter acquires the exclusive write slot.
func (s *SQLiteStore) lockWriter() { s.writeMu <- struct{}{} }

// unlockWriter releases the exclusive write slot.
func (s *SQLiteStore) unlockWriter() { <-s.writeMu }

// Close releases both pools.
func (s *SQLiteStore) Close() error {
	var errs []error
	if s.reader != nil {
		if err := s.reader.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.writer != nil {
		if err := s.writer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// --- schema -----------------------------------------------------------------

// deltaColumnList is the shared column list of the delta log and the bucket
// table. Both carry the same aggregate payload; only their keys differ.
const deltaColumnList = `point_count, error_count, duration_count, duration_sum, duration_min, duration_max,
	gauge_count, gauge_sum, gauge_min, gauge_max, gauge_last, gauge_last_ts,
	counter_delta, reset_count, log_count, first_ts, last_ts, sketch`

// deltaColumnDDL is deltaColumnList with types, shared by both tables.
const deltaColumnDDL = `point_count INTEGER NOT NULL,
	error_count INTEGER NOT NULL,
	duration_count INTEGER NOT NULL,
	duration_sum REAL NOT NULL,
	duration_min REAL NOT NULL,
	duration_max REAL NOT NULL,
	gauge_count INTEGER NOT NULL,
	gauge_sum REAL NOT NULL,
	gauge_min REAL NOT NULL,
	gauge_max REAL NOT NULL,
	gauge_last REAL NOT NULL,
	gauge_last_ts INTEGER NOT NULL,
	counter_delta REAL NOT NULL,
	reset_count INTEGER NOT NULL,
	log_count INTEGER NOT NULL,
	first_ts INTEGER NOT NULL,
	last_ts INTEGER NOT NULL,
	sketch BLOB`

// deltaValuePlaceholders matches deltaColumnList's arity.
const deltaValuePlaceholders = `?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?`

// schemaDDL is the complete aggregate schema (#162).
func schemaDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS aggregate_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) WITHOUT ROWID`,

		// The length CHECK is not decoration: a zero-length dictionary value is
		// an identity that can never be resolved back to anything, and it would
		// collide with every other empty value in its namespace. The durable
		// registrar refuses to mint one, so this constraint is unreachable from
		// the ingest path and exists to catch a corrupt or hand-edited file.
		`CREATE TABLE IF NOT EXISTS aggregate_dict (
			id INTEGER PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			kind INTEGER NOT NULL,
			value BLOB NOT NULL CHECK (length(value) > 0)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_aggregate_dict_scope
			ON aggregate_dict (tenant_id, kind, value)`,

		// Explicit identity columns, no generic variant (#162). status_class
		// carries span status for traces/edges; severity carries the log
		// severity tier. Exactly one of the two is ever non-zero, which is
		// what lets the pair round-trip onto the single SeriesKey.StatusClass
		// field the engine uses.
		`CREATE TABLE IF NOT EXISTS aggregate_series (
			id INTEGER PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			service_id INTEGER NOT NULL,
			name_id INTEGER NOT NULL,
			dims_id INTEGER NOT NULL,
			signal INTEGER NOT NULL,
			status_class INTEGER NOT NULL,
			http_class INTEGER NOT NULL,
			method INTEGER NOT NULL,
			span_kind INTEGER NOT NULL,
			severity INTEGER NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_aggregate_series_identity
			ON aggregate_series (tenant_id, service_id, name_id, dims_id, signal,
				status_class, http_class, method, span_kind, severity)`,
		`CREATE INDEX IF NOT EXISTS idx_aggregate_series_scope
			ON aggregate_series (tenant_id, signal)`,

		// Keyed by (window_start, series_id), NOT by an append sequence: each
		// group commit merges into the row an earlier commit left behind, so a
		// window holds one row per active series instead of one row per commit
		// per series. Finalization — and the writer-lock hold that comes with
		// it — is then O(active series), which is the whole point (#173).
		//
		// WITHOUT ROWID for the same reason as aggregate_buckets: the composite
		// key IS the table B-tree, so the point lookup every commit performs and
		// the window range scan every finalize performs are both leading-key
		// operations with no second structure to keep in sync.
		`CREATE TABLE IF NOT EXISTS aggregate_delta_log (
			window_start INTEGER NOT NULL,
			series_id INTEGER NOT NULL,
			` + deltaColumnDDL + `,
			PRIMARY KEY (window_start, series_id)
		) WITHOUT ROWID`,

		// WITHOUT ROWID so the composite PK IS the table B-tree: purge,
		// finalize and window reads are all leading-range operations on
		// window_start, and a duplicated rowid + unique index would cost real
		// bytes against the 5 GiB aggregate allowance at ~10M retained rows.
		`CREATE TABLE IF NOT EXISTS aggregate_buckets (
			window_start INTEGER NOT NULL,
			series_id INTEGER NOT NULL,
			` + deltaColumnDDL + `,
			PRIMARY KEY (window_start, series_id)
		) WITHOUT ROWID`,

		`CREATE TABLE IF NOT EXISTS aggregate_baseline (
			series_id INTEGER NOT NULL,
			producer_id BLOB NOT NULL,
			start_ts INTEGER NOT NULL,
			last_ts INTEGER NOT NULL,
			value REAL NOT NULL,
			PRIMARY KEY (series_id, producer_id)
		) WITHOUT ROWID`,
	}
}

// ensureSchema creates, verifies or (when permitted) rebuilds the schema.
func (s *SQLiteStore) ensureSchema(allowRebuild bool) error {
	present, err := s.presentTables()
	if err != nil {
		return err
	}
	switch {
	case len(present) == 0:
		return s.createSchema()
	case len(present) == len(aggregateTables):
		err := s.verifyMeta()
		if err == nil {
			return nil
		}
		var schemaErr *SchemaError
		if !errors.As(err, &schemaErr) || !allowRebuild {
			return err
		}
		s.warnRebuild(err.Error())
		return s.rebuild()
	default:
		missing := missingTables(present)
		err := &SchemaError{
			Reason: "partial_schema",
			Detail: fmt.Sprintf("%s is missing %s", s.path, strings.Join(missing, ", ")),
		}
		if !allowRebuild {
			return err
		}
		s.warnRebuild(err.Error())
		return s.rebuild()
	}
}

// warnRebuild logs the destructive-rebuild warning. It is deliberately loud:
// this is the one code path in the aggregate store that destroys data.
func (s *SQLiteStore) warnRebuild(reason string) {
	slog.Warn("💥 AGGREGATE_ALLOW_REBUILD=true — DESTROYING and recreating the aggregate store",
		"path", s.path,
		"reason", reason,
		"data_loss", "all aggregate history in this file is deleted; raw telemetry in the main database is untouched",
	)
}

// presentTables returns which aggregate-owned tables exist.
func (s *SQLiteStore) presentTables() ([]string, error) {
	rows, err := s.reader.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'aggregate_%'`)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: inspect schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var present []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("aggregate store: inspect schema: %w", err)
		}
		for _, want := range aggregateTables {
			if want == name {
				present = append(present, name)
				break
			}
		}
	}
	return present, rows.Err()
}

// missingTables returns the aggregate tables absent from present.
func missingTables(present []string) []string {
	have := make(map[string]struct{}, len(present))
	for _, p := range present {
		have[p] = struct{}{}
	}
	var missing []string
	for _, want := range aggregateTables {
		if _, ok := have[want]; !ok {
			missing = append(missing, want)
		}
	}
	return missing
}

// createSchema builds the schema at the current version and stamps meta.
func (s *SQLiteStore) createSchema() error {
	s.lockWriter()
	defer s.unlockWriter()
	tx, err := s.writer.Begin()
	if err != nil {
		return fmt.Errorf("aggregate store: begin create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, ddl := range schemaDDL() {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("aggregate store: create schema: %w", err)
		}
	}
	uuid, err := newStoreUUID()
	if err != nil {
		return err
	}
	meta := [][2]string{
		{"schema_version", fmt.Sprint(StoreSchemaVersion)},
		{"series_key_version", fmt.Sprint(SeriesKeyVersion)},
		{"sketch_codec_version", fmt.Sprint(SketchEncodingVersion)},
		{"store_uuid", uuid},
		{"created_at", time.Now().UTC().Format(time.RFC3339)},
	}
	for _, kv := range meta {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO aggregate_meta (key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			return fmt.Errorf("aggregate store: stamp meta %s: %w", kv[0], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aggregate store: commit create: %w", err)
	}
	s.uuid = uuid
	slog.Info("🗄️  Aggregate store created",
		"path", s.path,
		"schema_version", StoreSchemaVersion,
		"series_key_version", SeriesKeyVersion,
		"sketch_codec_version", SketchEncodingVersion,
		"store_uuid", uuid,
	)
	return nil
}

// rebuild drops only the aggregate-owned tables and recreates them.
func (s *SQLiteStore) rebuild() error {
	s.lockWriter()
	tx, err := s.writer.Begin()
	if err != nil {
		s.unlockWriter()
		return fmt.Errorf("aggregate store: begin rebuild: %w", err)
	}
	for _, table := range aggregateTables {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			_ = tx.Rollback()
			s.unlockWriter()
			return fmt.Errorf("aggregate store: drop %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		s.unlockWriter()
		return fmt.Errorf("aggregate store: commit rebuild: %w", err)
	}
	s.unlockWriter()
	return s.createSchema()
}

// verifyMeta checks the three versions this build depends on.
func (s *SQLiteStore) verifyMeta() error {
	meta := map[string]string{}
	rows, err := s.reader.Query(`SELECT key, value FROM aggregate_meta`)
	if err != nil {
		return fmt.Errorf("aggregate store: read meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return fmt.Errorf("aggregate store: read meta: %w", err)
		}
		meta[k] = v
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("aggregate store: read meta: %w", err)
	}
	checks := [][3]string{
		{"schema_version", meta["schema_version"], fmt.Sprint(StoreSchemaVersion)},
		{"series_key_version", meta["series_key_version"], fmt.Sprint(SeriesKeyVersion)},
		{"sketch_codec_version", meta["sketch_codec_version"], fmt.Sprint(SketchEncodingVersion)},
	}
	for _, c := range checks {
		if c[1] == "" {
			return &SchemaError{Reason: "missing_meta", Detail: c[0] + " is absent from aggregate_meta"}
		}
		if c[1] != c[2] {
			return &SchemaError{Reason: "version_mismatch", Key: c[0], Got: c[1], Want: c[2]}
		}
	}
	s.uuid = meta["store_uuid"]
	if s.uuid == "" {
		return &SchemaError{Reason: "missing_meta", Detail: "store_uuid is absent from aggregate_meta"}
	}
	return nil
}

// newStoreUUID mints the store's identity.
func newStoreUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("aggregate store: mint uuid: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// --- writes -----------------------------------------------------------------

// CommitGroup implements Store. One transaction carries the registrations, the
// pre-merged delta rows and the baseline upserts, so none of #162's three
// atomicity invariants can be violated by a partial write.
func (s *SQLiteStore) CommitGroup(b *GroupBatch) error {
	if b == nil || b.Empty() {
		return nil
	}
	start := time.Now()
	bytes, err := s.commitGroup(b)
	s.metrics.RecordCommit(time.Since(start), len(b.Deltas), bytes, err)
	return err
}

func (s *SQLiteStore) commitGroup(b *GroupBatch) (int64, error) {
	s.lockWriter()
	defer s.unlockWriter()

	tx, err := s.writer.Begin()
	if err != nil {
		return 0, fmt.Errorf("aggregate store: begin commit: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := insertDicts(tx, b.Dicts); err != nil {
		return 0, err
	}
	if err := insertSeries(tx, b.Series); err != nil {
		return 0, err
	}
	bytes, err := mergeDeltas(tx, b.Deltas)
	if err != nil {
		return 0, err
	}
	if err := upsertBaselines(tx, b.Baselines); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("aggregate store: commit: %w", err)
	}
	committed = true
	return bytes, nil
}

// insertDicts writes the dictionary registrations.
//
// Plain INSERT, deliberately NOT "OR IGNORE": OR IGNORE skips rows that violate
// ANY constraint, which would let a rejected registration disappear while the
// delta referencing it commits — precisely the atomicity invariant this batch
// exists to enforce. A registrar re-offers a row only after a failed commit,
// and a failed commit rolled the row back, so there is nothing to conflict
// with. A conflict here is corruption and must be loud.
func insertDicts(tx *sql.Tx, rows []DictRow) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO aggregate_dict (id, tenant_id, kind, value) VALUES (?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("aggregate store: prepare dict insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.Exec(int64(r.ID), int64(r.TenantID), int64(r.Kind), r.Value); err != nil {
			return fmt.Errorf("aggregate store: insert dict %d: %w", r.ID, err)
		}
	}
	return nil
}

// insertSeries writes the series registrations. Plain INSERT for the same
// reason as insertDicts.
func insertSeries(tx *sql.Tx, rows []SeriesRow) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO aggregate_series
		(id, tenant_id, service_id, name_id, dims_id, signal, status_class, http_class, method, span_kind, severity)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("aggregate store: prepare series insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		statusClass, severity := splitStatus(r.Key)
		if _, err := stmt.Exec(
			int64(r.ID),
			int64(r.Key.TenantID), int64(r.Key.ServiceID), int64(r.Key.NameID), int64(r.Key.DimsID),
			int64(r.Key.Signal), int64(statusClass), int64(r.Key.HTTPClass), int64(r.Key.Method),
			int64(r.Key.Variant), int64(severity),
		); err != nil {
			return fmt.Errorf("aggregate store: insert series %d: %w", r.ID, err)
		}
	}
	return nil
}

// splitStatus maps SeriesKey.StatusClass onto the schema's two explicit
// columns. Logs carry a severity tier; traces and edges carry a span status;
// metrics carry neither. Exactly one column is ever non-zero.
func splitStatus(k SeriesKey) (statusClass, severity StatusClass) {
	if k.Signal == SignalLog {
		return 0, k.StatusClass
	}
	return k.StatusClass, 0
}

// joinStatus is splitStatus in reverse.
func joinStatus(signal Signal, statusClass, severity StatusClass) StatusClass {
	if signal == SignalLog {
		return severity
	}
	return statusClass
}

// mergeDeltas folds the batch's per-(series, window) deltas into the delta log
// and returns the approximate payload it wrote.
//
// The log is keyed (window_start, series_id), so this is a read-modify-write,
// not an append: the row a previous commit left behind absorbs this commit's
// contribution. That is what holds a window's row count at O(active series)
// instead of O(commits x dirty series) and keeps FinalizeWindow's transaction —
// which runs under the writer lock every Export needs — bounded (#173).
//
// The scalar columns could be folded by a SQL UPSERT with no read, but the
// sketch blob cannot: merging two sketches means decoding both and collapsing
// bins, and this driver has no custom aggregate function to do it in SQL (the
// same constraint #162 records for bucket reads). So the read-modify-write is
// uniform across every column. The read is a point lookup into the mutable
// window's few thousand rows, which stay in the page cache.
//
// Two prepared statements, one Exec per row, and one scratch buffer reused
// across rows. Batching rows into multi-row VALUES was measured and is SLOWER
// with this driver (~105 ms vs ~87 ms for a 5,000-row commit): the cost is in
// per-parameter binding, which a wider statement does not avoid, and an
// 800-parameter statement pays extra to compile.
func mergeDeltas(tx *sql.Tx, rows []DeltaRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	sel, err := tx.Prepare(`SELECT ` + deltaColumnList + `
		FROM aggregate_delta_log WHERE window_start = ? AND series_id = ?`)
	if err != nil {
		return 0, fmt.Errorf("aggregate store: prepare delta read: %w", err)
	}
	defer func() { _ = sel.Close() }()
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO aggregate_delta_log
		(series_id, window_start, ` + deltaColumnList + `)
		VALUES (?,?,` + deltaValuePlaceholders + `)`)
	if err != nil {
		return 0, fmt.Errorf("aggregate store: prepare delta insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var (
		bytes   int64
		scratch []byte
		args    = make([]any, 0, 20)
	)
	for _, r := range rows {
		// Merge into a COPY read from the row, never into r.Delta: the caller
		// applies that same delta to the shards after the commit returns, and
		// folding the durable history into it would double-count in memory.
		d := r.Delta
		existing, err := scanDelta(sel.QueryRow(r.WindowStart, int64(r.SeriesID)).Scan, nil)
		switch {
		case err == nil:
			existing.Merge(d)
			d = existing
		case errors.Is(err, sql.ErrNoRows):
		default:
			return 0, fmt.Errorf("aggregate store: read delta (series %d, window %d): %w", r.SeriesID, r.WindowStart, err)
		}

		var sketch []byte
		if d != nil && d.Sketch != nil {
			// Safe to reuse scratch: Exec has consumed the bind before the
			// next iteration overwrites it.
			scratch = d.Sketch.AppendTo(scratch[:0])
			sketch = scratch
		}
		args = append(args[:0], int64(r.SeriesID), r.WindowStart)
		args = append(args, deltaArgs(d, sketch)...)
		if _, err := stmt.Exec(args...); err != nil {
			return 0, fmt.Errorf("aggregate store: insert delta (series %d, window %d): %w", r.SeriesID, r.WindowStart, err)
		}
		bytes += deltaRowBytes + int64(len(sketch))
	}
	return bytes, nil
}

// deltaRowBytes is the fixed on-disk cost of a delta row before its sketch:
// eighteen scalar columns plus the (window, series) key.
const deltaRowBytes = 96

// upsertBaselines writes the durable cumulative baselines (#166). They ride the
// same transaction as the deltas they justify.
func upsertBaselines(tx *sql.Tx, rows []BaselineRow) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO aggregate_baseline (series_id, producer_id, start_ts, last_ts, value)
		VALUES (?,?,?,?,?)
		ON CONFLICT(series_id, producer_id) DO UPDATE SET
			start_ts = excluded.start_ts, last_ts = excluded.last_ts, value = excluded.value`)
	if err != nil {
		return fmt.Errorf("aggregate store: prepare baseline upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.Exec(
			int64(r.SeriesID), producerBytes(r.Producer),
			nanosOf(r.Baseline.StartTime), nanosOf(r.Baseline.LastTimestamp), r.Baseline.Value,
		); err != nil {
			return fmt.Errorf("aggregate store: upsert baseline (series %d): %w", r.SeriesID, err)
		}
	}
	return nil
}

// --- finalize ---------------------------------------------------------------

// FinalizableWindows implements Store.
func (s *SQLiteStore) FinalizableWindows(cutoff int64, limit int) ([]int64, error) {
	if limit <= 0 || limit > purgeWindowsPerPass {
		limit = purgeWindowsPerPass
	}
	rows, err := s.reader.Query(
		`SELECT DISTINCT window_start FROM aggregate_delta_log WHERE window_start <= ? ORDER BY window_start LIMIT ?`,
		cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: list finalizable windows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var w int64
		if err := rows.Scan(&w); err != nil {
			return nil, fmt.Errorf("aggregate store: list finalizable windows: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// FinalizeWindow implements Store: it materializes the window's buckets and
// deletes exactly the delta rows it incorporated, in one transaction.
//
// "Exactly the incorporated rows" is a structural property of the predicate,
// not of the scheduling: the SELECT that materializes and the DELETE that
// clears the log carry the identical `window_start = ?` predicate inside one
// transaction, with the writer lock held so nothing can add to the window in
// between. The earlier sequence bound existed because the log was append-only;
// with one row per (window, series) there is no partial-append to fence off.
//
// The lock hold is O(active series in the window). Before the delta log was
// pre-merged this was O(commits x dirty series) — ~1.6M rows and 16 s of
// blocked ingestion every five minutes at 10k pts/s (#173).
//
// The delta rows are still streamed from the READ pool while the writes go to
// the writer transaction: bounded is not the same as small, and there is no
// reason to hold the whole window in memory to satisfy one transaction.
func (s *SQLiteStore) FinalizeWindow(windowStart int64) (FinalizeStats, error) {
	start := time.Now()
	stats, err := s.finalizeWindow(windowStart)
	stats.WindowStart = windowStart
	stats.Duration = time.Since(start)
	s.metrics.RecordFinalize(stats, err)
	return stats, err
}

func (s *SQLiteStore) finalizeWindow(windowStart int64) (FinalizeStats, error) {
	var stats FinalizeStats
	s.lockWriter()
	defer s.unlockWriter()

	tx, err := s.writer.Begin()
	if err != nil {
		return stats, fmt.Errorf("aggregate store: finalize %d: begin: %w", windowStart, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := s.materializeWindow(tx, windowStart, &stats); err != nil {
		return stats, fmt.Errorf("aggregate store: finalize %d: %w", windowStart, err)
	}
	if stats.DeltaRows == 0 {
		return stats, nil // nothing to finalize; the rollback stands
	}

	if _, err := tx.Exec(
		`DELETE FROM aggregate_delta_log WHERE window_start = ?`, windowStart); err != nil {
		return stats, fmt.Errorf("aggregate store: finalize %d: delete deltas: %w", windowStart, err)
	}
	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("aggregate store: finalize %d: commit: %w", windowStart, err)
	}
	committed = true
	return stats, nil
}

// materializeWindow streams the window's delta rows from the READ pool and
// writes one bucket per row into the writer transaction. The delta log holds at
// most one row per (window, series), so there is no cross-row merge left to do
// here — the merging happened on the commit path, spread across thousands of
// short transactions instead of concentrated into one long one.
//
// The statements are prepared once for the whole window: at a few thousand rows
// the per-row statement compile the old code paid was itself a measurable slice
// of the lock hold.
func (s *SQLiteStore) materializeWindow(tx *sql.Tx, windowStart int64, stats *FinalizeStats) error {
	// A window is normally finalized exactly once, and then no bucket row for
	// it exists yet. Probing aggregate_buckets per series would be thousands of
	// B-tree descents into the seven-day table to learn that; one EXISTS
	// settles it. The probe runs inside the writer transaction, so it sees
	// anything an earlier partial finalize or a recovery pass wrote.
	var probe int
	err := tx.QueryRow(
		`SELECT 1 FROM aggregate_buckets WHERE window_start = ? LIMIT 1`, windowStart).Scan(&probe)
	rewrite := false
	switch {
	case err == nil:
		rewrite = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return fmt.Errorf("probe buckets: %w", err)
	}

	var selBucket *sql.Stmt
	if rewrite {
		selBucket, err = tx.Prepare(
			`SELECT ` + deltaColumnList + ` FROM aggregate_buckets WHERE window_start = ? AND series_id = ?`)
		if err != nil {
			return fmt.Errorf("prepare bucket read: %w", err)
		}
		defer func() { _ = selBucket.Close() }()
	}
	insBucket, err := tx.Prepare(
		`INSERT OR REPLACE INTO aggregate_buckets (window_start, series_id, ` + deltaColumnList + `)
		 VALUES (?,?,` + deltaValuePlaceholders + `)`)
	if err != nil {
		return fmt.Errorf("prepare bucket write: %w", err)
	}
	defer func() { _ = insBucket.Close() }()

	rows, err := s.reader.Query(
		`SELECT series_id, `+deltaColumnList+`
		 FROM aggregate_delta_log
		 WHERE window_start = ?
		 ORDER BY series_id`, windowStart)
	if err != nil {
		return fmt.Errorf("read deltas: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		scratch []byte
		args    = make([]any, 0, 20)
	)
	for rows.Next() {
		var id int64
		d, err := scanDelta(rows.Scan, &id)
		if err != nil {
			return err
		}
		stats.DeltaRows++

		// A window finalized during downtime recovery and then again by the
		// scheduler must ADD to its bucket, not replace it.
		if selBucket != nil {
			existing, err := scanDelta(selBucket.QueryRow(windowStart, id).Scan, nil)
			switch {
			case err == nil:
				existing.Merge(d)
				d = existing
			case errors.Is(err, sql.ErrNoRows):
			default:
				return fmt.Errorf("read bucket (series %d): %w", id, err)
			}
		}

		scratch = scratch[:0]
		if d.Sketch != nil {
			scratch = d.Sketch.AppendTo(scratch)
		}
		var sketch []byte
		if len(scratch) > 0 {
			sketch = scratch
		}
		args = append(args[:0], windowStart, id)
		args = append(args, deltaArgs(d, sketch)...)
		if _, err := insBucket.Exec(args...); err != nil {
			return fmt.Errorf("write bucket (series %d): %w", id, err)
		}
		stats.Buckets++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read deltas: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read deltas: %w", err)
	}
	return nil
}

// --- retention --------------------------------------------------------------

// PurgeBefore implements Store. Deletion is per-window so retention never opens
// an unbounded transaction, and there is NO VACUUM: rolling retention on this
// file relies on range deletes, WAL checkpointing and free-page reuse (#162).
func (s *SQLiteStore) PurgeBefore(cutoff int64) (PurgeStats, error) {
	start := time.Now()
	stats, err := s.purgeBefore(cutoff)
	stats.Duration = time.Since(start)
	s.metrics.RecordPurge(stats, err)
	return stats, err
}

func (s *SQLiteStore) purgeBefore(cutoff int64) (PurgeStats, error) {
	var stats PurgeStats
	windows, err := s.purgeableWindows(cutoff)
	if err != nil {
		return stats, err
	}
	for _, w := range windows {
		s.lockWriter()
		buckets, deltas, err := purgeWindow(s.writer, w)
		s.unlockWriter()
		if err != nil {
			return stats, err
		}
		stats.Buckets += buckets
		stats.Deltas += deltas
	}

	// Baselines whose last observation predates the retention horizon can
	// never produce a correct delta again — #166 re-seeds anything older than
	// the lateness window anyway.
	s.lockWriter()
	res, err := s.writer.Exec(`DELETE FROM aggregate_baseline WHERE last_ts < ?`, cutoff*int64(time.Second))
	s.unlockWriter()
	if err != nil {
		return stats, fmt.Errorf("aggregate store: purge baselines: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil {
		stats.Baselines = n
	}
	return stats, nil
}

// purgeableWindows lists the windows below cutoff that still hold rows.
func (s *SQLiteStore) purgeableWindows(cutoff int64) ([]int64, error) {
	rows, err := s.reader.Query(
		`SELECT window_start FROM (
			SELECT DISTINCT window_start FROM aggregate_buckets WHERE window_start < ?
			UNION
			SELECT DISTINCT window_start FROM aggregate_delta_log WHERE window_start < ?
		) ORDER BY window_start LIMIT ?`, cutoff, cutoff, purgeWindowsPerPass)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: list purgeable windows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var w int64
		if err := rows.Scan(&w); err != nil {
			return nil, fmt.Errorf("aggregate store: list purgeable windows: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// purgeWindow deletes one window's buckets and any stale delta rows.
func purgeWindow(db *sql.DB, window int64) (buckets, deltas int64, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("aggregate store: purge %d: begin: %w", window, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	res, err := tx.Exec(`DELETE FROM aggregate_buckets WHERE window_start = ?`, window)
	if err != nil {
		return 0, 0, fmt.Errorf("aggregate store: purge %d: buckets: %w", window, err)
	}
	buckets, _ = res.RowsAffected()
	res, err = tx.Exec(`DELETE FROM aggregate_delta_log WHERE window_start = ?`, window)
	if err != nil {
		return 0, 0, fmt.Errorf("aggregate store: purge %d: deltas: %w", window, err)
	}
	deltas, _ = res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("aggregate store: purge %d: commit: %w", window, err)
	}
	committed = true
	return buckets, deltas, nil
}

// Analyze refreshes the planner statistics. It is the only maintenance the
// aggregate file gets: #162 excludes routine VACUUM outright, and ANALYZE is
// cheap enough to ride the existing daily maintenance tick.
func (s *SQLiteStore) Analyze() error {
	s.lockWriter()
	defer s.unlockWriter()
	if _, err := s.writer.Exec("ANALYZE"); err != nil {
		return fmt.Errorf("aggregate store: analyze: %w", err)
	}
	return nil
}

// --- reads ------------------------------------------------------------------

// ReadBuckets implements Store. The selector's bounds are mandatory and the row
// cap is enforced here, not by the caller: the worst case is 6,000 series x
// 2,016 windows and a dashboard is not trusted with it.
func (s *SQLiteStore) ReadBuckets(sel Selector) ([]Bucket, error) {
	limit, err := sel.Validate()
	if err != nil {
		return nil, err
	}
	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`SELECT b.window_start, b.series_id, `)
	for i, col := range strings.Split(deltaColumnList, ",") {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("b." + strings.TrimSpace(col))
	}
	sb.WriteString(` FROM aggregate_buckets b JOIN aggregate_series s ON s.id = b.series_id
		WHERE b.window_start >= ? AND b.window_start < ? AND s.tenant_id = ?`)
	args = append(args, sel.Start, sel.End, int64(sel.TenantID))
	if sel.Signal != SignalUnspecified {
		sb.WriteString(` AND s.signal = ?`)
		args = append(args, int64(sel.Signal))
	}
	if len(sel.SeriesIDs) > 0 {
		sb.WriteString(` AND b.series_id IN (` + placeholders(len(sel.SeriesIDs)) + `)`)
		for _, id := range sel.SeriesIDs {
			args = append(args, int64(id))
		}
	}
	sb.WriteString(` ORDER BY b.window_start, b.series_id LIMIT ?`)
	args = append(args, limit)

	rows, err := s.reader.Query(sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: read buckets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Bucket, 0, 64)
	for rows.Next() {
		var window, id int64
		d, err := scanDelta(func(dst ...any) error {
			return rows.Scan(append([]any{&window, &id}, dst...)...)
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("aggregate store: read buckets: %w", err)
		}
		out = append(out, Bucket{WindowStart: window, SeriesID: SeriesID(id), Delta: d})
	}
	return out, rows.Err()
}

// ReplayMutable implements Store. Only mutable windows are returned: finalized
// history never hydrates into RAM (#160).
func (s *SQLiteStore) ReplayMutable(since int64) ([]DeltaRow, error) {
	rows, err := s.reader.Query(
		`SELECT series_id, window_start, `+deltaColumnList+`
		 FROM aggregate_delta_log WHERE window_start >= ? ORDER BY window_start, series_id`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: replay: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DeltaRow
	for rows.Next() {
		var id, window int64
		d, err := scanDelta(func(dst ...any) error {
			return rows.Scan(append([]any{&id, &window}, dst...)...)
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("aggregate store: replay: %w", err)
		}
		out = append(out, DeltaRow{SeriesID: SeriesID(id), WindowStart: window, Delta: d})
	}
	return out, rows.Err()
}

// LoadBaselines implements Store.
func (s *SQLiteStore) LoadBaselines(max int) ([]BaselineRow, error) {
	if max <= 0 {
		max = MaxReadRows
	}
	rows, err := s.reader.Query(
		`SELECT series_id, producer_id, start_ts, last_ts, value FROM aggregate_baseline LIMIT ?`, max)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: load baselines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BaselineRow
	for rows.Next() {
		var (
			id             int64
			producer       []byte
			startTS, lastT int64
			value          float64
		)
		if err := rows.Scan(&id, &producer, &startTS, &lastT, &value); err != nil {
			return nil, fmt.Errorf("aggregate store: load baselines: %w", err)
		}
		out = append(out, BaselineRow{
			SeriesID: SeriesID(id),
			Producer: producerFromBytes(producer),
			Baseline: Baseline{
				StartTime:     timeFromNanos(startTS),
				LastTimestamp: timeFromNanos(lastT),
				Value:         value,
			},
		})
	}
	return out, rows.Err()
}

// ResolveSeries implements Store. The input count is capped at MaxReadRows.
func (s *SQLiteStore) ResolveSeries(ids []SeriesID) ([]SeriesInfo, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxReadRows {
		return nil, fmt.Errorf("%w: %d series ids, cap is %d", ErrSelectorUnbounded, len(ids), MaxReadRows)
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = int64(id)
	}
	// #nosec G202 -- the only interpolation is a placeholder run generated
	// from len(ids); every value is bound.
	query := `SELECT id, tenant_id, service_id, name_id, dims_id, signal, status_class, http_class, method, span_kind, severity
		 FROM aggregate_series WHERE id IN (` + placeholders(len(ids)) + `)`
	rows, err := s.reader.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: resolve series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]SeriesInfo, 0, len(ids))
	for rows.Next() {
		info, err := scanSeries(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("aggregate store: resolve series: %w", err)
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

// LoadDict implements Store.
func (s *SQLiteStore) LoadDict(max int) ([]DictRow, error) {
	if max <= 0 || max > maxDictRows {
		max = maxDictRows
	}
	rows, err := s.reader.Query(`SELECT id, tenant_id, kind, value FROM aggregate_dict LIMIT ?`, max)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: load dict: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DictRow
	for rows.Next() {
		var (
			id, tenant, kind int64
			value            []byte
		)
		if err := rows.Scan(&id, &tenant, &kind, &value); err != nil {
			return nil, fmt.Errorf("aggregate store: load dict: %w", err)
		}
		// #nosec G115 -- dictionary IDs, tenant IDs and kinds are written from
		// uint32/uint8 values by this package and nothing else writes the table.
		out = append(out, DictRow{
			ID:       uint32(id),
			TenantID: uint32(tenant),
			Kind:     Kind(kind),
			Value:    value,
		})
	}
	return out, rows.Err()
}

// LoadSeries implements Store.
func (s *SQLiteStore) LoadSeries(max int) ([]SeriesRow, error) {
	if max <= 0 || max > maxSeriesRows {
		max = maxSeriesRows
	}
	rows, err := s.reader.Query(
		`SELECT id, tenant_id, service_id, name_id, dims_id, signal, status_class, http_class, method, span_kind, severity
		 FROM aggregate_series LIMIT ?`, max)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: load series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SeriesRow
	for rows.Next() {
		info, err := scanSeries(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("aggregate store: load series: %w", err)
		}
		out = append(out, SeriesRow(info))
	}
	return out, rows.Err()
}

// Backlog implements Store.
func (s *SQLiteStore) Backlog() (BacklogStats, error) {
	var (
		stats  BacklogStats
		oldest sql.NullInt64
	)
	if err := s.reader.QueryRow(
		`SELECT COUNT(*), MIN(window_start) FROM aggregate_delta_log`).Scan(&stats.Rows, &oldest); err != nil {
		return stats, fmt.Errorf("aggregate store: backlog: %w", err)
	}
	if oldest.Valid {
		stats.OldestWindow = oldest.Int64
	}
	stats.Bytes = stats.Rows * deltaRowBytes
	return stats, nil
}

// --- row helpers ------------------------------------------------------------

// scanFunc is the shape of sql.Rows.Scan / sql.Row.Scan.
type scanFunc func(dst ...any) error

// scanDelta reads the shared delta column list through scan. When id is
// non-nil, it is prepended to the destination list (the delta-log read path
// that also wants the series id).
func scanDelta(scan scanFunc, id *int64) (*AggregateDelta, error) {
	var (
		d                   AggregateDelta
		gaugeLastTS         int64
		firstTS, lastTS     int64
		sketch              []byte
		dst                 []any
		pointCount, errCnt  int64
		durCount, gaugeCnt  int64
		resetCount, logCnt  int64
		durSum, durMin      float64
		durMax, gaugeSum    float64
		gaugeMin, gaugeMax  float64
		gaugeLast, counterD float64
	)
	if id != nil {
		dst = append(dst, id)
	}
	dst = append(dst,
		&pointCount, &errCnt, &durCount, &durSum, &durMin, &durMax,
		&gaugeCnt, &gaugeSum, &gaugeMin, &gaugeMax, &gaugeLast, &gaugeLastTS,
		&counterD, &resetCount, &logCnt, &firstTS, &lastTS, &sketch,
	)
	if err := scan(dst...); err != nil {
		return nil, err
	}
	d.Count = uint64(pointCount)       // #nosec G115 -- counters are written from uint64
	d.ErrorCount = uint64(errCnt)      // #nosec G115 -- counters are written from uint64
	d.DurationCount = uint64(durCount) // #nosec G115 -- counters are written from uint64
	d.GaugeCount = uint64(gaugeCnt)    // #nosec G115 -- counters are written from uint64
	d.ResetCount = uint64(resetCount)  // #nosec G115 -- counters are written from uint64
	d.LogCount = uint64(logCnt)        // #nosec G115 -- counters are written from uint64
	d.DurationSum, d.DurationMin, d.DurationMax = durSum, durMin, durMax
	d.GaugeSum, d.GaugeMin, d.GaugeMax, d.GaugeLast = gaugeSum, gaugeMin, gaugeMax, gaugeLast
	d.CounterDelta = counterD
	d.GaugeLastTime = timeFromNanos(gaugeLastTS)
	d.FirstTimestamp = timeFromNanos(firstTS)
	d.LastTimestamp = timeFromNanos(lastTS)
	if len(sketch) > 0 {
		sk, err := DecodeSketch(sketch)
		if err != nil {
			return nil, fmt.Errorf("decode sketch: %w", err)
		}
		d.Sketch = sk
	}
	return &d, nil
}

// deltaArgs renders a delta as the shared column list's bind arguments.
func deltaArgs(d *AggregateDelta, sketch []byte) []any {
	if d == nil {
		d = &AggregateDelta{}
	}
	var blob any
	if len(sketch) > 0 {
		blob = sketch
	}
	return []any{
		int64(d.Count), int64(d.ErrorCount), int64(d.DurationCount), // #nosec G115 -- counters are bounded by ingest volume
		d.DurationSum, d.DurationMin, d.DurationMax,
		int64(d.GaugeCount), d.GaugeSum, d.GaugeMin, d.GaugeMax, d.GaugeLast, nanosOf(d.GaugeLastTime), // #nosec G115
		d.CounterDelta, int64(d.ResetCount), int64(d.LogCount), // #nosec G115
		nanosOf(d.FirstTimestamp), nanosOf(d.LastTimestamp), blob,
	}
}

// scanSeries reads one aggregate_series row.
func scanSeries(scan scanFunc) (SeriesInfo, error) {
	var (
		id                                     int64
		tenant, service, name, dims            int64
		signal, statusClass, httpClass, method int64
		spanKind, severity                     int64
	)
	if err := scan(&id, &tenant, &service, &name, &dims, &signal, &statusClass, &httpClass, &method, &spanKind, &severity); err != nil {
		return SeriesInfo{}, err
	}
	sig := Signal(signal) // #nosec G115 -- signal is written from the bounded Signal enum
	return SeriesInfo{
		ID: SeriesID(id),
		Key: SeriesKey{
			TenantID:    uint32(tenant),  // #nosec G115 -- dictionary IDs are uint32
			ServiceID:   uint32(service), // #nosec G115 -- dictionary IDs are uint32
			NameID:      uint32(name),    // #nosec G115 -- dictionary IDs are uint32
			DimsID:      uint32(dims),    // #nosec G115 -- dictionary IDs are uint32
			Signal:      sig,
			StatusClass: joinStatus(sig, StatusClass(statusClass), StatusClass(severity)), // #nosec G115
			HTTPClass:   HTTPClass(httpClass),                                             // #nosec G115 -- bounded enum
			Method:      Method(method),                                                   // #nosec G115 -- bounded enum
			Variant:     Variant(spanKind),                                                // #nosec G115 -- bounded enum
		},
	}, nil
}

// placeholders returns "?,?,..." with n placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// nanosOf renders a time as Unix nanoseconds, mapping the zero time to 0 so a
// round trip preserves "unset".
func nanosOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// timeFromNanos is nanosOf in reverse.
func timeFromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// producerBytes renders a ProducerID as the schema's 8-byte BLOB.
func producerBytes(p ProducerID) []byte {
	b := make([]byte, 8)
	v := uint64(p)
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	return b
}

// producerFromBytes is producerBytes in reverse.
func producerFromBytes(b []byte) ProducerID {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return ProducerID(v)
}

// compile-time assertion that the SQLite store satisfies the interface.
var _ Store = (*SQLiteStore)(nil)
