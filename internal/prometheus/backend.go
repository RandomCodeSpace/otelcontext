package prometheus

import (
	"context"
	"encoding/json"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"net/url"
	"time"
)

// Backend is shared by OTLP ingest, HTTP and MCP. Local uses the main database;
// Client retains the opt-in remote Prometheus behavior.
type Backend interface {
	Query(context.Context, url.Values, bool) (json.RawMessage, error)
	MetricNames(context.Context, url.Values) (json.RawMessage, error)
	Export(context.Context, *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error)
	TenantID() string
	SupportsTenant(context.Context) bool
	MaxQueryBytes() int
	MaxQueryRange() time.Duration
	MaxResponseBytes() int64
	Ready(context.Context) error
	Close()
	Source() string
}

func (*Client) Source() string { return "prometheus" }
