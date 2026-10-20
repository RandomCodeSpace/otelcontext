package prometheus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"

	localstorage "github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	promstorage "github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/annotations"
	"gorm.io/gorm"
)

func (l *Local) queryContext(ctx context.Context, params url.Values) (context.Context, context.CancelFunc, error) {
	if err := l.checkTenant(ctx); err != nil {
		return nil, nil, err
	}
	size := 0
	for key, values := range params {
		size += len(key)
		for _, v := range values {
			size += len(v)
		}
		if size > l.cfg.MaxQueryBytes {
			return nil, nil, NewError(413, "bad_data", "query parameters exceed configured maximum")
		}
	}
	timeout := l.cfg.Timeout
	if s := params.Get("timeout"); s != "" {
		d, err := queryDuration(s)
		if err != nil || d <= 0 {
			return nil, nil, NewError(400, "bad_data", "invalid timeout")
		}
		if d < timeout {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, cancel, nil
}

func queryDuration(s string) (time.Duration, error) {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > float64(math.MaxInt64)/float64(time.Second) {
			return 0, errors.New("invalid duration")
		}
		return time.Duration(v * float64(time.Second)), nil
	}
	v, err := model.ParseDuration(s)
	return time.Duration(v), err
}
func queryTime(s string) (time.Time, error) {
	v, err := parseTimestamp(s)
	if err != nil || v < 0 || v > 253402300799 {
		return time.Time{}, errors.New("invalid timestamp")
	}
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9)), nil
}

func (l *Local) Query(ctx context.Context, params url.Values, ranged bool) (json.RawMessage, error) {
	ctx, cancel, err := l.queryContext(ctx, params)
	if err != nil {
		return nil, err
	}
	defer cancel()
	release, err := l.admit(ctx, "/api/v1/query")
	if err != nil {
		return nil, err
	}
	defer release()
	expression := params.Get("query")
	if expression == "" {
		return nil, NewError(400, "bad_data", "query is required")
	}
	end := l.now()
	start := end
	var step time.Duration
	if ranged {
		start, err = queryTime(params.Get("start"))
		if err != nil {
			return nil, NewError(400, "bad_data", "invalid start")
		}
		end, err = queryTime(params.Get("end"))
		if err != nil {
			return nil, NewError(400, "bad_data", "invalid end")
		}
		step, err = queryDuration(params.Get("step"))
		if err != nil || step < time.Millisecond || end.Before(start) || end.Sub(start) > l.cfg.MaxQueryRange || end.Sub(start)/step > 11000 {
			return nil, NewError(400, "bad_data", "invalid range or step; maximum 7 days and 11000 steps")
		}
	} else if value := params.Get("time"); value != "" {
		end, err = queryTime(value)
		if err != nil {
			return nil, NewError(400, "bad_data", "invalid time")
		}
	}
	limit := l.cfg.MaxSeries
	if value := params.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > l.cfg.MaxSeries {
			return nil, NewError(400, "bad_data", "invalid result series limit")
		}
	}
	var body json.RawMessage
	err = l.snapshot(ctx, func(q *localQueryable) error {
		opts := promql.NewPrometheusQueryOpts(false, 5*time.Minute)
		var query promql.Query
		if ranged {
			query, err = l.engine.NewRangeQuery(ctx, q, opts, expression, start, end, step)
		} else {
			query, err = l.engine.NewInstantQuery(ctx, q, opts, expression, end)
		}
		if err != nil {
			return NewError(400, "bad_data", err.Error())
		}
		defer query.Close()
		result := query.Exec(ctx)
		if result.Err != nil {
			return NewError(422, "execution", result.Err.Error())
		}
		count := 0
		switch value := result.Value.(type) {
		case promql.Vector:
			count = len(value)
		case promql.Matrix:
			count = len(value)
		}
		if count > limit {
			return NewError(422, "execution", "query result exceeds series limit; narrow selectors or aggregate")
		}
		warnings, infos := result.Warnings.AsStrings(expression, 10, 10)
		body, err = l.encodeResult(struct {
			Status   string   `json:"status"`
			Data     any      `json:"data"`
			Warnings []string `json:"warnings,omitempty"`
			Infos    []string `json:"infos,omitempty"`
		}{"success", map[string]any{"resultType": result.Value.Type(), "result": result.Value}, warnings, infos})
		return err
	})
	return body, localQueryError(ctx, err)
}

func localQueryError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return NewError(503, "timeout", "local query canceled or timed out")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return err
	}
	return NewError(503, "unavailable", "local query storage unavailable")
}
func (l *Local) encodeResult(value any) (json.RawMessage, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, NewError(500, "internal", "cannot encode local query result")
	}
	if int64(len(body)) > l.cfg.MaxResponseBytes {
		return nil, NewError(422, "execution", "query response exceeds configured maximum")
	}
	return body, nil
}

