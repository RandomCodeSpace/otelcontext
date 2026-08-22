package aggregate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	// maxTemplateRows bounds the startup log-template warm-up. The
	// per-partition cap times a sane partition count sits orders of magnitude
	// below it; it exists so a corrupted file cannot make startup allocate
	// without bound.
	maxTemplateRows = 200_000
	// sweepChunk bounds how many IDs go into one DELETE ... IN (...) so a
	// sweep never compiles a statement with a hundred thousand parameters.
	sweepChunk = 500
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
	"aggregate_log_template",
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

// PingContext verifies the aggregate database is reachable on the READ pool.
//
// The read pool, never the writer: the writer is MaxOpenConns(1) behind the
// group commit, so a ping issued there would queue behind whatever commit is
// in flight and report "unreachable" for a database that is merely busy. A
// readiness probe has to answer "can this process still read its aggregates",
// which is exactly what the read pool answers.
func (s *SQLiteStore) PingContext(ctx context.Context) error {
	if s == nil || s.reader == nil {
		return ErrStoreClosed
	}
	return s.reader.PingContext(ctx)
}

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
	counter_delta, reset_count, log_count, first_ts, last_ts, sketch,
	request_count, error_request_count,
	hist_count, hist_sum, hist_min, hist_max, hist_flags, hist_source_error,
	hist_tail_bound, hist_tail_count`

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
	sketch BLOB,
	request_count INTEGER NOT NULL,
	error_request_count INTEGER NOT NULL,
	hist_count INTEGER NOT NULL,
	hist_sum REAL NOT NULL,
	hist_min REAL NOT NULL,
	hist_max REAL NOT NULL,
	hist_flags INTEGER NOT NULL,
	hist_source_error REAL NOT NULL,
	hist_tail_bound REAL NOT NULL,
	hist_tail_count INTEGER NOT NULL`

