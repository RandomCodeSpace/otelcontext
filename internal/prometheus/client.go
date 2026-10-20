// Package prometheus provides tenant-bound OTLP metric storage and PromQL,
// using either the local database and embedded engine or an external server.
package prometheus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"golang.org/x/sync/semaphore"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Config bounds requests and shared ingest admission. Zero values select
// defaults; negative values fail construction. URL is operator configuration, never taken from a request.
type Config struct {
	URL                 string
	TenantID            string
	Timeout             time.Duration
	MaxConcurrent       int
	MaxIngestConcurrent int
	MaxIngestWaiters    int
	MaxRequestBytes     int64
	MaxResponseBytes    int64
	MaxQueryBytes       int
	MaxQueryRange       time.Duration
	MaxSeries           int

	// MaxIngestBytes bounds the sum of proto.Size across active and waiting
	// exports. Zero selects 128 MiB, leaving headroom on a 4 GiB app for decoded
	// objects, active encoded copies and other work. This is a protobuf wire-size
	// budget, not a Go heap/RSS limit; inbound network decoding precedes Export
	// and is not covered. Decoded objects can be much larger than proto.Size.
	MaxIngestBytes int64
}

type Client struct {
	base          *url.URL
	cfg           Config
	http          *http.Client
	slots         chan struct{}
	ingestSlots   chan struct{}
	ingestWaiters chan struct{}
	ingestBytes   *semaphore.Weighted
	readySlots    chan struct{}
}

func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("Prometheus URL must be an absolute HTTP(S) base URL without credentials, query or fragment")
	}
	c, err := newBackendClient(cfg)
	if err != nil {
		return nil, err
	}
	cfg = c.cfg
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = cfg.MaxConcurrent + cfg.MaxIngestConcurrent + 1
	transport.MaxIdleConnsPerHost = transport.MaxConnsPerHost
	transport.ResponseHeaderTimeout = cfg.Timeout
	transport.MaxResponseHeaderBytes = 64 << 10
	c.base = u
	c.http = &http.Client{
		Transport: transport, Timeout: cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// newBackendClient validates limits and allocates admission queues together.
// Both backends use this path; only NewClient attaches an HTTP transport.
func newBackendClient(cfg Config) (*Client, error) {
	if cfg.TenantID == "" {
		cfg.TenantID = storage.DefaultTenantID
	}
	if storage.SanitizeTenantID(cfg.TenantID) != cfg.TenantID {
		return nil, errors.New("invalid Prometheus tenant ID")
	}
	if cfg.Timeout < 0 || cfg.MaxConcurrent < 0 || cfg.MaxIngestConcurrent < 0 || cfg.MaxIngestWaiters < 0 || cfg.MaxIngestBytes < 0 || cfg.MaxRequestBytes < 0 || cfg.MaxResponseBytes < 0 || cfg.MaxResponseBytes == math.MaxInt64 || cfg.MaxQueryBytes < 0 || cfg.MaxQueryRange < 0 || cfg.MaxSeries < 0 {
		return nil, errors.New("Prometheus limits must be positive")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 8
	}
	if cfg.MaxIngestConcurrent == 0 {
		cfg.MaxIngestConcurrent = 16
	}
	if cfg.MaxIngestWaiters == 0 {
		cfg.MaxIngestWaiters = 256
	}
	if cfg.MaxIngestBytes == 0 {
		cfg.MaxIngestBytes = 128 << 20
	}
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = 64 << 20
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 8 << 20
	}
	if cfg.MaxQueryBytes == 0 {
		cfg.MaxQueryBytes = 64 << 10
	}
	if cfg.MaxQueryRange == 0 {
		cfg.MaxQueryRange = 7 * 24 * time.Hour
	}
	if cfg.MaxSeries == 0 {
		cfg.MaxSeries = 1000
	}
	// Enforce the operator-facing bounds here too: callers can construct a
	// client without passing through the application's configuration validator.
	if cfg.MaxConcurrent > 64 || cfg.MaxIngestConcurrent > 256 || cfg.MaxIngestWaiters > 4096 {
		return nil, errors.New("Prometheus admission limits exceed maxima: 64 queries, 256 active exports, 4096 waiting exports")
	}
	return &Client{cfg: cfg, slots: make(chan struct{}, cfg.MaxConcurrent),
		ingestSlots: make(chan struct{}, cfg.MaxIngestConcurrent), ingestWaiters: make(chan struct{}, cfg.MaxIngestWaiters),
		ingestBytes: semaphore.NewWeighted(cfg.MaxIngestBytes), readySlots: make(chan struct{}, 1)}, nil
}