func (l *Local) MetricNames(ctx context.Context, params url.Values) (json.RawMessage, error) {
	ctx, cancel, err := l.queryContext(ctx, params)
	if err != nil {
		return nil, err
	}
	defer cancel()
	release, err := l.admit(ctx, "/api/v1/label/__name__/values")
	if err != nil {
		return nil, err
	}
	defer release()
	start, err := queryTime(params.Get("start"))
	if err != nil {
		return nil, NewError(400, "bad_data", "invalid start")
	}
	end, err := queryTime(params.Get("end"))
	if err != nil || end.Before(start) || end.Sub(start) > l.cfg.MaxQueryRange {
		return nil, NewError(400, "bad_data", "invalid metric name time range")
	}
	selectors := params["match[]"]
	if len(selectors) == 0 {
		selectors = []string{`{__name__=~".+"}`}
	}
	if len(selectors) > 16 {
		return nil, NewError(400, "bad_data", "too many metric selectors")
	}
	names := map[string]bool{}
	err = l.snapshot(ctx, func(q *localQueryable) error {
		querier, err := q.Querier(start.UnixMilli(), end.UnixMilli())
		if err != nil {
			return err
		}
		for _, selector := range selectors {
			matchers, err := parser.NewParser(parser.Options{}).ParseMetricSelector(selector)
			if err != nil {
				return NewError(400, "bad_data", "invalid metric selector")
			}
			values, _, err := querier.LabelValues(ctx, labels.MetricName, nil, matchers...)
			if err != nil {
				return err
			}
			for _, name := range values {
				names[name] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, localQueryError(ctx, err)
	}
	list := make([]string, 0, len(names))
	for name := range names {
		list = append(list, name)
	}
	sort.Strings(list)
	if len(list) > l.cfg.MaxSeries {
		return nil, NewError(422, "execution", "metric names exceed series limit")
	}
	return l.encodeResult(map[string]any{"status": "success", "data": list})
}

func (l *Local) snapshot(ctx context.Context, fn func(*localQueryable) error) error {
	opts := &sql.TxOptions{ReadOnly: true}
	if l.db.Dialector.Name() == "postgres" {
		opts.Isolation = sql.LevelRepeatableRead
	}
	return l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(&localQueryable{tx: tx, tenant: l.cfg.TenantID, cutoff: l.now().Add(-l.retention).UnixMilli(), maxSeries: l.cfg.MaxSeries, maxRange: l.cfg.MaxQueryRange})
	}, opts)
}

type localQueryable struct {
	tx                         *gorm.DB
	tenant                     string
	cutoff                     int64
	maxSeries                  int
	maxRange                   time.Duration
	candidates, samples, bytes int
}

func (q *localQueryable) Querier(mint, maxt int64) (promstorage.Querier, error) {
	if maxt < mint || float64(maxt)-float64(mint) > float64((q.maxRange+5*time.Minute).Milliseconds()) {
		return nil, errors.New("local selector range exceeds configured maximum")
	}
	if mint < q.cutoff {
		mint = q.cutoff
	}
	return &localQuerier{q: q, mint: mint, maxt: maxt}, nil
}

type localQuerier struct {
	q          *localQueryable
	mint, maxt int64
}

func (*localQuerier) Close() error { return nil }

type labelledSeries struct {
	row    localstorage.PromQLSeries
	labels labels.Labels
}

func (q *localQuerier) matching(ctx context.Context, matchers []*labels.Matcher) ([]labelledSeries, error) {
	if q.mint > q.maxt {
		return nil, nil
	}
	query := q.q.tx.WithContext(ctx).Model(&localstorage.PromQLSeries{}).Where("tenant_id = ? AND last_timestamp >= ?", q.q.tenant, q.mint)
	for _, m := range matchers {
		if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
			query = query.Where("name = ?", m.Value)
		}
	}
	// SQL suppresses oversized values before driver allocation. Candidate and byte
	// bounds apply before label parsing or sample retrieval, including unmatched rows.
	rows, err := query.Select("id, CASE WHEN LENGTH(labels) <= ? THEN labels ELSE NULL END, last_timestamp", localMaxLabelsBytes).Order("id").Limit(localMaxCandidates - q.q.candidates + 1).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var selected []labelledSeries
	for rows.Next() {
		var id string
		var text sql.NullString
		var last int64
		if err := rows.Scan(&id, &text, &last); err != nil {
			return nil, err
		}
		q.q.candidates++
		if q.q.candidates > localMaxCandidates || !text.Valid {
			return nil, errors.New("local metric candidate or label limit exceeded")
		}
		q.q.bytes += len(text.String)
		if q.q.bytes > localMaxReadBytes {
			return nil, errors.New("local query read byte limit exceeded")
		}
		var ls labels.Labels
		if err := json.Unmarshal([]byte(text.String), &ls); err != nil {
			return nil, err
		}
		matches := true
		for _, m := range matchers {
			if !m.Matches(ls.Get(m.Name)) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if len(selected) >= q.q.maxSeries {
			return nil, errors.New("local query input series limit exceeded")
		}
		selected = append(selected, labelledSeries{row: localstorage.PromQLSeries{ID: id}, labels: ls})
	}
	return selected, rows.Err()
}
func (q *localQuerier) Select(ctx context.Context, sorted bool, hints *promstorage.SelectHints, matchers ...*labels.Matcher) promstorage.SeriesSet {
	selected, err := q.matching(ctx, matchers)
	if err != nil {
		return promstorage.ErrSeriesSet(err)
	}
	set := &localSeriesSet{index: -1}
	for _, s := range selected {
		samples, err := q.readSamples(ctx, s.row.ID, hints)
		if err != nil {
			return promstorage.ErrSeriesSet(err)
		}
		if len(samples) > 0 {
			set.series = append(set.series, promstorage.NewListSeries(s.labels, samples))
		}
	}
	if sorted {
		sort.Slice(set.series, func(i, j int) bool { return labels.Compare(set.series[i].Labels(), set.series[j].Labels()) < 0 })
	}
	return set
}
func (q *localQuerier) readSamples(ctx context.Context, id string, hints *promstorage.SelectHints) ([]chunks.Sample, error) {
	mint, maxt := q.mint, q.maxt
	if hints != nil {
		if hints.Start > mint {
			mint = hints.Start
		}
		if hints.End < maxt {
			maxt = hints.End
		}
	}
	rows, err := q.q.tx.WithContext(ctx).Model(&localstorage.PromQLSample{}).
		Select("timestamp, start_timestamp, CASE WHEN LENGTH(payload) <= ? THEN payload ELSE NULL END", localMaxSampleBytes).
		Where("series_id = ? AND timestamp >= ? AND timestamp <= ?", id, mint, maxt).Order("timestamp").
		Limit(localMaxSamples - q.q.samples + 1).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []chunks.Sample
	for rows.Next() {
		var t, st int64
		var data []byte
		if err := rows.Scan(&t, &st, &data); err != nil {
			return nil, err
		}
		q.q.samples++
		q.q.bytes += len(data)
		if q.q.samples > localMaxSamples || q.q.bytes > localMaxReadBytes {
			return nil, errors.New("local query sample or byte limit exceeded")
		}
		if len(data) == 0 {
			return nil, errors.New("invalid local metric payload")
		}
		var ts prompb.TimeSeries
		if err := ts.Unmarshal(data); err != nil {
			return nil, err
		}
		sample := &localSample{t: t, st: st}
		switch {
		case len(ts.Samples) == 1 && len(ts.Histograms) == 0:
			if ts.Samples[0].Timestamp != t {
				return nil, errors.New("invalid local sample timestamp")
			}
			sample.f = ts.Samples[0].Value
		case len(ts.Histograms) == 1 && len(ts.Samples) == 0:
			if ts.Histograms[0].Timestamp != t {
				return nil, errors.New("invalid local histogram timestamp")
			}
			sample.fh = ts.Histograms[0].ToFloatHistogram()
			if sample.fh == nil {
				return nil, errors.New("invalid local histogram")
			}
			if err := sample.fh.Validate(); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("invalid local sample cardinality")
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}
func (q *localQuerier) LabelValues(ctx context.Context, name string, _ *promstorage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	selected, err := q.matching(ctx, matchers)
	if err != nil {
		return nil, nil, err
	}
	values := map[string]bool{}
	for _, s := range selected {
		var stamp int64
		result := q.q.tx.WithContext(ctx).Model(&localstorage.PromQLSample{}).Select("timestamp").Where("series_id = ? AND timestamp >= ? AND timestamp <= ?", s.row.ID, q.mint, q.maxt).Limit(1).Scan(&stamp)
		if result.Error != nil {
			return nil, nil, result.Error
		}
		if result.RowsAffected > 0 {
			if value := s.labels.Get(name); value != "" {
				values[value] = true
			}
		}
	}
	out := make([]string, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil, nil
}
func (q *localQuerier) LabelNames(context.Context, *promstorage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, fmt.Errorf("label name discovery is not exposed by local PromQL")
}

type localSeriesSet struct {
	series []promstorage.Series
	index  int
}

func (s *localSeriesSet) Next() bool                      { s.index++; return s.index < len(s.series) }
func (s *localSeriesSet) At() promstorage.Series          { return s.series[s.index] }
func (*localSeriesSet) Err() error                        { return nil }
func (*localSeriesSet) Warnings() annotations.Annotations { return nil }

type localSample struct {
	t, st int64
	f     float64
	fh    *histogram.FloatHistogram
}

func (s *localSample) T() int64                      { return s.t }
func (s *localSample) ST() int64                     { return s.st }
func (s *localSample) F() float64                    { return s.f }
func (*localSample) H() *histogram.Histogram         { return nil }
func (s *localSample) FH() *histogram.FloatHistogram { return s.fh }
func (s *localSample) Type() chunkenc.ValueType {
	if s.fh != nil {
		return chunkenc.ValFloatHistogram
	}
	return chunkenc.ValFloat
}
func (s *localSample) Copy() chunks.Sample {
	copy := *s
	if s.fh != nil {
		copy.fh = s.fh.Copy()
	}
	return &copy
}