// deltaValuePlaceholders matches deltaColumnList's arity.
const deltaValuePlaceholders = `?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?`

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

		// Durable log-template miner state (#200 Q4). The primary key IS the
		// dictionary ID minted for the template, which is also the NameID of
		// every log series that used it: one identity, three tables, no
		// second ID space to keep in step.
		//
		// There is no sample column, deliberately. Raw log bodies are a
		// credential and PII sink; exemplars already carry the raw line for
		// the cases that need one.
		`CREATE TABLE IF NOT EXISTS aggregate_log_template (
			template_id INTEGER PRIMARY KEY,
			tenant TEXT NOT NULL,
			service TEXT NOT NULL,
			pattern_version INTEGER NOT NULL,
			tokens TEXT NOT NULL,
			seq INTEGER NOT NULL,
			is_other INTEGER NOT NULL,
			alias_of INTEGER NOT NULL,
			hit_count INTEGER NOT NULL,
			first_ts INTEGER NOT NULL,
			last_ts INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_aggregate_log_template_partition
			ON aggregate_log_template (tenant, service)`,
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
		// The watermarks start at 1 — the first ID either allocator may mint,
		// since 0 is the "none" sentinel. They only ever increase (#200 Q1).
		{MetaDictWatermark, "1"},
		{MetaSeriesWatermark, "1"},
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
	if err := upsertTemplates(tx, b.Templates); err != nil {
		return 0, err
	}
	// The high-watermarks ride the same transaction as the rows whose IDs
	// they cover. Stamping them afterwards would leave a window in which a
	// committed row names an ID the next boot is free to mint again (#200 Q1).
	if err := bumpWatermarks(tx, b); err != nil {
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
		args    = make([]any, 0, 22)
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
		args    = make([]any, 0, 22)
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
//
// The cap is no longer allowed to be silent (#194 blocker 4): the query asks
// for limit+1 rows and the extra row, if it exists, becomes Truncated plus a
// resume cursor. A caller that needs completeness pages with Selector.After
// until Truncated is false; a caller that wants a scalar total should not be
// here at all and should call SumBuckets.
func (s *SQLiteStore) ReadBuckets(sel Selector) (BucketPage, error) {
	limit, err := sel.Validate()
	if err != nil {
		return BucketPage{}, err
	}
	page := BucketPage{Limit: limit}
	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`SELECT window_start, series_id, src, ` + deltaColumnList + ` FROM (`)
	args = appendBucketUnion(&sb, sel,
		`t.window_start AS window_start, t.series_id AS series_id, `+
			aliasColumns("t.", deltaColumnList), args)
	sb.WriteString(`)`)
	if !sel.After.zero() {
		// Keyset resume over the TOTAL order (window, series, source). The
		// source component is what makes it total: one window can hold a
		// materialized bucket and a not-yet-finalized delta row for the same
		// series, and an OFFSET-free resume must be able to sit between them.
		sb.WriteString(` WHERE window_start > ?
			OR (window_start = ? AND (series_id > ? OR (series_id = ? AND src > ?)))`)
		args = append(args,
			sel.After.WindowStart, sel.After.WindowStart,
			int64(sel.After.SeriesID), int64(sel.After.SeriesID), int64(sel.After.Source))
	}
	sb.WriteString(` ORDER BY window_start, series_id, src LIMIT ?`)
	args = append(args, limit+1)

	rows, err := s.reader.Query(sb.String(), args...)
	if err != nil {
		return BucketPage{}, fmt.Errorf("aggregate store: read buckets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Bucket, 0, 64)
	for rows.Next() {
		var window, id, src int64
		d, err := scanDelta(func(dst ...any) error {
			return rows.Scan(append([]any{&window, &id, &src}, dst...)...)
		}, nil)
		if err != nil {
			return BucketPage{}, fmt.Errorf("aggregate store: read buckets: %w", err)
		}
		if len(out) == limit {
			// The limit+1'th row. It is never returned; it only proves that
			// the answer is incomplete.
			page.Truncated = true
			last := out[len(out)-1]
			page.Next = BucketCursor{WindowStart: last.WindowStart, SeriesID: last.SeriesID, Source: last.Source}
			break
		}
		out = append(out, Bucket{
			WindowStart: window,
			SeriesID:    SeriesID(id),
			Delta:       d,
			Source:      BucketSource(src), // #nosec G115 -- src is a literal 0/1 in the query
		})
	}
	if err := rows.Err(); err != nil {
		return BucketPage{}, fmt.Errorf("aggregate store: read buckets: %w", err)
	}
	page.Buckets = out
	return page, nil
}

// sumColumnList is the SUMmable subset of the delta columns, in the order
// scanSumRow reads them. Sketches are absent on purpose: SQL cannot merge one.
const sumColumnList = `point_count, error_count, request_count, error_request_count,
	duration_count, duration_sum, log_count`

// SumBuckets implements Store. It is the scalar-totals path of the #197 read
// contract: the database does the SUM/COUNT and returns one row per group, so
// there is no row cap to truncate and no arithmetic for the caller to get
// wrong on a partial page.
//
// The result size is bounded by the GROUPING — at most (windows x services x
// signals), all three of which are already bounded by the retention horizon and
// the cardinality limiter — and never by the number of rows scanned. That is
// the structural difference from ReadBuckets, whose result size IS the row
// count.
func (s *SQLiteStore) SumBuckets(sel Selector, by GroupBy) ([]SumRow, error) {
	if _, err := sel.Validate(); err != nil {
		return nil, err
	}
	var (
		sb    strings.Builder
		args  []any
		group []string
	)
	sb.WriteString(`SELECT `)
	for _, g := range [...]struct {
		flag GroupBy
		col  string
	}{
		{GroupByWindow, "window_start"},
		{GroupByService, "service_id"},
		{GroupByName, "name_id"},
		{GroupBySignal, "signal"},
	} {
		if by&g.flag != 0 {
			sb.WriteString(g.col)
			group = append(group, g.col)
		} else {
			sb.WriteString("0")
		}
		sb.WriteString(", ")
	}
	sb.WriteString(`COALESCE(SUM(point_count),0), COALESCE(SUM(error_count),0),
		COALESCE(SUM(request_count),0), COALESCE(SUM(error_request_count),0),
		COALESCE(SUM(duration_count),0), COALESCE(SUM(duration_sum),0),
		COALESCE(SUM(log_count),0) FROM (`)
	args = appendBucketUnion(&sb, sel,
		`t.window_start AS window_start, s.service_id AS service_id,
			s.name_id AS name_id, s.signal AS signal, `+
			aliasColumns("t.", sumColumnList), args)
	sb.WriteString(`)`)
	if len(group) > 0 {
		sb.WriteString(` GROUP BY ` + strings.Join(group, ", "))
	}

	rows, err := s.reader.Query(sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: sum buckets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SumRow
	for rows.Next() {
		var (
			r                              SumRow
			window, service, name, signal  int64
			count, errCount, reqs, errReqs int64
			durCount, logCount             int64
			durSum                         float64
		)
		if err := rows.Scan(&window, &service, &name, &signal,
			&count, &errCount, &reqs, &errReqs, &durCount, &durSum, &logCount); err != nil {
			return nil, fmt.Errorf("aggregate store: sum buckets: %w", err)
		}
		r.WindowStart = window
		r.ServiceID = uint32(service)         // #nosec G115 -- dictionary IDs are uint32
		r.NameID = uint32(name)               // #nosec G115 -- dictionary IDs are uint32
		r.Signal = Signal(signal)             // #nosec G115 -- signal is written from the bounded Signal enum
		r.Count = uint64(count)               // #nosec G115 -- counters are written from uint64
		r.ErrorCount = uint64(errCount)       // #nosec G115 -- counters are written from uint64
		r.RequestCount = uint64(reqs)         // #nosec G115 -- counters are written from uint64
		r.ErrorRequestCount = uint64(errReqs) // #nosec G115 -- counters are written from uint64
		r.DurationCount = uint64(durCount)    // #nosec G115 -- counters are written from uint64
		r.LogCount = uint64(logCount)         // #nosec G115 -- counters are written from uint64
		r.DurationSum = durSum
		out = append(out, r)
	}
	return out, rows.Err()
}

// bucketSources are the two durable tables a store-owned row can live in, in
// scan order. aggregate_buckets holds what finalization materialized;
// aggregate_delta_log holds what it has not incorporated yet — including a
// window a forced closed-window eviction handed to the store before the
// finalizer reached it (#194 blocker 6). FinalizeWindow deletes exactly the
// delta rows it merges, in the same transaction, so a contribution is visible
// in exactly one of the two tables and reading both neither double-counts nor
// omits.
var bucketSources = [...]struct {
	table string
	src   BucketSource
}{
	{"aggregate_buckets", SourceFinalized},
	{"aggregate_delta_log", SourceDelta},
}

// appendBucketUnion writes the UNION ALL over both durable tables with the
// given per-row projection and appends its bind arguments. The projection is
// evaluated against alias `t` (the table) and `s` (its aggregate_series join).
func appendBucketUnion(sb *strings.Builder, sel Selector, projection string, args []any) []any {
	for i, src := range bucketSources {
		if i > 0 {
			sb.WriteString(` UNION ALL `)
		}
		fmt.Fprintf(sb, `SELECT %d AS src, %s FROM %s t
			JOIN aggregate_series s ON s.id = t.series_id
			WHERE t.window_start >= ? AND t.window_start < ? AND s.tenant_id = ?`,
			src.src, projection, src.table)
		args = append(args, sel.Start, sel.End, int64(sel.TenantID))
		if sel.Signal != SignalUnspecified {
			sb.WriteString(` AND s.signal = ?`)
			args = append(args, int64(sel.Signal))
		}
		if len(sel.SeriesIDs) > 0 {
			sb.WriteString(` AND t.series_id IN (` + placeholders(len(sel.SeriesIDs)) + `)`)
			for _, id := range sel.SeriesIDs {
				args = append(args, int64(id))
			}
		}
		if sel.SketchOnly {
			sb.WriteString(` AND t.sketch IS NOT NULL`)
		}
	}
	return args
}

// aliasColumns qualifies every column in a comma-separated list with prefix and
// re-aliases it to its bare name. The alias is not cosmetic: these projections
// feed a derived table whose columns the outer SELECT addresses by name, and
// SQLite does not promise a stable result-column name for an unaliased
// qualified expression.
func aliasColumns(prefix, list string) string {
	cols := strings.Split(list, ",")
	for i, col := range cols {
		name := strings.TrimSpace(col)
		cols[i] = prefix + name + " AS " + name
	}
	return strings.Join(cols, ", ")
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

// ReadFinalizedSince implements Store. It reads MATERIALIZED bucket rows only:
// the delta log holds the mutable set, which recovery replays through its own
// path, so reading both here would fold the same contribution twice.
//
// Newest window first. The cap is the store's own row cap, applied with the
// limit+1 probe every read in this file uses, so a caller learns that its
// horizon was cut instead of silently receiving a partial service map.
func (s *SQLiteStore) ReadFinalizedSince(since int64, signals []Signal, limit int) (FinalizedPage, error) {
	if limit <= 0 || limit > MaxReadRows {
		limit = MaxReadRows
	}
	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`SELECT t.window_start, t.series_id, ` + aliasColumns("t.", deltaColumnList) +
		` FROM aggregate_buckets t JOIN aggregate_series s ON s.id = t.series_id
		  WHERE t.window_start >= ?`)
	args = append(args, since)
	if len(signals) > 0 {
		sb.WriteString(` AND s.signal IN (` + placeholders(len(signals)) + `)`)
		for _, sig := range signals {
			args = append(args, int64(sig))
		}
	}
	sb.WriteString(` ORDER BY t.window_start DESC, t.series_id LIMIT ?`)
	args = append(args, limit+1)

	rows, err := s.reader.Query(sb.String(), args...)
	if err != nil {
		return FinalizedPage{}, fmt.Errorf("aggregate store: read finalized: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := FinalizedPage{Buckets: make([]Bucket, 0, 64)}
	for rows.Next() {
		var window, id int64
		d, err := scanDelta(func(dst ...any) error {
			return rows.Scan(append([]any{&window, &id}, dst...)...)
		}, nil)
		if err != nil {
			return FinalizedPage{}, fmt.Errorf("aggregate store: read finalized: %w", err)
		}
		if len(page.Buckets) == limit {
			// The limit+1'th row is never returned; it only proves the answer
			// is incomplete.
			page.Truncated = true
			break
		}
		page.Buckets = append(page.Buckets, Bucket{
			WindowStart: window,
			SeriesID:    SeriesID(id),
			Delta:       d,
			Source:      SourceFinalized,
		})
	}
	if err := rows.Err(); err != nil {
		return FinalizedPage{}, fmt.Errorf("aggregate store: read finalized: %w", err)
	}
	return page, nil
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
	return scanDictRows(s.reader, dictLimit(max))
}

// LoadSeries implements Store.
func (s *SQLiteStore) LoadSeries(max int) ([]SeriesRow, error) {
	return scanSeriesRows(s.reader, seriesLimit(max))
}

// dictLimit and seriesLimit clamp a caller's row limit to one row past the
// supported bound. The extra row is what lets a caller distinguish "the whole
// table" from "as much of it as fit", which a bare LIMIT cannot (#200 Q3).
func dictLimit(max int) int {
	if max <= 0 || max > MaxDictRows+1 {
		return MaxDictRows + 1
	}
	return max
}

func seriesLimit(max int) int {
	if max <= 0 || max > MaxSeriesRows+1 {
		return MaxSeriesRows + 1
	}
	return max
}

// queryer is the read surface shared by the pool and a read transaction, so
// the identity scans can run either standalone or inside one snapshot.
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// scanDictRows reads the dictionary table through q.
func scanDictRows(q queryer, limit int) ([]DictRow, error) {
	rows, err := q.Query(`SELECT id, tenant_id, kind, value FROM aggregate_dict ORDER BY id LIMIT ?`, limit)
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

// scanSeriesRows reads the series table through q.
func scanSeriesRows(q queryer, limit int) ([]SeriesRow, error) {
	rows, err := q.Query(
		`SELECT id, tenant_id, service_id, name_id, dims_id, signal, status_class, http_class, method, span_kind, severity
		 FROM aggregate_series ORDER BY id LIMIT ?`, limit)
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
		d                    AggregateDelta
		gaugeLastTS          int64
		firstTS, lastTS      int64
		sketch               []byte
		dst                  []any
		pointCount, errCnt   int64
		durCount, gaugeCnt   int64
		resetCount, logCnt   int64
		reqCount, errReqCnt  int64
		durSum, durMin       float64
		durMax, gaugeSum     float64
		gaugeMin, gaugeMax   float64
		gaugeLast, counterD  float64
		histCount, histFlags int64
		histTailCount        int64
		histSum, histMin     float64
		histMax, histSrcErr  float64
		histTailBound        float64
	)
	if id != nil {
		dst = append(dst, id)
	}
	dst = append(dst,
		&pointCount, &errCnt, &durCount, &durSum, &durMin, &durMax,
		&gaugeCnt, &gaugeSum, &gaugeMin, &gaugeMax, &gaugeLast, &gaugeLastTS,
		&counterD, &resetCount, &logCnt, &firstTS, &lastTS, &sketch,
		&reqCount, &errReqCnt,
		&histCount, &histSum, &histMin, &histMax, &histFlags, &histSrcErr,
		&histTailBound, &histTailCount,
	)
	if err := scan(dst...); err != nil {
		return nil, err
	}
	d.Count = uint64(pointCount)            // #nosec G115 -- counters are written from uint64
	d.ErrorCount = uint64(errCnt)           // #nosec G115 -- counters are written from uint64
	d.DurationCount = uint64(durCount)      // #nosec G115 -- counters are written from uint64
	d.GaugeCount = uint64(gaugeCnt)         // #nosec G115 -- counters are written from uint64
	d.ResetCount = uint64(resetCount)       // #nosec G115 -- counters are written from uint64
	d.LogCount = uint64(logCnt)             // #nosec G115 -- counters are written from uint64
	d.RequestCount = uint64(reqCount)       // #nosec G115 -- counters are written from uint64
	d.ErrorRequestCount = uint64(errReqCnt) // #nosec G115 -- counters are written from uint64
	d.DurationSum, d.DurationMin, d.DurationMax = durSum, durMin, durMax
	d.GaugeSum, d.GaugeMin, d.GaugeMax, d.GaugeLast = gaugeSum, gaugeMin, gaugeMax, gaugeLast
	d.CounterDelta = counterD
	d.GaugeLastTime = timeFromNanos(gaugeLastTS)
	d.HistogramCount = uint64(histCount)         // #nosec G115 -- counters are written from uint64
	d.HistogramTailCount = uint64(histTailCount) // #nosec G115 -- counters are written from uint64
	d.HistogramFlags = uint32(histFlags)         // #nosec G115 -- flags are written from uint32
	d.HistogramSum, d.HistogramMin, d.HistogramMax = histSum, histMin, histMax
	d.HistogramSourceError, d.HistogramTailBound = histSrcErr, histTailBound
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
		int64(d.RequestCount), int64(d.ErrorRequestCount), // #nosec G115 -- counters are bounded by ingest volume
		int64(d.HistogramCount), d.HistogramSum, d.HistogramMin, d.HistogramMax, // #nosec G115 -- counters are bounded by ingest volume
		int64(d.HistogramFlags), d.HistogramSourceError, // #nosec G115 -- a uint32 flags word always fits int64
		d.HistogramTailBound, int64(d.HistogramTailCount), // #nosec G115 -- counters are bounded by ingest volume
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

// compile-time assertion that the SQLite store satisfies the contract.
var (
	_ Store          = (*SQLiteStore)(nil)
	_ WatermarkStore = (*SQLiteStore)(nil)
	_ GCStore        = (*SQLiteStore)(nil)
)

// --- identity persistence and GC (#200) -------------------------------------

// upsertTemplates writes the identity-critical half of the log-template miner
// state. It rides the same transaction as the delta that used the identity.
//
// The pattern is written only when this row's pattern_version is at least the
// stored one, so a re-offered row from a failed commit can never roll a newer
// generalization backwards.
func upsertTemplates(tx *sql.Tx, rows []TemplateRow) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO aggregate_log_template
		(template_id, tenant, service, pattern_version, tokens, seq, is_other, alias_of, hit_count, first_ts, last_ts)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(template_id) DO UPDATE SET
			pattern_version = excluded.pattern_version,
			tokens          = excluded.tokens,
			alias_of        = excluded.alias_of,
			hit_count       = MAX(hit_count, excluded.hit_count),
			last_ts         = MAX(last_ts, excluded.last_ts)
		WHERE excluded.pattern_version >= aggregate_log_template.pattern_version`)
	if err != nil {
		return fmt.Errorf("aggregate store: prepare template upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.Exec(
			int64(r.ID), r.Tenant, r.Service, int64(r.PatternVersion), r.Tokens,
			// #nosec G115 -- partition-local ordinal, bounded by the per-service cap
			int64(r.Seq), boolToInt(r.IsOther), int64(r.AliasOf),
			// #nosec G115 -- a hit count large enough to wrap int64 is not reachable
			int64(r.Count), r.FirstSeen, r.LastSeen,
		); err != nil {
			return fmt.Errorf("aggregate store: upsert template %d: %w", r.ID, err)
		}
	}
	return nil
}

// clampUint64 reads a counter column that this package only ever writes as a
// uint64. A negative value means the file was hand-edited or corrupted; zero
// is the honest reading, and it costs a count rather than an identity.
func clampUint64(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// boolToInt renders a bool as SQLite's 0/1.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// bumpWatermarks raises the persisted identity high-watermarks to cover every
// ID this batch made durable. They never decrease.
func bumpWatermarks(tx *sql.Tx, b *GroupBatch) error {
	var dictWM, seriesWM int64
	for _, r := range b.Dicts {
		if int64(r.ID)+1 > dictWM {
			dictWM = int64(r.ID) + 1
		}
	}
	for _, r := range b.Templates {
		if int64(r.ID)+1 > dictWM {
			dictWM = int64(r.ID) + 1
		}
	}
	for _, r := range b.Series {
		if int64(r.ID)+1 > seriesWM {
			seriesWM = int64(r.ID) + 1
		}
	}
	for _, kv := range []struct {
		key   string
		value int64
	}{{MetaDictWatermark, dictWM}, {MetaSeriesWatermark, seriesWM}} {
		if kv.value <= 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO aggregate_meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value
			WHERE CAST(excluded.value AS INTEGER) > CAST(aggregate_meta.value AS INTEGER)`,
			kv.key, strconv.FormatInt(kv.value, 10)); err != nil {
			return fmt.Errorf("aggregate store: bump %s: %w", kv.key, err)
		}
	}
	return nil
}

// Watermarks implements WatermarkStore.
func (s *SQLiteStore) Watermarks() (uint32, SeriesID, error) {
	var dictWM, seriesWM int64
	for _, spec := range []struct {
		key string
		dst *int64
	}{{MetaDictWatermark, &dictWM}, {MetaSeriesWatermark, &seriesWM}} {
		var raw sql.NullString
		err := s.reader.QueryRow(`SELECT value FROM aggregate_meta WHERE key = ?`, spec.key).Scan(&raw)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return 0, 0, fmt.Errorf("aggregate store: read %s: %w", spec.key, err)
		}
		if !raw.Valid {
			continue
		}
		n, convErr := strconv.ParseInt(strings.TrimSpace(raw.String), 10, 64)
		if convErr != nil || n < 0 {
			return 0, 0, &SchemaError{Reason: "missing_meta", Detail: spec.key + " is not a non-negative integer"}
		}
		*spec.dst = n
	}
	if dictWM > int64(math.MaxUint32) {
		return 0, 0, &SchemaError{Reason: "missing_meta", Detail: MetaDictWatermark + " exceeds the uint32 dictionary ID space"}
	}
	// #nosec G115 -- bounded by the MaxUint32 check directly above
	return uint32(dictWM), SeriesID(seriesWM), nil
}

// GCSnapshot implements GCStore. Every scan runs inside ONE deferred read
// transaction, which in WAL mode pins a single snapshot of the file for its
// whole life — the reference set and the identity tables therefore describe
// the same instant. No writer lock is taken: a commit may land during the
// scan, and the barrier's revalidation is what accounts for it.
func (s *SQLiteStore) GCSnapshot() (*GCSnapshot, error) {
	tx, err := s.reader.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: begin gc snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	snap := &GCSnapshot{Referenced: make(map[SeriesID]struct{}, 1024)}
	for _, q := range []string{
		`SELECT DISTINCT series_id FROM aggregate_buckets`,
		`SELECT DISTINCT series_id FROM aggregate_delta_log`,
		`SELECT DISTINCT series_id FROM aggregate_baseline`,
	} {
		if err := collectSeriesIDs(tx, q, snap.Referenced); err != nil {
			return nil, err
		}
	}
	if snap.Series, err = scanSeriesRows(tx, MaxSeriesRows+1); err != nil {
		return nil, err
	}
	if len(snap.Series) > MaxSeriesRows {
		return nil, &PreloadError{Table: "aggregate_series", Rows: len(snap.Series), Max: MaxSeriesRows}
	}
	if snap.Dict, err = scanDictRows(tx, MaxDictRows+1); err != nil {
		return nil, err
	}
	if len(snap.Dict) > MaxDictRows {
		return nil, &PreloadError{Table: "aggregate_dict", Rows: len(snap.Dict), Max: MaxDictRows}
	}
	if snap.Templates, err = scanTemplateRows(tx, maxTemplateRows+1); err != nil {
		return nil, err
	}
	return snap, nil
}

// collectSeriesIDs folds one series-id projection into dst.
func collectSeriesIDs(q queryer, query string, dst map[SeriesID]struct{}) error {
	rows, err := q.Query(query)
	if err != nil {
		return fmt.Errorf("aggregate store: scan series references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("aggregate store: scan series references: %w", err)
		}
		dst[SeriesID(id)] = struct{}{}
	}
	return rows.Err()
}

// LoadTemplates implements GCStore.
func (s *SQLiteStore) LoadTemplates(max int) ([]TemplateRow, error) {
	return scanTemplateRows(s.reader, templateLimit(max))
}

// templateLimit clamps a template row limit to one past the supported bound.
func templateLimit(max int) int {
	if max <= 0 || max > maxTemplateRows+1 {
		return maxTemplateRows + 1
	}
	return max
}

// scanTemplateRows reads the log-template table through q.
func scanTemplateRows(q queryer, limit int) ([]TemplateRow, error) {
	rows, err := q.Query(`SELECT template_id, tenant, service, pattern_version, tokens,
		seq, is_other, alias_of, hit_count, first_ts, last_ts
		FROM aggregate_log_template ORDER BY template_id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("aggregate store: load templates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TemplateRow
	for rows.Next() {
		var (
			r                                          TemplateRow
			id, version, seq, isOther, alias, hitCount int64
		)
		if err := rows.Scan(&id, &r.Tenant, &r.Service, &version, &r.Tokens,
			&seq, &isOther, &alias, &hitCount, &r.FirstSeen, &r.LastSeen); err != nil {
			return nil, fmt.Errorf("aggregate store: load templates: %w", err)
		}
		// #nosec G115 -- every id column is written from a uint32 by this
		// package and nothing else writes the table.
		r.ID, r.PatternVersion, r.AliasOf = uint32(id), uint32(version), uint32(alias)
		r.Seq, r.Count = clampUint64(seq), clampUint64(hitCount)
		r.IsOther = isOther != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregate store: load templates: %w", err)
	}
	if len(out) > maxTemplateRows {
		return nil, &PreloadError{Table: "aggregate_log_template", Rows: len(out), Max: maxTemplateRows}
	}
	return out, nil
}

// SaveTemplateStats implements GCStore. Statistics only: it never creates a
// row, so a template swept between the dirty mark and this write stays swept.
func (s *SQLiteStore) SaveTemplateStats(rows []TemplateStatRow) error {
	if len(rows) == 0 {
		return nil
	}
	s.lockWriter()
	defer s.unlockWriter()
	tx, err := s.writer.Begin()
	if err != nil {
		return fmt.Errorf("aggregate store: begin template stats: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	stmt, err := tx.Prepare(`UPDATE aggregate_log_template
		SET hit_count = ?, first_ts = ?, last_ts = ? WHERE template_id = ?`)
	if err != nil {
		return fmt.Errorf("aggregate store: prepare template stats: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		// #nosec G115 -- a hit count large enough to wrap int64 is not reachable
		if _, err := stmt.Exec(int64(r.Count), r.FirstSeen, r.LastSeen, int64(r.ID)); err != nil {
			return fmt.Errorf("aggregate store: template stats %d: %w", r.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aggregate store: commit template stats: %w", err)
	}
	committed = true
	return nil
}

// SweepIdentities implements GCStore: one transaction, series first, then the
// dictionary rows the swept series released, then the template rows those
// dictionary IDs backed.
//
// The order matters for exactly one reason: a crash between two transactions
// could leave a series row naming a deleted dictionary ID. Inside one
// transaction there is no such window, and the ordering is kept anyway so the
// intent survives a future implementation that cannot span tables.
func (s *SQLiteStore) SweepIdentities(series []SeriesID, dict []uint32, templates []uint32) (SweepStats, error) {
	var stats SweepStats
	if len(series) == 0 && len(dict) == 0 && len(templates) == 0 {
		return stats, nil
	}
	start := time.Now()
	s.lockWriter()
	defer s.unlockWriter()
	tx, err := s.writer.Begin()
	if err != nil {
		return stats, fmt.Errorf("aggregate store: begin sweep: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	seriesIDs := make([]int64, len(series))
	for i, id := range series {
		seriesIDs[i] = int64(id)
	}
	dictIDs := make([]int64, len(dict))
	for i, id := range dict {
		dictIDs[i] = int64(id)
	}
	templateIDs := make([]int64, len(templates))
	for i, id := range templates {
		templateIDs[i] = int64(id)
	}
	// The DELETE prefixes are literals, not composed from identifiers: SQLite
	// cannot bind a table name, so the only safe way to vary one is not to.
	for _, step := range []struct {
		what   string
		prefix string
		ids    []int64
		dst    *int64
	}{
		{"aggregate_series", `DELETE FROM aggregate_series WHERE id IN (`, seriesIDs, &stats.Series},
		{"aggregate_dict", `DELETE FROM aggregate_dict WHERE id IN (`, dictIDs, &stats.Dict},
		{"aggregate_log_template", `DELETE FROM aggregate_log_template WHERE template_id IN (`, templateIDs, &stats.Templates},
	} {
		n, err := deleteByID(tx, step.what, step.prefix, step.ids)
		if err != nil {
			return stats, err
		}
		*step.dst = n
	}
	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("aggregate store: commit sweep: %w", err)
	}
	committed = true
	stats.Duration = time.Since(start)
	return stats, nil
}

// deleteByID removes rows by primary key in bounded chunks. prefix is a
// literal "DELETE FROM <table> WHERE <col> IN (" and what names the table for
// error messages only.
func deleteByID(tx *sql.Tx, what, prefix string, ids []int64) (int64, error) {
	var total int64
	for start := 0; start < len(ids); start += sweepChunk {
		end := start + sweepChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		res, err := tx.Exec(prefix+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return total, fmt.Errorf("aggregate store: sweep %s: %w", what, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	return total, nil
}
