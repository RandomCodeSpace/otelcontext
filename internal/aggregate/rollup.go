package aggregate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	rollupHour    = int64(3600)
	rollupBatch   = 128
	rollupVersion = "1"
)

var errRollupInvalidated = errors.New("aggregate rollup invalidated during build")

// hourStart uses floor division, including for pre-epoch stored windows. Public
// selectors currently reject nonpositive timestamps, but retention does not.
func hourStart(t int64) int64 { return t - (t%rollupHour+rollupHour)%rollupHour }

// This expression avoids SQLite's truncation toward zero for negative times.
func rollupHourSQL(value string) string {
	return `(` + value + ` - ((` + value + ` % 3600 + 3600) % 3600))`
}

func rollupTriggerDDL(event string) string {
	invalidate := func(row string) string {
		return `DELETE FROM aggregate_rollup_hours WHERE window_start = ` + rollupHourSQL(row+`.window_start`) + `;`
	}
	body := ""
	if event != "INSERT" {
		body += invalidate("OLD")
	}
	if event != "DELETE" {
		body += invalidate("NEW")
	}
	return `CREATE TRIGGER aggregate_rollup_` + strings.ToLower(event) +
		` AFTER ` + event + ` ON aggregate_buckets BEGIN ` + body + ` END`
}

