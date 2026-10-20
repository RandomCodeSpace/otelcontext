package prometheus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	localstorage "github.com/RandomCodeSpace/otelcontext/internal/storage"
	promconfig "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	promstorage "github.com/prometheus/prometheus/storage"
	converter "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Export converts and validates the entire batch before opening a transaction.
// A rejection or storage failure rolls back all samples. Retries with identical
// series/timestamps/payloads are idempotent; conflicting duplicates are rejected.
func (l *Local) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	if err := l.checkTenant(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, l.cfg.Timeout)
	defer cancel()
	size := int64(proto.Size(req))
	if size > l.cfg.MaxRequestBytes {
		return nil, NewError(413, "bad_data", "OTLP request exceeds configured maximum")
	}
	if !l.ingestBytes.TryAcquire(size) {
		return nil, NewError(503, "unavailable", "local ingest byte limit reached")
	}
	defer l.ingestBytes.Release(size)
	release, err := l.admit(ctx, "/api/v1/otlp/v1/metrics")
	if err != nil {
		return nil, err
	}
	defer release()
	data, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	request := pmetricotlp.NewExportRequest()
	if err := request.UnmarshalProto(data); err != nil {
		return nil, NewError(400, "bad_data", "invalid OTLP metrics")
	}
	if request.Metrics().DataPointCount() > localMaxBatchSamples {
		return nil, NewError(413, "bad_data", "OTLP batch exceeds 10000 datapoints")
	}
	now := l.now()
	a := &localAppender{tenant: l.cfg.TenantID, mint: now.Add(-l.retention).UnixMilli(), maxt: now.Add(5 * time.Minute).UnixMilli(),
		series: make(map[string]localstorage.PromQLSeries)}
	// NoTranslation retains dotted metric and label names. Resource identity is
	// part of each series; datapoint labels take precedence per upstream rules.
	_, err = converter.NewPrometheusConverter(a).FromMetrics(ctx, request.Metrics(), converter.Settings{
		AllowUTF8: true, KeepIdentifyingResourceAttributes: true,
		DisableTargetInfo: true, PromoteScopeMetadata: true,
		PromoteResourceAttributes: converter.NewPromoteResourceAttributes(promconfig.OTLPConfig{PromoteAllResourceAttributes: true}),
	})
	if err != nil || a.err != nil {
		return nil, NewError(http.StatusBadRequest, "bad_data", fmt.Sprintf("local OTLP batch rejected: %v", errors.Join(err, a.err)))
	}
	if err := ctx.Err(); err != nil {
		return nil, NewError(503, "canceled", "local export canceled")
	}
	err = l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Stable row-lock order also prevents cross-batch deadlocks on PostgreSQL.
		ids := make([]string, 0, len(a.series))
		for id := range a.series {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			s := a.series[id]
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}},
				DoUpdates: clause.Assignments(map[string]any{"last_timestamp": gorm.Expr("CASE WHEN promql_series.last_timestamp < ? THEN ? ELSE promql_series.last_timestamp END", s.LastTimestamp, s.LastTimestamp)})}).Create(&s).Error; err != nil {
				return err
			}
			var saved localstorage.PromQLSeries
			if err := tx.First(&saved, "id = ?", id).Error; err != nil {
				return err
			}
			if saved.TenantID != s.TenantID || saved.Labels != s.Labels {
				return errors.New("local metric identity conflict")
			}
		}
		sort.Slice(a.samples, func(i, j int) bool {
			x, y := a.samples[i], a.samples[j]
			if x.SeriesID != y.SeriesID {
				return x.SeriesID < y.SeriesID
			}
			return x.Timestamp < y.Timestamp
		})
		for _, sample := range a.samples {
			res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "series_id"}, {Name: "timestamp"}}, DoNothing: true}).Create(&sample)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				var old localstorage.PromQLSample
				if err := tx.Where("series_id = ? AND timestamp = ?", sample.SeriesID, sample.Timestamp).First(&old).Error; err != nil {
					return err
				}
				if old.StartTimestamp != sample.StartTimestamp || !bytes.Equal(old.Payload, sample.Payload) {
					return NewError(400, "bad_data", "conflicting sample at the same series timestamp")
				}
			}
		}
		return nil
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return nil, err
		}
		return nil, NewError(503, "unavailable", "local metric transaction failed")
	}
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

type localAppender struct {
	tenant     string
	mint, maxt int64
	series     map[string]localstorage.PromQLSeries
	samples    []localstorage.PromQLSample
	bytes      int
	err        error
}

func (a *localAppender) Commit() error   { return a.err } // Local owns the DB transaction.
func (a *localAppender) Rollback() error { a.samples = nil; a.series = nil; return nil }
func (a *localAppender) Append(_ promstorage.SeriesRef, ls labels.Labels, st, t int64, v float64, h *histogram.Histogram, fh *histogram.FloatHistogram, _ promstorage.AppendV2Options) (promstorage.SeriesRef, error) {
	if a.err != nil {
		return 0, a.err
	}
	fail := func(message string) (promstorage.SeriesRef, error) { a.err = errors.New(message); return 0, a.err }
	if len(a.samples) >= localMaxBatchSamples {
		return fail("local export exceeds 10000 translated samples")
	}
	if t < a.mint || t > a.maxt {
		return fail("metric timestamp is outside retention or more than five minutes in the future")
	}
	if ls.Len() > 128 || ls.Get(labels.MetricName) == "" {
		return fail("invalid or excessive metric labels")
	}
	labelBytes, err := json.Marshal(ls)
	if err != nil || len(labelBytes) > localMaxLabelsBytes {
		return fail("metric labels exceed 16 KiB")
	}
	hash := sha256.Sum256(append(append([]byte(a.tenant), 0), labelBytes...))
	id := hex.EncodeToString(hash[:])
	ts := prompb.TimeSeries{}
	switch {
	case h != nil:
		if err := h.Validate(); err != nil {
			return fail("invalid native histogram")
		}
		ts.Histograms = []prompb.Histogram{prompb.FromIntHistogram(t, h)}
	case fh != nil:
		if err := fh.Validate(); err != nil {
			return fail("invalid native histogram")
		}
		ts.Histograms = []prompb.Histogram{prompb.FromFloatHistogram(t, fh)}
	default:
		ts.Samples = []prompb.Sample{{Timestamp: t, Value: v}}
	}
	if ts.Size() > localMaxSampleBytes {
		return fail("metric sample exceeds 64 KiB")
	}
	payload, err := ts.Marshal()
	if err != nil {
		a.err = err
		return 0, err
	}
	charge := len(payload)
	if _, ok := a.series[id]; !ok {
		charge += len(labelBytes)
	}
	if a.bytes+charge > localMaxBatchBytes {
		return fail("translated local export exceeds 8 MiB")
	}
	a.bytes += charge
	s, ok := a.series[id]
	if !ok {
		s = localstorage.PromQLSeries{ID: id, TenantID: a.tenant, Name: ls.Get(labels.MetricName), Labels: string(labelBytes), LastTimestamp: t}
	}
	if s.LastTimestamp < t {
		s.LastTimestamp = t
	}
	a.series[id] = s
	a.samples = append(a.samples, localstorage.PromQLSample{SeriesID: id, Timestamp: t, StartTimestamp: st, Payload: payload})
	return 1, nil // The converter supplies full labels for every append.
}
