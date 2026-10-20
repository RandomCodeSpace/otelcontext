package prometheus

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/prometheus/promql"
	"gorm.io/gorm"
)

const (
	localMaxSamples      = 100000
	localMaxBatchSamples = 10000
	localMaxSampleBytes  = 64 << 10
	localMaxLabelsBytes  = 16 << 10
	localMaxReadBytes    = 32 << 20
	localMaxBatchBytes   = 8 << 20
	localMaxCandidates   = 10000
)

// Local evaluates PromQL against original samples in OtelContext's main DB.
// Client supplies only shared tenant checks and bounded admission; there is no
// HTTP client, remote endpoint, separate TSDB, or network call in this backend.
type Local struct {
	*Client
	db        *gorm.DB
	engine    *promql.Engine
	retention time.Duration
	now       func() time.Time
}

var _ Backend = (*Local)(nil)

func NewLocal(db *gorm.DB, cfg Config, retention time.Duration) (*Local, error) {
	c, err := newBackendClient(cfg)
	if err != nil {
		return nil, err
	}
	cfg = c.cfg
	if db == nil || retention <= 0 {
		return nil, errors.New("local PromQL requires a database and positive retention")
	}
	if db.Dialector.Name() != "sqlite" && db.Dialector.Name() != "postgres" {
		return nil, errors.New("local PromQL supports SQLite and PostgreSQL")
	}
	if cfg.MaxSeries > localMaxCandidates {
		return nil, errors.New("local PromQL series limit exceeds candidate bound")
	}
	return &Local{Client: c, db: db, retention: retention, now: time.Now,
		engine: promql.NewEngine(promql.EngineOpts{Logger: slog.Default(), MaxSamples: localMaxSamples,
			Timeout: cfg.Timeout, LookbackDelta: 5 * time.Minute, EnableAtModifier: true, EnableNegativeOffset: true,
			UseStartTimestamps:       true,
			NoStepSubqueryIntervalFn: func(int64) int64 { return 60000 },
		})}, nil
}
func (*Local) Source() string { return "local" }
func (*Local) Close()         {} // The repository owns the database lifetime.
func (l *Local) Ready(ctx context.Context) error {
	release, err := l.admit(ctx, "/-/ready")
	if err != nil {
		return err
	}
	defer release()
	var id string
	// Read-only; startup migration gate owns schema creation.
	if err := l.db.WithContext(ctx).Raw("SELECT s.id FROM promql_series s LEFT JOIN promql_samples p ON p.series_id = s.id LIMIT 1").Scan(&id).Error; err != nil {
		return NewError(http.StatusServiceUnavailable, "unavailable", "local PromQL storage unavailable")
	}
	return nil
}