// The extension deliberately does not join aggregateTables or bump its version:
// those paths can rebuild authoritative data. Persistent triggers also execute
// for schema-v5 binaries that know nothing about rollups.
func (s *SQLiteStore) ensureRollupSchema() error {
	return s.rollupWrite(context.Background(), func(tx *sql.Tx) error {
		var hourlyExists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master
    WHERE type='table' AND name='aggregate_hourly' COLLATE NOCASE)`).Scan(&hourlyExists); err != nil {
			return err
		}
		ddl := []string{
			`CREATE TABLE IF NOT EXISTS aggregate_rollup_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS aggregate_rollup_hours(
    window_start INTEGER PRIMARY KEY, state INTEGER NOT NULL,
    build_id TEXT NOT NULL, sketch_safe INTEGER NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS aggregate_hourly(
    window_start INTEGER NOT NULL, series_id INTEGER NOT NULL, ` + deltaColumnDDL + `,
    PRIMARY KEY(window_start, series_id)) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS idx_aggregate_hourly_summary ON aggregate_hourly
    (window_start, series_id, point_count, error_count, request_count,
     error_request_count, duration_count, duration_sum, log_count)`,
		}
		for _, q := range ddl {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		var version string
		err := tx.QueryRow(`SELECT value FROM aggregate_rollup_meta WHERE key='version'`).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && version != rollupVersion {
			return fmt.Errorf("unsupported rollup version %q", version)
		}
		var owner string
		err = tx.QueryRow(`SELECT value FROM aggregate_rollup_meta WHERE key='store_uuid'`).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Recreating derived storage cannot preserve its old completion claims.
		invalidate := !hourlyExists || owner != s.uuid
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "aggregate_rollup_" + strings.ToLower(event)
			var definition string
			err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&definition)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if definition != rollupTriggerDDL(event) {
				invalidate = true
				if _, err := tx.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
					return err
				}
				if _, err := tx.Exec(rollupTriggerDDL(event)); err != nil {
					return err
				}
			}
		}
		if invalidate {
			if _, err := tx.Exec(`UPDATE aggregate_rollup_hours SET state=0, build_id='', sketch_safe=0`); err != nil {
				return err
			}
		}
		// Abandoned builds remain invisible and are retried by the worker.
		for _, kv := range [][2]string{{"version", rollupVersion}, {"store_uuid", s.uuid}} {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO aggregate_rollup_meta VALUES (?,?)`, kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLiteStore) rollupWrite(ctx context.Context, fn func(*sql.Tx) error) error {
	select {
	case s.writeMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer s.unlockWriter()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) startRollups() {
	ctx, cancel := context.WithCancel(context.Background())
	s.rollupCancel = cancel
	s.rollupDone = make(chan struct{})
	go func() {
		defer close(s.rollupDone)
		// Opening a store never waits for history. Let recovery and startup expiry
		// run before competing for the read pool and writer.
		if err := rollupPause(ctx, time.Second); err != nil {
			return
		}

		work := summaryWorker{sourceAfter: -1 << 63, summaryAfter: -1 << 63}
		var refresh time.Time
		for ctx.Err() == nil {
			now := time.Now()
			if !now.Before(refresh) {
				if err := s.discoverRollupHours(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("Aggregate hourly discovery deferred", "error", err)
				}
				work.sourceAfter, work.summaryAfter = -1<<63, -1<<63
				refresh = now.Add(time.Minute)
			}
			active, err := s.summaryWorkerStep(ctx, now, &work, s.buildRollupHour)
			if err != nil && ctx.Err() == nil {
				slog.Warn("Aggregate rollup work deferred; history reads use original rows", "error", err)
			}
			pause := 25 * time.Millisecond
			if !active {
				pause = time.Until(refresh)
			}
			if err := rollupPause(ctx, pause); err != nil {
				return
			}
		}
	}()
}

func (s *SQLiteStore) stopRollups() {
	if s.rollupCancel != nil {
		s.rollupCancel()
		<-s.rollupDone
	}
}

func rollupPause(ctx context.Context, pause time.Duration) error {
	if pause <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Primary-key seeks discover populated hours without scanning retained rows or
// allocating a seven-day fixture. Repeating discovery after restart is cheap.
func (s *SQLiteStore) discoverRollupHours(ctx context.Context) error {
	// Derived rows are also discovered: a purge can remove the last bucket and
	// its completion marker while leaving invisible derived rows to clean up.
	for _, table := range []string{"aggregate_buckets", "aggregate_hourly"} {
		var after int64 = -1 << 63
		for {
			var window int64
			err := s.reader.QueryRowContext(ctx, `SELECT window_start FROM `+table+`
    WHERE window_start>=? ORDER BY window_start LIMIT 1`, after).Scan(&window)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			hour := hourStart(window)
			if err := s.rollupWrite(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO aggregate_rollup_hours VALUES (?,0,'',0)`, hour)
				return err
			}); err != nil {
				return err
			}
			if hour > (1<<63-1)-rollupHour {
				break
			}
			after = hour + rollupHour
		}
	}
	return nil
}

func (s *SQLiteStore) buildPendingRollups(ctx context.Context, now time.Time, pause time.Duration) error {
	// Advancing the cursor also avoids retrying an invalidated or hot hour in a
	// tight loop. The next pass retries it, as it does after a crash.
	var after int64 = -1 << 63
	cutoff := now.Unix() - int64(AllowedLateness/time.Second) - rollupHour
	for {
		var hour int64
		err := s.reader.QueryRowContext(ctx, `SELECT window_start FROM aggregate_rollup_hours
   WHERE state<>2 AND window_start>=? AND window_start<=? ORDER BY window_start LIMIT 1`, after, cutoff).Scan(&hour)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.buildRollupHour(ctx, hour, pause); err != nil && !errors.Is(err, errRollupInvalidated) {
			return err
		}
		after = hour + rollupHour
	}
}

func checkRollupBuild(ctx context.Context, tx *sql.Tx, hour int64, token string) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM aggregate_rollup_hours WHERE window_start=? AND state=1 AND build_id=?`, hour, token).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return errRollupInvalidated
	}
	return err
}

// buildRollupHour never holds the writer while reading/merging history. The
// persistent token is invalidated by any bucket mutation, including another
// process. Only a still-current token may write chunks or publish completion.
func (s *SQLiteStore) buildRollupHour(ctx context.Context, hour int64, pause time.Duration) error {
	// Also bound snapshot age and WAL retention if the identity table is much
	// larger than the ordinary active-series budget. An overdue build falls back.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := newStoreUUID()
	if err != nil {
		return err
	}
	err = s.rollupWrite(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO aggregate_rollup_hours(window_start,state,build_id,sketch_safe) VALUES (?,1,?,0)
		 ON CONFLICT(window_start) DO UPDATE SET state=1,build_id=excluded.build_id,sketch_safe=0 WHERE state<>2`, hour, token)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			return errRollupInvalidated
		}
		return err
	})
	if err != nil {
		return err
	}
	for {
		var deleted int64
		err := s.rollupWrite(ctx, func(tx *sql.Tx) error {
			if err := checkRollupBuild(ctx, tx, hour, token); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `DELETE FROM aggregate_hourly WHERE window_start=? AND series_id IN
    (SELECT series_id FROM aggregate_hourly WHERE window_start=? LIMIT ?)`, hour, hour, rollupBatch)
			if err != nil {
				return err
			}
			deleted, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return err
		}
		if deleted < rollupBatch {
			break
		}
		if err := rollupPause(ctx, pause); err != nil {
			return err
		}
	}
	snapshot, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = snapshot.Rollback() }()
	var after SeriesID
	total := 0
	sketchSafe := true
	for {
		ids, err := rollupSeriesBatch(ctx, snapshot, after)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		batch, sourceSafe, err := readRollupBatch(ctx, snapshot, hour, ids)
		if err != nil {
			return err
		}
		sketchSafe = sketchSafe && sourceSafe
		for _, row := range batch {
			if sk := row.Delta.Sketch; sk != nil && (sk.Collapsed() || sk.Saturations() != 0) {
				sketchSafe = false
			}
		}
		if len(batch) > 0 {
			err := s.rollupWrite(ctx, func(tx *sql.Tx) error {
				if err := checkRollupBuild(ctx, tx, hour, token); err != nil {
					return err
				}
				stmt, err := tx.PrepareContext(ctx, `INSERT INTO aggregate_hourly(window_start, series_id, `+deltaColumnList+`) VALUES (?,?,`+deltaValuePlaceholders+`)`)
				if err != nil {
					return err
				}
				defer func() { _ = stmt.Close() }()
				for _, row := range batch {
					var blob []byte
					if row.Delta.Sketch != nil {
						blob = row.Delta.Sketch.AppendTo(nil)
					}
					args := append([]any{hour, row.SeriesID}, deltaArgs(row.Delta, blob)...)
					if _, err := stmt.ExecContext(ctx, args...); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			total += len(batch)
		}
		after = ids[len(ids)-1]
		if err := rollupPause(ctx, pause); err != nil {
			return err
		}
	}
	// Release the read snapshot before publication/checkpointing.
	if err := snapshot.Rollback(); err != nil {
		return err
	}
	return s.rollupWrite(ctx, func(tx *sql.Tx) error {
		if err := checkRollupBuild(ctx, tx, hour, token); err != nil {
			return err
		}
		if total == 0 {
			_, err := tx.ExecContext(ctx, `DELETE FROM aggregate_rollup_hours WHERE window_start=?`, hour)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE aggregate_rollup_hours SET state=2, sketch_safe=? WHERE window_start=?`, sketchSafe, hour)
		return err
	})
}

func rollupSeriesBatch(ctx context.Context, tx *sql.Tx, after SeriesID) ([]SeriesID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM aggregate_series WHERE id>? ORDER BY id LIMIT ?`, after, rollupBatch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []SeriesID
	for rows.Next() {
		var id SeriesID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func readRollupBatch(ctx context.Context, tx *sql.Tx, hour int64, ids []SeriesID) ([]DeltaRow, bool, error) {
	args := []any{hour, hour + rollupHour}
	for _, id := range ids {
		args = append(args, id)
	}
	// The existing series/window index bounds each of these 128 range probes.
	// Sorting in index order gives deterministic gauge timestamp ties and sums.
	rows, err := tx.QueryContext(ctx, `SELECT series_id, `+deltaColumnList+`
  FROM aggregate_buckets INDEXED BY idx_aggregate_buckets_series_window
  WHERE window_start>=? AND window_start<? AND series_id IN (`+placeholders(len(ids))+`)
  ORDER BY series_id, window_start`, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var batch []DeltaRow
	safe := true
	for rows.Next() {
		var id int64
		d, err := scanDelta(rows.Scan, &id)
		if err != nil {
			return nil, false, err
		}
		if sk := d.Sketch; sk != nil && (sk.Scale() != SketchDefaultScale || sk.Collapsed() || sk.Saturations() != 0) {
			safe = false
		}
		if len(batch) == 0 || batch[len(batch)-1].SeriesID != SeriesID(id) {
			batch = append(batch, DeltaRow{SeriesID: SeriesID(id), WindowStart: hour, Delta: d})
		} else {
			acc := batch[len(batch)-1].Delta
			// SumBuckets sums this column even when duration_count is zero.
			// Preserve stored scalar semantics rather than Merge's observation guard.
			durationSum := acc.DurationSum + d.DurationSum
			acc.Merge(d)
			acc.DurationSum = durationSum
		}
	}
	return batch, safe, rows.Err()
}

// Each range is either one complete hour or an authoritative boundary/missing
// hour. CROSS JOIN below keeps ranges outside the table scan, so covered
// five-minute rows are not visited just to reject them. The entire statement,
// including completion markers and deltas, reads one SQLite snapshot.
func appendRollupRanges(sb *strings.Builder, sel Selector, sketches bool) []any {
	safe := ""
	if sketches {
		safe = " AND r.sketch_safe=1"
	}
	sb.WriteString(`WITH RECURSIVE hours(h) AS (VALUES (?) UNION ALL
  SELECT h+3600 FROM hours WHERE h+3600<?), rollup_ranges AS MATERIALIZED (
  SELECT MAX(h,?) AS lo, MIN(h+3600,?) AS hi,
   COALESCE(r.state=2` + safe + ` AND h>=? AND h+3600<=?
    AND (SELECT value FROM aggregate_rollup_meta WHERE key='store_uuid')=
        (SELECT value FROM aggregate_meta WHERE key='store_uuid'),0) AS rolled
  FROM hours LEFT JOIN aggregate_rollup_hours r ON r.window_start=h) `)
	return []any{hourStart(sel.Start), sel.End, sel.Start, sel.End, sel.Start, sel.End}
}

func rollupSource(table string) string {
	if table == "aggregate_delta_log" {
		return table + ` t INDEXED BY sqlite_autoindex_aggregate_delta_log_1 WHERE `
	}
	rolled := "0"
	index := "sqlite_autoindex_" + table + "_1"
	if table == "aggregate_hourly" {
		rolled = "1"
	}
	return `rollup_ranges p CROSS JOIN ` + table + ` t INDEXED BY ` + index + `
  WHERE p.rolled=` + rolled + ` AND t.window_start>=p.lo AND t.window_start<p.hi AND `
}

func appendRollupSketchUnion(sb *strings.Builder, sel Selector, args []any, rolled bool) []any {
	sources := []string{"aggregate_buckets", "aggregate_delta_log"}
	if rolled {
		sources = []string{"aggregate_buckets", "aggregate_hourly", "aggregate_delta_log"}
	}
	for i, table := range sources {
		if i > 0 {
			sb.WriteString(` UNION ALL `)
		}
		sb.WriteString(`SELECT (SELECT service_id FROM aggregate_series WHERE id=t.series_id) AS service_id,
   t.sketch AS sketch FROM `)
		if rolled {
			sb.WriteString(rollupSource(table))
		} else {
			sb.WriteString(table + ` t INDEXED BY sqlite_autoindex_` + table + `_1 WHERE `)
		}
		sb.WriteString(`t.window_start>=? AND t.window_start<?
   AND t.series_id IN (SELECT id FROM rollup_series) AND t.sketch IS NOT NULL`)
		args = append(args, sel.Start, sel.End)
	}
	return args
}

// sumRollupSnapshot chooses sources and reads them in one SQLite snapshot.
// Adjacent fallback hours use the original series-leading streaming reduction.
// Complete hours are read individually: fixing window_start gives the hourly
// covering index series order, so neither path sorts retained bucket rows.
func sumRollupSnapshot(ctx context.Context, tx sqliteQueryer, sel Selector, by GroupBy) ([]SumRow, error) {
	var sb strings.Builder
	args := appendRollupRanges(&sb, sel, false)
	sb.WriteString(`SELECT lo, hi, rolled FROM rollup_ranges ORDER BY lo`)
	rows, err := tx.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	type interval struct {
		lo, hi int64
		rolled bool
	}
	var ranges []interval
	for rows.Next() {
		var r interval
		if err := rows.Scan(&r.lo, &r.hi, &r.rolled); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if n := len(ranges); n > 0 && !r.rolled && !ranges[n-1].rolled {
			ranges[n-1].hi = r.hi
		} else {
			ranges = append(ranges, r)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var out []SumRow
	groups := make(map[sumGroupKey]int)
	for _, r := range ranges {
		chunk := sel
		chunk.Start, chunk.End = r.lo, r.hi
		var part []SumRow
		var used bool
		if r.rolled {
			part, used, err = readSummarySums(ctx, tx, chunk, by)
		}
		if err == nil && !used {
			part, err = readSumBuckets(ctx, tx, chunk, by, r.rolled)
		}
		if err != nil {
			return nil, err
		}
		for _, row := range part {
			out = mergeSumRow(out, groups, row)
		}
	}
	return out, ctx.Err()
}
