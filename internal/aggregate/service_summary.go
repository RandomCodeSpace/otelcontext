package aggregate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// summarySketchSafe is stricter than the ordinary codec: a summary may only
// regroup sketches whose bins account for every observation without loss.
func summarySketchSafe(s *Sketch) bool {
	if s == nil || s.scale != SketchDefaultScale || s.collapsed || s.saturations != 0 || math.IsNaN(s.sum) || math.IsInf(s.sum, 0) {
		return false
	}
	var bins uint64
	if s.hasBins {
		span := int64(s.maxIdx) - int64(s.minIdx)
		if span < 0 || span >= int64(SketchMaxBins) || s.maxIdx == math.MaxInt32 {
			return false
		}
		for _, n := range s.bins[:span+1] {
			bins += uint64(n) // At most 512 uint32 populations.
		}
	}
	return bins == s.binTotal && s.count >= s.zeroCount && s.count-s.zeroCount == bins
}

// encodeSummarySketch keeps the ordinary codec's 256-bin limit. The parts
// partition bins, not observations: all zeros and the original Sum belong to
// the first part. No bucket midpoint is used to reconstruct Sum.
func encodeSummarySketch(s *Sketch) (low, high []byte, safe bool) {
	if s == nil {
		return nil, nil, true
	}
	if !summarySketchSafe(s) {
		return nil, nil, false
	}
	parts := [2]*Sketch{NewSketch(), NewSketch()}
	parts[0].zeroCount, parts[0].sum = s.zeroCount, s.sum
	n := 0
	if s.hasBins {
		for offset := int64(0); offset <= int64(s.maxIdx)-int64(s.minIdx); offset++ {
			if count := s.bins[offset]; count != 0 {
				parts[n/SketchMaxSerializedBins].add(int32(int64(s.minIdx)+offset), uint64(count))
				n++
			}
		}
	}
	for _, part := range parts {
		part.count = part.zeroCount + part.binTotal
	}
	low = parts[0].Encode()
	if n > SketchMaxSerializedBins {
		high = parts[1].Encode()
	}
	return low, high, true
}

func decodeSummarySketch(low, high []byte) (*Sketch, error) {
	if low == nil {
		if high != nil {
			return nil, fmt.Errorf("%w: summary high part without low part", ErrSketchCorrupt)
		}
		return nil, nil
	}
	s, err := DecodeSketch(low)
	if err != nil {
		return nil, err
	}
	if !summarySketchSafe(s) {
		return nil, fmt.Errorf("%w: unsafe summary low part", ErrSketchCorrupt)
	}
	if high != nil {
		h, err := DecodeSketch(high)
		if err != nil {
			return nil, err
		}
		if !summarySketchSafe(h) || !s.hasBins || !h.hasBins || h.minIdx <= s.maxIdx || int64(h.maxIdx)-int64(s.minIdx) >= int64(SketchMaxBins) || h.zeroCount != 0 || h.sum != 0 || math.MaxUint64-s.count < h.count {
			return nil, fmt.Errorf("%w: unsafe summary high part", ErrSketchCorrupt)
		}
		originalSum := s.sum
		s.Merge(h)
		s.sum = originalSum // Preserve even the sign bit of a finite zero sum.
		if !summarySketchSafe(s) {
			return nil, fmt.Errorf("%w: unsafe reconstructed summary", ErrSketchCorrupt)
		}
	}
	return s, nil
}

const summaryIdentityColumns = `tenant_id,service_id,name_id,dims_id,signal,status_class,http_class,method,span_kind,severity`

type summaryIdentity struct {
	id  SeriesID
	key [10]int64
}

func summaryIdentityQuery(after *summaryIdentity, limit int) (string, []any) {
	q := `SELECT id,` + summaryIdentityColumns + ` FROM aggregate_series INDEXED BY idx_aggregate_series_identity`
	var args []any
	if after != nil {
		q += ` WHERE (` + summaryIdentityColumns + `) > (?,?,?,?,?,?,?,?,?,?)`
		for _, v := range after.key {
			args = append(args, v)
		}
	}
	q += ` ORDER BY ` + summaryIdentityColumns + ` LIMIT ?`
	return q, append(args, limit)
}