func (c *Client) TenantID() string             { return c.cfg.TenantID }
func (c *Client) MaxQueryBytes() int           { return c.cfg.MaxQueryBytes }
func (c *Client) MaxQueryRange() time.Duration { return c.cfg.MaxQueryRange }
func (c *Client) MaxResponseBytes() int64      { return c.cfg.MaxResponseBytes }
func (c *Client) Close()                       { c.http.CloseIdleConnections() }

// Ready probes the configured backend independently of queries and ingest.
// The caller should supply a short readiness deadline. It proves reachability
// and Prometheus readiness, not the retention configuration or OTLP enablement.
func (c *Client) Ready(ctx context.Context) error {
	status, _, _, err := c.request(ctx, "/-/ready", "", func() ([]byte, error) { return nil, nil })
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return NewError(http.StatusServiceUnavailable, "unavailable", fmt.Sprintf("Prometheus readiness returned HTTP %d", status))
	}
	return nil
}

// SupportsTenant follows the existing bound-principal precedence. It does not
// introduce authentication: missing tenant context still means "default".
func (c *Client) SupportsTenant(ctx context.Context) bool {
	tenant := storage.TenantFromContext(ctx)
	if bound, ok := authn.BoundTenantFromContext(ctx); ok {
		tenant = bound
	}
	return storage.SanitizeTenantID(tenant) == tenant && tenant == c.cfg.TenantID
}

// APIError retains the upstream envelope, including warnings and partial data.
// Local transport/limit errors use the same public Prometheus error shape.
type APIError struct {
	StatusCode int
	ErrorType  string
	Message    string
	Body       json.RawMessage
}

func (e *APIError) Error() string { return e.Message }

func NewError(status int, kind, message string) *APIError {
	body, _ := json.Marshal(map[string]string{"status": "error", "errorType": kind, "error": message})
	return &APIError{StatusCode: status, ErrorType: kind, Message: message, Body: body}
}

// ErrorResponse is shared by HTTP adapters; callers can also errors.As APIError.
func ErrorResponse(err error) (int, json.RawMessage) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = NewError(http.StatusBadGateway, "internal", "Prometheus request failed")
	}
	return apiErr.StatusCode, apiErr.Body
}

func (c *Client) checkTenant(ctx context.Context) error {
	if !c.SupportsTenant(ctx) {
		return NewError(http.StatusNotImplemented, "unavailable", "PromQL is not configured for this tenant")
	}
	return nil
}

// Query returns the complete Prometheus response envelope on success AND on an
// upstream API error. It does not parse, rewrite or restrict PromQL expressions.
func (c *Client) Query(ctx context.Context, params url.Values, rangeQuery bool) (json.RawMessage, error) {
	if err := c.checkTenant(ctx); err != nil {
		return nil, err
	}
	path := "/api/v1/query"
	if rangeQuery {
		path = "/api/v1/query_range"
	}
	if rangeQuery {
		if err := c.checkRange(params); err != nil {
			return nil, err
		}
	}
	return c.queryAPI(ctx, path, params)
}

// MetricNames discovers retained series through Prometheus's label API. The
// caller supplies bounded start/end and optional match[] selectors.
func (c *Client) MetricNames(ctx context.Context, params url.Values) (json.RawMessage, error) {
	if err := c.checkTenant(ctx); err != nil {
		return nil, err
	}
	if err := c.checkRange(params); err != nil {
		return nil, err
	}
	return c.queryAPI(ctx, "/api/v1/label/__name__/values", params)
}