func summaryIdentities(ctx context.Context, db sqliteQueryer, after *summaryIdentity, limit int) ([]summaryIdentity, error) {
	q, args := summaryIdentityQuery(after, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []summaryIdentity
	for rows.Next() {
		var v summaryIdentity
		if err := rows.Scan(&v.id, &v.key[0], &v.key[1], &v.key[2], &v.key[3], &v.key[4], &v.key[5], &v.key[6], &v.key[7], &v.key[8], &v.key[9]); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const summaryKeyColumns = `hour_start,resolution,tenant_id,signal,service_id,name_id,window_start`
const summaryValueColumns = `point_count,error_count,request_count,error_request_count,duration_count,duration_sum,log_count,identity_count`
const summaryCurrentSQL = `r.state=2 AND m.source_build_id=r.build_id
 AND (SELECT value FROM aggregate_rollup_meta WHERE key='service_summary_store_uuid')=(SELECT value FROM aggregate_meta WHERE key='store_uuid')
 AND (SELECT value FROM aggregate_rollup_meta WHERE key='store_uuid')=(SELECT value FROM aggregate_meta WHERE key='store_uuid')`
const summaryReadySQL = `m.state=2 AND ` + summaryCurrentSQL
const summaryCompleteSQL = `m.state IN (2,4) AND ` + summaryCurrentSQL

// State 4 is finished, but scalar regrouping was not proved exact. Full scalar
// reads must fall back. Exact count projections and independently checked
// sketches may use it; the worker does not retry until its source changes.
const summaryArithmeticFallback = 4

// Nonnegative integral inputs with total <= 2^52 add exactly in any order.
// Reserve another 2^52 for late deltas, so every combined subset stays <=2^53.
// This deliberately excludes fractional sums; finite alone is not sufficient.
const summarySumLimit = uint64(1 << 52)

func summaryExactSum(total *uint64, value float64) bool {
	if !(value >= 0 && value <= float64(summarySumLimit)) || math.Trunc(value) != value {
		return false
	}
	n := uint64(value)
	if n > summarySumLimit-*total {
		return false
	}
	*total += n
	return true
}

func (s *SQLiteStore) ensureServiceSummarySchema() error {
	return s.rollupWrite(context.Background(), func(tx *sql.Tx) error {
		for _, table := range []string{"aggregate_service_summary", "aggregate_service_summary_hours"} {
			var kind string
			err := tx.QueryRow(`SELECT type FROM sqlite_master WHERE name=? COLLATE NOCASE`, table).Scan(&kind)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && kind != "table" {
				return fmt.Errorf("service summary storage %s is a %s", table, kind)
			}
		}
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='aggregate_service_summary' COLLATE NOCASE)`).Scan(&exists); err != nil {
			return err
		}
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS aggregate_service_summary(` + summaryKeyColumnsDDL + `,
    point_count INTEGER NOT NULL,error_count INTEGER NOT NULL,request_count INTEGER NOT NULL,error_request_count INTEGER NOT NULL,
    duration_count INTEGER NOT NULL,duration_sum REAL NOT NULL,log_count INTEGER NOT NULL,identity_count INTEGER NOT NULL,
    sketch_state INTEGER NOT NULL,sketch BLOB,sketch_high BLOB,PRIMARY KEY(` + summaryKeyColumns + `)) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS aggregate_service_summary_hours(window_start INTEGER PRIMARY KEY,state INTEGER NOT NULL,build_id TEXT NOT NULL,source_build_id TEXT NOT NULL)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		var version, owner string
		err := tx.QueryRow(`SELECT value FROM aggregate_rollup_meta WHERE key='service_summary_version'`).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && version != "1" {
			return fmt.Errorf("unsupported service summary version %q", version)
		}
		err = tx.QueryRow(`SELECT value FROM aggregate_rollup_meta WHERE key='service_summary_store_uuid'`).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !exists || owner != s.uuid {
			if _, err := tx.Exec(`UPDATE aggregate_service_summary_hours SET state=0,build_id='',source_build_id=''`); err != nil {
				return err
			}
		}
		for _, kv := range [][2]string{{"service_summary_version", "1"}, {"service_summary_store_uuid", s.uuid}} {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO aggregate_rollup_meta VALUES (?,?)`, kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	})
}

const summaryKeyColumnsDDL = `hour_start INTEGER NOT NULL,resolution INTEGER NOT NULL,tenant_id INTEGER NOT NULL,signal INTEGER NOT NULL,service_id INTEGER NOT NULL,name_id INTEGER NOT NULL,window_start INTEGER NOT NULL`

// A scalar cell is accumulated only from nonnegative SQLite integers. Refusing
// derived overflow keeps the authoritative SQL path responsible for its error.
type summaryScalar struct {
	counts  [7]int64 // point,error,request,error_request,duration,log,identity
	sum     float64
	present bool
}

func (a *summaryScalar) add(b summaryScalar) error {
	for i, n := range b.counts {
		if n < 0 || a.counts[i] > math.MaxInt64-n {
			return fmt.Errorf("service summary integer overflow")
		}
		a.counts[i] += n
	}
	a.sum += b.sum
	if math.IsNaN(a.sum) || math.IsInf(a.sum, 0) {
		return fmt.Errorf("service summary non-finite duration sum")
	}
	a.present = true
	return nil
}
func scanSummaryScalar(scan scanFunc, prefix ...any) (summaryScalar, error) {
	var a summaryScalar
	args := append(prefix, &a.counts[0], &a.counts[1], &a.counts[2], &a.counts[3], &a.counts[4], &a.sum, &a.counts[5])
	err := scan(args...)
	a.present = err == nil
	return a, err
}

type summaryCell struct {
	resolution, tenant, signal, service, name, window int64
	scalar                                            summaryScalar
	sketchState                                       int
	low, high                                         []byte
}

func (c summaryCell) args(hour int64) []any {
	return []any{hour, c.resolution, c.tenant, c.signal, c.service, c.name, c.window, c.scalar.counts[0], c.scalar.counts[1], c.scalar.counts[2], c.scalar.counts[3], c.scalar.counts[4], c.scalar.sum, c.scalar.counts[5], c.scalar.counts[6], c.sketchState, c.low, c.high}
}

// Owned by the single worker. Ordinary yields preserve this generation and
// cursor. Any error poisons it, including an uncertain commit outcome, so the
// next attempt must claim a new token and clear the invisible partial rows.
type summaryBuild struct {
	hour                   int64
	uuid, source, token    string
	cleaning, done, failed bool
	after                  *summaryIdentity
	tenant, service, name  int64
	group                  bool
	windows                [12]summaryScalar
	trace, logs, edge      summaryScalar
	sketch                 *Sketch
	unsafe                 bool
	pending                []summaryCell
	hourlySum, windowSum   uint64
	sumUnsafe              bool
}

func (s *SQLiteStore) claimSummaryBuild(ctx context.Context, hour int64) (*summaryBuild, error) {
	token, err := newStoreUUID()
	if err != nil {
		return nil, err
	}
	c := &summaryBuild{hour: hour, uuid: s.uuid, token: token, cleaning: true}
	err = s.rollupWrite(ctx, func(tx *sql.Tx) error {
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM aggregate_meta WHERE key='store_uuid'`).Scan(&owner); err != nil {
			return err
		}
		if owner != c.uuid {
			return errRollupInvalidated
		}
		if err := tx.QueryRowContext(ctx, `SELECT build_id FROM aggregate_rollup_hours WHERE window_start=? AND state=2`, hour).Scan(&c.source); errors.Is(err, sql.ErrNoRows) {
			return errRollupInvalidated
		} else if err != nil {
			return err
		}
		var ready bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM aggregate_service_summary_hours m JOIN aggregate_rollup_hours r USING(window_start) WHERE m.window_start=? AND `+summaryCompleteSQL+`)`, hour).Scan(&ready); err != nil {
			return err
		}
		if ready {
			return errRollupInvalidated
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO aggregate_service_summary_hours VALUES (?,1,?,?) ON CONFLICT(window_start) DO UPDATE SET state=1,build_id=excluded.build_id,source_build_id=excluded.source_build_id`, hour, token, c.source)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}
func checkSummaryBuild(ctx context.Context, tx *sql.Tx, c *summaryBuild) error {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM aggregate_service_summary_hours m JOIN aggregate_rollup_hours r USING(window_start)
 WHERE m.window_start=? AND m.state=1 AND m.build_id=? AND m.source_build_id=? AND r.state=2 AND r.build_id=?
 AND (SELECT value FROM aggregate_meta WHERE key='store_uuid')=?
 AND (SELECT value FROM aggregate_rollup_meta WHERE key='store_uuid')=?
 AND (SELECT value FROM aggregate_rollup_meta WHERE key='service_summary_store_uuid')=?)`, c.hour, c.token, c.source, c.source, c.uuid, c.uuid, c.uuid).Scan(&ok)
	if err == nil && !ok {
		return errRollupInvalidated
	}
	return err
}

// LIMIT applies to primary-key rows; every delete rechecks its owner's token
// inside this transaction, including the last batch.
func deleteSummaryBatch(ctx context.Context, tx *sql.Tx, hour int64, limit int) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM aggregate_service_summary WHERE (`+summaryKeyColumns+`) IN (SELECT `+summaryKeyColumns+` FROM aggregate_service_summary WHERE hour_start=? LIMIT ?)`, hour, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// summaryBuildSlice bounds both input and pending output. It never carries a
// reader or writer across a yield, and opens a fresh checked snapshot each time.
// The private allowance lets tests prove progress without a large fixture.
func (s *SQLiteStore) summaryBuildSlice(ctx context.Context, c *summaryBuild, allowance int) (done bool, err error) {
	if c.failed || c.done {
		return c.done, errRollupInvalidated
	}
	defer func() {
		if err != nil {
			c.failed = true
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	yieldAt := time.Now().Add(25 * time.Second)
	if allowance < 1 || allowance > rollupBatch {
		allowance = rollupBatch
	}
	if c.cleaning {
		var n int64
		err = s.rollupWrite(ctx, func(tx *sql.Tx) error {
			if err := checkSummaryBuild(ctx, tx, c); err != nil {
				return err
			}
			var err error
			n, err = deleteSummaryBatch(ctx, tx, c.hour, allowance)
			return err
		})
		if err != nil {
			return false, err
		}
		c.cleaning = n == int64(allowance)
		return false, nil
	}
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = checkSummaryBuild(ctx, tx, c); err != nil {
		return false, err
	}
	if c.after == nil {
		// Twelve disjoint index seeks exclude the canonical timestamps. They
		// reject an off-grid source window without scanning log payloads or
		// the rows at any of the twelve normal five-minute starts.
		for window := c.hour; window < c.hour+rollupHour; window += 300 {
			var found int64
			err := tx.QueryRowContext(ctx, `SELECT window_start FROM aggregate_buckets WHERE window_start>? AND window_start<? LIMIT 1`, window, window+300).Scan(&found)
			if err == nil {
				return false, fmt.Errorf("noncanonical summary window %d", found)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
		}
	}
	ids, err := summaryIdentities(ctx, tx, c.after, allowance)
	if err != nil {
		return false, err
	}
	for i := range ids {
		if time.Now().After(yieldAt) || len(c.pending) > rollupBatch-16 {
			break
		}
		if err = c.consume(ctx, tx, ids[i]); err != nil {
			return false, err
		}
		id := ids[i]
		c.after = &id
	}
	if len(ids) == 0 {
		if err = c.flushService(); err != nil {
			return false, err
		}
		done = true
	}
	if err = tx.Rollback(); err != nil {
		return false, err
	}
	err = s.rollupWrite(ctx, func(tx *sql.Tx) error {
		if err := checkSummaryBuild(ctx, tx, c); err != nil {
			return err
		}
		if len(c.pending) > 0 {
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO aggregate_service_summary(`+summaryKeyColumns+`,`+summaryValueColumns+`,sketch_state,sketch,sketch_high) VALUES (`+placeholders(18)+`)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, row := range c.pending {
				if _, err := stmt.ExecContext(ctx, row.args(c.hour)...); err != nil {
					return err
				}
			}
		}
		if done {
			state := 2
			if c.sumUnsafe {
				state = summaryArithmeticFallback
			}
			_, err := tx.ExecContext(ctx, `UPDATE aggregate_service_summary_hours SET state=? WHERE window_start=? AND state=1 AND build_id=?`, state, c.hour, c.token)
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	c.pending = nil
	c.done = done
	return done, nil
}

func (c *summaryBuild) cell(signal, resolution, window, name int64, a summaryScalar) summaryCell {
	return summaryCell{resolution: resolution, tenant: c.tenant, signal: signal, service: c.service, name: name, window: window, scalar: a}
}
func (c *summaryBuild) flushEdge() {
	if c.edge.present {
		c.pending = append(c.pending, c.cell(int64(SignalServiceEdge), rollupHour, c.hour, c.name, c.edge))
	}
	c.edge = summaryScalar{}
}
func (c *summaryBuild) flushService() error {
	if !c.group {
		return nil
	}
	c.flushEdge()
	for i, a := range c.windows {
		if a.present {
			c.pending = append(c.pending, c.cell(int64(SignalTraceOp), 300, c.hour+int64(i)*300, 0, a))
		}
	}
	if c.trace.present {
		row := c.cell(int64(SignalTraceOp), rollupHour, c.hour, 0, c.trace)
		if c.unsafe {
			row.sketchState = 2
		} else if c.sketch != nil {
			var safe bool
			row.low, row.high, safe = encodeSummarySketch(c.sketch)
			if !safe {
				row.sketchState = 2
			} else {
				row.sketchState = 1
			}
		}
		c.pending = append(c.pending, row)
	}
	if c.logs.present {
		c.pending = append(c.pending, c.cell(int64(SignalLog), rollupHour, c.hour, 0, c.logs))
	}
	c.windows = [12]summaryScalar{}
	c.trace = summaryScalar{}
	c.logs = summaryScalar{}
	c.sketch = nil
	c.unsafe = false
	c.group = false
	return nil
}
func (c *summaryBuild) consume(ctx context.Context, tx *sql.Tx, id summaryIdentity) error {
	if c.group && (c.tenant != id.key[0] || c.service != id.key[1]) {
		if err := c.flushService(); err != nil {
			return err
		}
	}
	if c.group && c.name != id.key[2] {
		c.flushEdge()
	}
	c.group = true
	c.tenant, c.service, c.name = id.key[0], id.key[1], id.key[2]
	signal := Signal(id.key[4])
	if signal != SignalTraceOp && signal != SignalLog && signal != SignalServiceEdge {
		return nil
	}
	var other bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM aggregate_dict WHERE id IN (?,?,?) AND value=?)`, id.key[1], id.key[2], id.key[3], []byte(OtherValue)).Scan(&other); err != nil {
		return err
	}
	// Point probes of the completed hourly source avoid canonical log payloads.
	a, err := scanSummaryScalar(tx.QueryRowContext(ctx, `SELECT `+sumColumnList+` FROM aggregate_hourly INDEXED BY idx_aggregate_hourly_summary WHERE window_start=? AND series_id=?`, c.hour, id.id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if other {
		a.counts[6] = a.counts[0]
	}
	// The budgets span the entire hour, across service flushes and yields.
	// Per-cell checks alone would not prove later regrouping across services.
	if !summaryExactSum(&c.hourlySum, a.sum) {
		c.sumUnsafe = true
	}
	switch signal {
	case SignalLog:
		return c.logs.add(a)
	case SignalServiceEdge:
		return c.edge.add(a)
	}
	if err := c.trace.add(a); err != nil {
		return err
	}
	var safe bool
	if err := tx.QueryRowContext(ctx, `SELECT sketch_safe FROM aggregate_rollup_hours WHERE window_start=?`, c.hour).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		c.unsafe = true
	}
	var blob []byte
	if err := tx.QueryRowContext(ctx, `SELECT sketch FROM aggregate_hourly WHERE window_start=? AND series_id=?`, c.hour, id.id).Scan(&blob); err != nil {
		return err
	}
	if blob != nil {
		sk, err := DecodeSketch(blob)
		if err != nil {
			return err
		}
		if !summarySketchSafe(sk) {
			c.unsafe = true
		}
		if !c.unsafe {
			if c.sketch == nil {
				c.sketch = NewSketch()
			}
			if math.MaxUint64-c.sketch.count < sk.count {
				c.unsafe = true
			} else {
				c.sketch.Merge(sk)
				c.unsafe = !summarySketchSafe(c.sketch)
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT window_start,`+sumColumnList+` FROM aggregate_buckets INDEXED BY idx_aggregate_buckets_series_window WHERE series_id=? AND window_start>=? AND window_start<? ORDER BY window_start`, id.id, c.hour, c.hour+rollupHour)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var w int64
		a, err := scanSummaryScalar(rows.Scan, &w)
		if err != nil {
			return err
		}
		if (w-c.hour)%300 != 0 {
			return fmt.Errorf("noncanonical summary window %d", w)
		}
		if other {
			a.counts[6] = a.counts[0]
		}
		if !summaryExactSum(&c.windowSum, a.sum) {
			c.sumUnsafe = true
		}
		if err := c.windows[(w-c.hour)/300].add(a); err != nil {
			return err
		}
	}
	return rows.Err()
}

// A cleanup token owns deletions independently of a build token. The data key
// has no generation field, so checking only once before a loop is unsafe.
type summaryCleanup struct {
	hour, cutoff        int64
	token, source, uuid string
}

func summarySourceToken(ctx context.Context, tx *sql.Tx, hour int64) (string, error) {
	var token string
	err := tx.QueryRowContext(ctx, `SELECT build_id FROM aggregate_rollup_hours WHERE window_start=? AND state=2`, hour).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return token, err
}
func summaryNeedsCleanup(ctx context.Context, tx *sql.Tx, hour, cutoff int64) (bool, error) {
	var state int
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT m.state,COALESCE(r.state=2 AND r.build_id=m.source_build_id,0)
 FROM aggregate_service_summary_hours m LEFT JOIN aggregate_rollup_hours r USING(window_start) WHERE m.window_start=?`, hour).Scan(&state, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // Derived-only rows have no publication owner.
	}
	if err != nil {
		return false, err
	}
	if state == 3 || !current {
		return true, nil
	}
	if hour >= cutoff {
		return false, nil // Healthy, wholly retained hour: no data probe.
	}
	if hour <= math.MaxInt64-rollupHour && cutoff >= hour+rollupHour {
		return true, nil
	}
	// Bucket mutation invalidates the source token in the same transaction.
	// With a current token, the boundary hour needs cleanup only if its source
	// still contains an expired window. The time-leading PK seeks directly to
	// this interval, including the empty-interval case; no summary-cell scan.
	var expired bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM aggregate_buckets WHERE window_start>=? AND window_start<? LIMIT 1)`, hour, cutoff).Scan(&expired)
	return expired, err
}
func (s *SQLiteStore) claimSummaryCleanup(ctx context.Context, hour, cutoff int64) (*summaryCleanup, error) {
	token, err := newStoreUUID()
	if err != nil {
		return nil, err
	}
	c := &summaryCleanup{hour: hour, cutoff: cutoff, token: token, uuid: s.uuid}
	err = s.rollupWrite(ctx, func(tx *sql.Tx) error {
		needed, err := summaryNeedsCleanup(ctx, tx, hour, cutoff)
		if err != nil {
			return err
		}
		if !needed {
			return errRollupInvalidated
		}
		c.source, err = summarySourceToken(ctx, tx, hour)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO aggregate_service_summary_hours VALUES (?,3,?,?) ON CONFLICT(window_start) DO UPDATE SET state=3,build_id=excluded.build_id,source_build_id=excluded.source_build_id`, hour, token, c.source)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}
func (s *SQLiteStore) summaryCleanupSlice(ctx context.Context, c *summaryCleanup, limit int) (done bool, err error) {
	if limit < 1 || limit > rollupBatch {
		limit = rollupBatch
	}
	err = s.rollupWrite(ctx, func(tx *sql.Tx) error {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM aggregate_service_summary_hours WHERE window_start=? AND state=3 AND build_id=? AND source_build_id=? AND (SELECT value FROM aggregate_meta WHERE key='store_uuid')=?)`, c.hour, c.token, c.source, c.uuid).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return errRollupInvalidated
		}
		source, err := summarySourceToken(ctx, tx, c.hour)
		if err != nil {
			return err
		}
		if source != c.source {
			return errRollupInvalidated
		}
		needed, err := summaryNeedsCleanup(ctx, tx, c.hour, c.cutoff)
		if err != nil {
			return err
		}
		if !needed {
			return errRollupInvalidated
		}
		n, err := deleteSummaryBatch(ctx, tx, c.hour, limit)
		if err != nil {
			return err
		}
		done = n < int64(limit)
		if done {
			_, err = tx.ExecContext(ctx, `DELETE FROM aggregate_service_summary_hours WHERE window_start=? AND state=3 AND build_id=? AND source_build_id=?`, c.hour, c.token, c.source)
		}
		return err
	})
	return done, err
}

// The purge pass discovers derived-only hours too. The bounded scan and delete
// share the existing per-pass allowance. A later purge resumes a cleanup state
// with a fresh token, fencing any old invocation before its next mutation.
func (s *SQLiteStore) purgeServiceSummaries(ctx context.Context, cutoff int64) error {
	var after int64 = math.MinInt64
	for budget := 0; budget < purgeWindowsPerPass; {
		var hour int64
		err := s.reader.QueryRowContext(ctx, `SELECT MIN(h) FROM (
   SELECT MIN(hour_start) h FROM aggregate_service_summary WHERE hour_start>=?
   UNION ALL SELECT MIN(window_start) h FROM aggregate_service_summary_hours WHERE window_start>=?) HAVING MIN(h) IS NOT NULL`, after, after).Scan(&hour)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		c, err := s.claimSummaryCleanup(ctx, hour, cutoff)
		if err != nil && !errors.Is(err, errRollupInvalidated) {
			return err
		}
		if err == nil {
			for budget < purgeWindowsPerPass {
				done, err := s.summaryCleanupSlice(ctx, c, rollupBatch)
				budget++
				if errors.Is(err, errRollupInvalidated) {
					break
				}
				if err != nil {
					return err
				}
				if done {
					break
				}
			}
		} else {
			budget++
		}
		if hour > math.MaxInt64-rollupHour {
			return nil
		}
		after = hour + rollupHour
	}
	return nil
}

type summaryWorker struct {
	sourceAfter, summaryAfter int64
	build                     *summaryBuild
}

// One source attempt and one summary slice get an opportunity per turn. Source
// failures advance their cursor and are reported without starving ready hours.
func (s *SQLiteStore) summaryWorkerStep(ctx context.Context, now time.Time, w *summaryWorker, buildSource func(context.Context, int64, time.Duration) error) (bool, error) {
	active := false
	var sourceErr, summaryErr error
	var hour int64
	cutoff := now.Unix() - int64(AllowedLateness/time.Second) - rollupHour
	err := s.reader.QueryRowContext(ctx, `SELECT window_start FROM aggregate_rollup_hours WHERE state<>2 AND window_start>=? AND window_start<=? ORDER BY window_start LIMIT 1`, w.sourceAfter, cutoff).Scan(&hour)
	if err == nil {
		active = true
		w.sourceAfter = hour + rollupHour
		sourceErr = buildSource(ctx, hour, 25*time.Millisecond)
	} else if !errors.Is(err, sql.ErrNoRows) {
		sourceErr = err
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if w.build == nil {
		err := s.reader.QueryRowContext(ctx, `SELECT r.window_start FROM aggregate_rollup_hours r LEFT JOIN aggregate_service_summary_hours m USING(window_start)
   WHERE r.state=2 AND r.window_start>=? AND (m.window_start IS NULL OR m.state NOT IN (2,4) OR m.source_build_id<>r.build_id) ORDER BY r.window_start LIMIT 1`, w.summaryAfter).Scan(&hour)
		if err == nil {
			active = true
			w.summaryAfter = hour + rollupHour
			w.build, summaryErr = s.claimSummaryBuild(ctx, hour)
		} else if !errors.Is(err, sql.ErrNoRows) {
			summaryErr = err
		}
	}
	if w.build != nil {
		active = true
		done, err := s.summaryBuildSlice(ctx, w.build, rollupBatch)
		summaryErr = err
		if done || err != nil {
			w.build = nil
		}
	}
	return active, errors.Join(sourceErr, summaryErr)
}

func summarySignals(sel Selector) []Signal {
	if sel.Signal != SignalUnspecified {
		return []Signal{sel.Signal}
	}
	return sel.Signals
}
func summaryResolution(sel Selector, by GroupBy) int64 {
	if len(sel.SeriesIDs) > 0 || sel.SketchOnly || sel.After != (BucketCursor{}) || by & ^(GroupByWindow|GroupByService|GroupByName|GroupBySignal) != 0 {
		return 0
	}
	signals := summarySignals(sel)
	if len(signals) == 0 {
		return 0
	}
	only := func(signal Signal) bool {
		for _, s := range signals {
			if s != signal {
				return false
			}
		}
		return true
	}
	if by&GroupByWindow != 0 {
		if by&GroupByName == 0 && only(SignalTraceOp) {
			return 300
		}
		return 0
	}
	if only(SignalServiceEdge) {
		return rollupHour
	}
	if by&GroupByName != 0 {
		return 0
	}
	for _, signal := range signals {
		if signal != SignalTraceOp && signal != SignalLog {
			return 0
		}
	}
	return rollupHour
}
func summaryReady(ctx context.Context, db sqliteQueryer, hour int64) (bool, error) {
	return summaryEligible(ctx, db, hour, summaryReadySQL)
}

func summaryEligible(ctx context.Context, db sqliteQueryer, hour int64, predicate string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM aggregate_service_summary_hours m JOIN aggregate_rollup_hours r USING(window_start) WHERE m.window_start=? AND `+predicate, hour)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	ready := rows.Next()
	return ready, rows.Err()
}

// Membership values are bound once, including large accepted service lists.
// The delta branch retains the authoritative per-series reduction for scalar
// groups, and the outer SQL SUM preserves signed overflow across both sources.
func summarySumQuery(sel Selector, by GroupBy, resolution int64) (string, []any) {
	return summarySumQueryFor(sel, by, resolution, false)
}

// trafficOnly is used solely by QueryBuckets, whose result contains counts
// and identity coverage, never DurationSum. Its private result type omits sums.
func summarySumQueryFor(sel Selector, by GroupBy, resolution int64, trafficOnly bool) (string, []any) {
	var sb strings.Builder
	var args []any
	sb.WriteString(`WITH summary_services(id) AS (`)
	if len(sel.ServiceIDs) == 0 {
		sb.WriteString(`SELECT 0 WHERE 0`)
	} else {
		sb.WriteString(`VALUES `)
		for i, id := range sel.ServiceIDs {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`(?)`)
			args = append(args, id)
		}
	}
	sb.WriteString(`), summary_series AS (SELECT id FROM aggregate_series WHERE `)
	membership := sel
	membership.ServiceIDs = nil
	args = appendSeriesFilter(&sb, membership, args)
	if len(sel.ServiceIDs) > 0 {
		sb.WriteString(` AND service_id IN (SELECT id FROM summary_services)`)
	}
	sb.WriteString(`) SELECT `)
	var keys, summaryKeys, deltaKeys, groups []string
	for _, g := range []struct {
		flag GroupBy
		col  string
	}{{GroupByWindow, "window_start"}, {GroupByService, "service_id"}, {GroupByName, "name_id"}, {GroupBySignal, "signal"}} {
		if by&g.flag != 0 {
			keys = append(keys, g.col)
			summaryKeys = append(summaryKeys, "t."+g.col+" AS "+g.col)
			src := "s."
			if g.flag == GroupByWindow {
				src = "t."
			}
			deltaKeys = append(deltaKeys, src+g.col+" AS "+g.col)
			groups = append(groups, g.col)
		} else {
			keys = append(keys, "0")
			summaryKeys = append(summaryKeys, "0 AS "+g.col)
			deltaKeys = append(deltaKeys, "0 AS "+g.col)
		}
	}
	cols := strings.Split(summaryValueColumns, ",")
	sb.WriteString(strings.Join(keys, ",") + ",")
	for i, col := range cols {
		if i > 0 {
			sb.WriteByte(',')
		}
		if trafficOnly && col == "duration_sum" {
			sb.WriteByte('0')
			continue
		}
		sb.WriteString(`COALESCE(SUM(` + col + `),0)`)
	}
	group := ""
	if len(groups) > 0 {
		group = ` GROUP BY ` + strings.Join(groups, ",")
	}
	sums := func(identity string) string {
		var out []string
		for _, col := range cols {
			if trafficOnly && col == "duration_sum" {
				out = append(out, "0 AS duration_sum")
				continue
			}
			value := "t." + col
			if col == "identity_count" {
				value = identity
			}
			out = append(out, "SUM("+value+") AS "+col)
		}
		return strings.Join(out, ",")
	}
	sb.WriteString(` FROM (SELECT ` + strings.Join(summaryKeys, ",") + `,` + sums("t.identity_count") + ` FROM aggregate_service_summary t WHERE hour_start=? AND resolution=? AND `)
	args = append(args, hourStart(sel.Start), resolution)
	args = appendSeriesFilter(&sb, membership, args)
	if len(sel.ServiceIDs) > 0 {
		sb.WriteString(` AND service_id IN (SELECT id FROM summary_services)`)
	}
	sb.WriteString(` AND window_start>=? AND window_start<?`)
	args = append(args, sel.Start, sel.End)
	sb.WriteString(group + ` UNION ALL SELECT ` + strings.Join(deltaKeys, ",") + `,` + sums(`CASE WHEN EXISTS(SELECT 1 FROM aggregate_dict WHERE id IN (s.service_id,s.name_id,s.dims_id) AND value=?) THEN t.point_count ELSE 0 END`) + ` FROM `)
	args = append(args, []byte(OtherValue))
	if by != 0 && by&GroupByWindow == 0 {
		var scalarSums []string
		for _, col := range strings.Split(strings.ReplaceAll(sumColumnList, "\n", ""), ",") {
			col = strings.TrimSpace(col)
			if trafficOnly && col == "duration_sum" {
				scalarSums = append(scalarSums, "0 AS duration_sum")
				continue
			}
			scalarSums = append(scalarSums, `SUM(t.`+col+`) AS `+col)
		}
		sb.WriteString(`(SELECT t.series_id,` + strings.Join(scalarSums, ",") + ` FROM aggregate_delta_log t WHERE t.window_start>=? AND t.window_start<? AND t.series_id IN (SELECT id FROM summary_series) GROUP BY t.series_id) t JOIN aggregate_series s ON s.id=t.series_id`)
	} else {
		sb.WriteString(`aggregate_delta_log t INDEXED BY sqlite_autoindex_aggregate_delta_log_1 JOIN aggregate_series s ON s.id=t.series_id WHERE t.window_start>=? AND t.window_start<? AND t.series_id IN (SELECT id FROM summary_series)`)
	}
	args = append(args, sel.Start, sel.End)
	sb.WriteString(group + `)` + group)
	return sb.String(), args
}

func summaryDeltaSumSafe(ctx context.Context, db sqliteQueryer, sel Selector) (bool, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT COALESCE(MIN(t.duration_sum>=0 AND t.duration_sum<=4503599627370496 AND t.duration_sum=CAST(t.duration_sum AS INTEGER)),1),COALESCE(SUM(t.duration_sum),0)
 FROM aggregate_delta_log t INDEXED BY sqlite_autoindex_aggregate_delta_log_1 WHERE t.window_start>=? AND t.window_start<? AND t.series_id IN (SELECT id FROM aggregate_series WHERE `)
	args := appendSeriesFilter(&sb, sel, []any{sel.Start, sel.End})
	sb.WriteByte(')')
	rows, err := db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var safe bool
	var sum float64
	if rows.Next() {
		if err := rows.Scan(&safe, &sum); err != nil {
			return false, err
		}
	}
	// All admitted inputs are nonnegative integers. A sum above 2^52 cannot
	// round back below this bound (integer additions stay exact through 2^53).
	return safe && sum <= float64(summarySumLimit), rows.Err()
}

func readSummarySums(ctx context.Context, db sqliteQueryer, sel Selector, by GroupBy) ([]SumRow, bool, error) {
	return readSummaryProjection(ctx, db, sel, by, false)
}

func readSummaryProjection(ctx context.Context, db sqliteQueryer, sel Selector, by GroupBy, trafficOnly bool) ([]SumRow, bool, error) {
	if trafficOnly && by != GroupByWindow && by != GroupByWindow|GroupByService {
		return nil, false, nil
	}
	resolution := summaryResolution(sel, by)
	if resolution == 0 || resolution == rollupHour && (sel.Start != hourStart(sel.Start) || sel.End-sel.Start != rollupHour) {
		return nil, false, nil
	}
	predicate := summaryReadySQL
	if trafficOnly {
		predicate = summaryCompleteSQL
	}
	ready, err := summaryEligible(ctx, db, hourStart(sel.Start), predicate)
	if err != nil || !ready {
		return nil, false, err
	}
	if !trafficOnly {
		if safe, err := summaryDeltaSumSafe(ctx, db, sel); err != nil || !safe {
			return nil, false, err
		}
	}
	q, args := summarySumQueryFor(sel, by, resolution, trafficOnly)
	rows, err := readSumQuery(ctx, db, q, args)
	return rows, true, err
}

func visitServiceSummarySketches(ctx context.Context, db sqliteQueryer, sel Selector, visit func(uint32, *Sketch) error) error {
	if _, err := sel.Validate(); err != nil {
		return err
	}
	if len(sel.SeriesIDs) > 0 || sel.After != (BucketCursor{}) || len(summarySignals(sel)) != 1 || summarySignals(sel)[0] != SignalTraceOp {
		return visitSQLiteSketches(ctx, db, sel, visit)
	}
	type interval struct {
		lo, hi int64
		ready  bool
	}
	var ranges []interval
	anyReady := false
	for start := sel.Start; start < sel.End; {
		end := sel.End
		if end-hourStart(start) > rollupHour {
			end = hourStart(start) + rollupHour
		}
		ready := false
		if start == hourStart(start) && end-start == rollupHour {
			var err error
			// Arithmetic fallback does not certify a sketch. Completion permits
			// reading the payload; readSummarySketchHour and the whole-chunk
			// probe below still enforce all source, codec and merge safety.
			ready, err = summaryEligible(ctx, db, start, summaryCompleteSQL)
			if err != nil {
				return err
			}
		}
		anyReady = anyReady || ready
		if n := len(ranges); n > 0 && !ready && !ranges[n-1].ready {
			ranges[n-1].hi = end
		} else {
			ranges = append(ranges, interval{start, end, ready})
		}
		start = end
	}
	if !anyReady {
		return visitSQLiteSketches(ctx, db, sel, visit)
	}
	probe := NewSketch()
	byService := map[uint32]*Sketch{}
	var order []uint32
	unsafe := errors.New("unsafe summary sketch candidate")
	collect := func(id uint32, sk *Sketch) error {
		if sk == nil {
			return nil
		}
		if !summarySketchSafe(sk) || math.MaxUint64-probe.count < sk.count {
			return unsafe
		}
		probe.Merge(sk)
		if !summarySketchSafe(probe) {
			return unsafe
		}
		if byService[id] == nil {
			byService[id] = NewSketch()
			order = append(order, id)
		}
		byService[id].Merge(sk)
		return nil
	}
	for _, r := range ranges {
		chunk := sel
		chunk.Start, chunk.End = r.lo, r.hi
		var err error
		if r.ready {
			err = readSummarySketchHour(ctx, db, chunk, collect, unsafe)
		} else {
			err = visitSQLiteSketches(ctx, db, chunk, collect)
		}
		if errors.Is(err, unsafe) {
			return visitSQLiteSketches(ctx, db, sel, visit)
		}
		if err != nil {
			return err
		}
	}
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(id, byService[id]); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func readSummarySketchHour(ctx context.Context, db sqliteQueryer, sel Selector, collect func(uint32, *Sketch) error, unsafe error) error {
	var sb strings.Builder
	var args []any
	sb.WriteString(`WITH services(id) AS (`)
	if len(sel.ServiceIDs) == 0 {
		sb.WriteString(`SELECT 0 WHERE 0`)
	} else {
		sb.WriteString(`VALUES `)
		for i, id := range sel.ServiceIDs {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`(?)`)
			args = append(args, id)
		}
	}
	sb.WriteString(`) SELECT service_id,sketch_state,sketch,sketch_high,0 FROM aggregate_service_summary WHERE hour_start=? AND resolution=3600 AND tenant_id=? AND signal=?`)
	args = append(args, sel.Start, sel.TenantID, SignalTraceOp)
	if len(sel.ServiceIDs) > 0 {
		sb.WriteString(` AND service_id IN (SELECT id FROM services)`)
	}
	sb.WriteString(` UNION ALL SELECT (SELECT service_id FROM aggregate_series WHERE id=t.series_id),1,t.sketch,NULL,1 FROM aggregate_delta_log t INDEXED BY sqlite_autoindex_aggregate_delta_log_1 WHERE window_start>=? AND window_start<? AND t.sketch IS NOT NULL AND series_id IN (SELECT id FROM aggregate_series WHERE tenant_id=? AND signal=?`)
	args = append(args, sel.Start, sel.End, sel.TenantID, SignalTraceOp)
	if len(sel.ServiceIDs) > 0 {
		sb.WriteString(` AND service_id IN (SELECT id FROM services)`)
	}
	sb.WriteByte(')')
	rows, err := db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uint32
		var state, delta int
		var low, high []byte
		if err := rows.Scan(&id, &state, &low, &high, &delta); err != nil {
			return err
		}
		if state == 2 {
			return unsafe
		}
		if state == 0 {
			if low != nil || high != nil {
				return fmt.Errorf("%w: absent summary with payload", ErrSketchCorrupt)
			}
			continue
		}
		if state != 1 {
			return fmt.Errorf("%w: summary sketch state %d", ErrSketchCorrupt, state)
		}
		var sk *Sketch
		var err error
		if delta == 1 {
			sk, err = DecodeSketch(low)
		} else {
			if low == nil {
				return fmt.Errorf("%w: missing summary payload", ErrSketchCorrupt)
			}
			sk, err = decodeSummarySketch(low, high)
		}
		if err != nil {
			return err
		}
		if err := collect(id, sk); err != nil {
			return err
		}
	}
	return rows.Err()
}