func (c *Client) checkRange(params url.Values) error {
	start, e1 := parseTimestamp(params.Get("start"))
	end, e2 := parseTimestamp(params.Get("end"))
	// Prometheus owns parameter errors (including missing values and end < start).
	if e1 == nil && e2 == nil && end-start > c.cfg.MaxQueryRange.Seconds() {
		return NewError(http.StatusBadRequest, "bad_data", "query range exceeds configured maximum")
	}
	return nil
}

func parseTimestamp(value string) (float64, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return float64(t.Unix()) + float64(t.Nanosecond())/1e9, nil
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("invalid timestamp")
	}
	return f, nil
}

func (c *Client) queryAPI(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	// Bound work before URL escaping allocates its output. The encoded form
	// is checked again below because escaping can expand individual bytes.
	remaining := c.cfg.MaxQueryBytes
	for key, values := range params {
		for _, value := range values {
			if len(key) > remaining || len(value) > remaining-len(key) {
				return nil, NewError(http.StatusRequestEntityTooLarge, "bad_data", "query parameters exceed configured maximum")
			}
			remaining -= len(key) + len(value)
			if remaining < 2 {
				return nil, NewError(http.StatusRequestEntityTooLarge, "bad_data", "query parameters exceed configured maximum")
			}
			remaining -= 2
		}
	}
	encoded := params.Encode()
	if len(encoded) > c.cfg.MaxQueryBytes {
		return nil, NewError(http.StatusRequestEntityTooLarge, "bad_data", "query parameters exceed configured maximum")
	}
	status, body, _, err := c.request(ctx, path, "application/x-www-form-urlencoded", func() ([]byte, error) { return []byte(encoded), nil })
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || (envelope.Status != "success" && envelope.Status != "error") {
		return nil, NewError(http.StatusBadGateway, "internal", "invalid Prometheus response envelope")
	}
	if status != http.StatusOK || envelope.Status == "error" {
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		return body, &APIError{StatusCode: status, ErrorType: envelope.ErrorType, Message: envelope.Error, Body: body}
	}
	if path == "/api/v1/query" || path == "/api/v1/query_range" {
		var result struct {
			Data struct {
				ResultType string          `json:"resultType"`
				Result     json.RawMessage `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, NewError(http.StatusBadGateway, "internal", "invalid Prometheus query result")
		}
		if result.Data.ResultType == "matrix" || result.Data.ResultType == "vector" {
			var series []json.RawMessage
			if err := json.Unmarshal(result.Data.Result, &series); err != nil {
				return nil, NewError(http.StatusBadGateway, "internal", "invalid Prometheus series")
			}
			if len(series) > c.cfg.MaxSeries {
				return nil, NewError(http.StatusUnprocessableEntity, "execution", "query result exceeds configured series maximum")
			}
		}
	}
	return body, nil
}

// Export sends the original resource/scope/metric protobuf without converting
// names, units, temporality, histograms, exemplars, flags or attributes.
// Ingest must resolve and select resource tenants before calling Export.
func (c *Client) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	if err := c.checkTenant(ctx); err != nil {
		return nil, err
	}
	// Stable Prometheus requires cumulative input. Its optional experimental
	// delta converter is deliberately outside this contract. Reject the whole
	// export before any write rather than acknowledge silently discarded deltas.
	for _, resource := range req.GetResourceMetrics() {
		for _, scope := range resource.GetScopeMetrics() {
			for _, metric := range scope.GetMetrics() {
				if metric.GetSum().GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA ||
					metric.GetHistogram().GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA ||
					metric.GetExponentialHistogram().GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
					return nil, NewError(http.StatusBadRequest, "bad_data", "delta metric temporality is unsupported by this Prometheus backend; cumulative OTLP export required")
				}
			}
		}
	}
	requestBytes := int64(proto.Size(req))
	if requestBytes > c.cfg.MaxRequestBytes {
		return nil, NewError(http.StatusRequestEntityTooLarge, "bad_data", "OTLP request exceeds configured maximum")
	}
	// Reserve before waiting for a write slot or allocating an encoded copy.
	// Holding the reservation through Export also covers every error/cancel path.
	if !c.ingestBytes.TryAcquire(requestBytes) {
		return nil, NewError(http.StatusServiceUnavailable, "unavailable", "Prometheus ingest byte limit reached")
	}
	defer c.ingestBytes.Release(requestBytes)
	status, data, contentType, err := c.request(ctx, "/api/v1/otlp/v1/metrics", "application/x-protobuf", func() ([]byte, error) { return proto.Marshal(req) })
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		var detail statuspb.Status
		if strings.HasPrefix(contentType, "application/json") {
			_ = protojson.Unmarshal(data, &detail)
		} else {
			_ = proto.Unmarshal(data, &detail)
		}
		message := detail.GetMessage()
		if message == "" && strings.HasPrefix(contentType, "text/plain") {
			message = string(data)
		}
		if len(message) > 1024 {
			message = message[:1024]
		}
		return nil, NewError(status, "unavailable", strings.TrimSpace(fmt.Sprintf("Prometheus OTLP receiver returned HTTP %d: %s", status, message)))
	}
	resp := new(colmetricspb.ExportMetricsServiceResponse)
	if strings.HasPrefix(contentType, "application/json") {
		err = protojson.Unmarshal(data, resp)
	} else {
		err = proto.Unmarshal(data, resp)
	}
	if err != nil {
		return nil, NewError(http.StatusBadGateway, "internal", "invalid Prometheus OTLP response")
	}
	return resp, nil
}

func (c *Client) request(ctx context.Context, path, contentType string, encode func() ([]byte, error)) (int, []byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	if ctx.Err() != nil {
		return 0, nil, "", NewError(http.StatusServiceUnavailable, "canceled", "Prometheus request canceled")
	}
	release, err := c.admit(ctx, path)
	if err != nil {
		return 0, nil, "", err
	}
	defer release()
	body, err := encode()
	if err != nil {
		return 0, nil, "", err
	}
	if int64(len(body)) > c.cfg.MaxRequestBytes {
		return 0, nil, "", NewError(http.StatusRequestEntityTooLarge, "bad_data", "request exceeds configured maximum")
	}
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	method := http.MethodPost
	if path == "/api/v1/label/__name__/values" || path == "/-/ready" {
		method, u.RawQuery = http.MethodGet, string(body)
		body = nil
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		kind, message := "unavailable", "Prometheus backend unavailable"
		if errors.Is(err, context.DeadlineExceeded) {
			kind, message = "timeout", "Prometheus request timed out"
		} else if errors.Is(err, context.Canceled) {
			kind, message = "canceled", "Prometheus request canceled"
		}
		return 0, nil, "", NewError(http.StatusServiceUnavailable, kind, message)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	if err != nil {
		return 0, nil, "", NewError(http.StatusBadGateway, "internal", "failed to read Prometheus response")
	}
	if int64(len(data)) > c.cfg.MaxResponseBytes {
		return 0, nil, "", NewError(http.StatusBadGateway, "internal", "Prometheus response exceeds configured maximum")
	}
	return resp.StatusCode, data, resp.Header.Get("Content-Type"), nil
}

func (c *Client) admit(ctx context.Context, path string) (func(), error) {
	if path == "/api/v1/otlp/v1/metrics" {
		// A finite waiter pool holds only the existing request, not an encoded
		// copy. Queued exporters time out with their caller; readers cannot
		// consume write slots. Try a free slot first to avoid queue overhead.
		select {
		case c.ingestSlots <- struct{}{}:
			return func() { <-c.ingestSlots }, nil
		default:
		}
		select {
		case c.ingestWaiters <- struct{}{}:
			defer func() { <-c.ingestWaiters }()
		default:
			return nil, NewError(http.StatusServiceUnavailable, "unavailable", "Prometheus ingest waiter limit reached")
		}
		select {
		case c.ingestSlots <- struct{}{}:
			return func() { <-c.ingestSlots }, nil
		case <-ctx.Done():
			return nil, NewError(http.StatusServiceUnavailable, "timeout", "Prometheus ingest admission canceled or timed out")
		}
	}
	slots := c.slots
	if path == "/-/ready" {
		slots = c.readySlots
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
		return nil, NewError(http.StatusServiceUnavailable, "unavailable", "Prometheus request concurrency limit reached")
	}
}
