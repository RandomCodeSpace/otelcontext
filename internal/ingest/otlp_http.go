package ingest

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// defaultRetryAfterSeconds is the Retry-After value advertised when the
// pipeline queue is full and the HTTP handler returns 429. Mirrors the
// gRPC RESOURCE_EXHAUSTED back-off semantics from Phase 1 — short enough
// that healthy clients recover quickly, long enough to give a stuck
// downstream a chance to drain.
const defaultRetryAfterSeconds = 1

// headerContentType is the canonical HTTP Content-Type header name. Local
// const so the OTLP-HTTP receiver compiles without pulling in a shared
// helper for a one-line literal.
const headerContentType = "Content-Type" //nolint:goconst // single literal; Sonar S1192 satisfied via const

// withTenantFromHTTP attaches a tenant ID from the X-Tenant-ID header (if any)
// to the request context before delegating to the gRPC Export methods.
// Uses the shared storage.WithTenantContext helper so ingest and read paths
// agree on the context key.
//
// The raw header value is run through storage.SanitizeTenantID — the same
// sanitizer applied on the gRPC metadata path in tenantFromContext — so
// control characters, oversized strings, and empty values are rejected
// identically regardless of transport.
func withTenantFromHTTP(r *http.Request) context.Context {
	if v := r.Header.Get("X-Tenant-ID"); v != "" {
		if sanitized := storage.SanitizeTenantID(v); sanitized != "" {
			return storage.WithTenantContext(r.Context(), sanitized)
		}
	}
	return r.Context()
}

const (
	defaultMaxBodyBytes = 4 * 1024 * 1024  // 4MB  — pre-decompression wire cap
	maxDecompressedBody = 64 * 1024 * 1024 // 64MB — post-gzip cap (zip-bomb guard)
	contentTypeProtobuf = "application/x-protobuf"
	contentTypeJSON     = "application/json"
)

// errDecompressedTooLarge is returned when a gzipped request body expands past
// maxDecompressedBody — a classic compression-bomb scenario.
var errDecompressedTooLarge = errors.New("decompressed body exceeds limit")

// HTTPHandler provides HTTP OTLP endpoints that delegate to the existing gRPC Export() methods.
type HTTPHandler struct {
	traces               *TraceServer
	logs                 *LogsServer
	metrics              *MetricsServer
	maxBodyBytes         int64 // pre-decompress wire size cap
	maxDecompressedBytes int64 // post-gzip decompressed size cap (zip-bomb guard)

	// onThrottle is invoked once per signal type (traces|logs|metrics) every
	// time the async ingest pipeline returns RESOURCE_EXHAUSTED and the HTTP
	// handler maps it to 429. nil-safe.
	onThrottle func(signal string)
}

// NewHTTPHandler creates an HTTP OTLP handler wrapping the existing gRPC servers.
func NewHTTPHandler(traces *TraceServer, logs *LogsServer, metrics *MetricsServer) *HTTPHandler {
	return &HTTPHandler{
		traces:               traces,
		logs:                 logs,
		metrics:              metrics,
		maxBodyBytes:         defaultMaxBodyBytes,
		maxDecompressedBytes: maxDecompressedBody,
	}
}

// SetMaxBodyBytes configures the maximum pre-decompression request body size.
func (h *HTTPHandler) SetMaxBodyBytes(n int64) {
	if n > 0 {
		h.maxBodyBytes = n
	}
}

// SetMaxDecompressedBytes configures the maximum post-gzip body size. Rejecting
// larger inputs defends against compression bombs from authed clients.
func (h *HTTPHandler) SetMaxDecompressedBytes(n int64) {
	if n > 0 {
		h.maxDecompressedBytes = n
	}
}

// SetThrottleCallback wires a per-signal counter that increments every time a
// 429 is returned because the async ingest pipeline is at capacity. Used by
// main.go to feed `otelcontext_http_otlp_throttled_total{signal=…}`.
func (h *HTTPHandler) SetThrottleCallback(fn func(signal string)) {
	h.onThrottle = fn
}

// isQueueFull reports whether the error returned by an Export() method is
// the gRPC RESOURCE_EXHAUSTED status used by the async pipeline to signal
// "queue at capacity". Used by the HTTP handlers to map back to 429.
func isQueueFull(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrQueueFull) {
		return true
	}
	if s, ok := grpcstatus.FromError(err); ok && s.Code() == codes.ResourceExhausted {
		return true
	}
	return false
}

// writeThrottled emits an OTLP-shaped 429 with a Retry-After header. The
// Retry-After value is duplicated in the protobuf Status message so clients
// that don't read headers (some custom OTLP shims) still see it.
func (h *HTTPHandler) writeThrottled(w http.ResponseWriter, signal string) {
	if h.onThrottle != nil {
		h.onThrottle(signal)
	}
	w.Header().Set("Retry-After", strconv.Itoa(defaultRetryAfterSeconds))
	writeOTLPError(w, http.StatusTooManyRequests, fmt.Sprintf("ingest pipeline at capacity, retry after %ds", defaultRetryAfterSeconds))
}

// RegisterRoutes registers the HTTP OTLP endpoints on the given mux.
func (h *HTTPHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/traces", h.handleTraces)
	mux.HandleFunc("POST /v1/logs", h.handleLogs)
	mux.HandleFunc("POST /v1/metrics", h.handleMetrics)
}

func (h *HTTPHandler) handleTraces(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errDecompressedTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeOTLPError(w, status, err.Error())
		return
	}

	req := &coltracepb.ExportTraceServiceRequest{}
	if err := h.unmarshal(r, body, req); err != nil {
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.traces.Export(withTenantFromHTTP(r), req)
	if err != nil {
		if isQueueFull(err) {
			h.writeThrottled(w, "traces")
			return
		}
		slog.Error("HTTP OTLP traces export failed", "error", err)
		writeOTLPError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeResponse(w, r, resp)
}

func (h *HTTPHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errDecompressedTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeOTLPError(w, status, err.Error())
		return
	}

	req := &collogspb.ExportLogsServiceRequest{}
	if err := h.unmarshal(r, body, req); err != nil {
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.logs.Export(withTenantFromHTTP(r), req)
	if err != nil {
		if isQueueFull(err) {
			h.writeThrottled(w, "logs")
			return
		}
		slog.Error("HTTP OTLP logs export failed", "error", err)
		writeOTLPError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeResponse(w, r, resp)
}

func (h *HTTPHandler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errDecompressedTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeOTLPError(w, status, err.Error())
		return
	}

	req := &colmetricspb.ExportMetricsServiceRequest{}
	if err := h.unmarshal(r, body, req); err != nil {
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.metrics.Export(withTenantFromHTTP(r), req)
	if err != nil {
		if isQueueFull(err) {
			h.writeThrottled(w, "metrics")
			return
		}
		slog.Error("HTTP OTLP metrics export failed", "error", err)
		writeOTLPError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeResponse(w, r, resp)
}

// readBody reads the request body with a pre-decompression size limit (wire
// cap) and, for gzipped requests, a separate post-decompression cap that
// prevents compression-bomb amplification from authed clients.
//
// Returns errDecompressedTooLarge when the decompressed stream exceeds
// h.maxDecompressedBytes so callers can map it to HTTP 413.
func (h *HTTPHandler) readBody(r *http.Request) ([]byte, error) {
	var reader io.Reader = http.MaxBytesReader(nil, r.Body, h.maxBodyBytes)

	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer func() { _ = gz.Close() }()
		// +1 so a body that is exactly maxDecompressedBytes still fits; anything
		// larger triggers the sentinel.
		limit := h.maxDecompressedBytes
		reader = io.LimitReader(gz, limit+1)
		body, err := io.ReadAll(reader)
		if err != nil {
			if err.Error() == "http: request body too large" {
				return nil, fmt.Errorf("request body exceeds %d bytes limit", h.maxBodyBytes)
			}
			return nil, fmt.Errorf("failed to read request body: %w", err)
		}
		if int64(len(body)) > limit {
			return nil, fmt.Errorf("%w: decompressed body > %d bytes", errDecompressedTooLarge, limit)
		}
		return body, nil
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		// Check if it's a MaxBytesError (request too large)
		if err.Error() == "http: request body too large" {
			return nil, fmt.Errorf("request body exceeds %d bytes limit", h.maxBodyBytes)
		}
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	return body, nil
}

// unmarshal decodes the body based on Content-Type header.
func (h *HTTPHandler) unmarshal(r *http.Request, body []byte, msg proto.Message) error {
	ct := r.Header.Get(headerContentType)
	switch ct {
	case contentTypeProtobuf, "":
		if err := proto.Unmarshal(body, msg); err != nil {
			return fmt.Errorf("failed to unmarshal protobuf: %w", err)
		}
	case contentTypeJSON:
		if err := protojson.Unmarshal(body, msg); err != nil {
			return fmt.Errorf("failed to unmarshal JSON: %w", err)
		}
	default:
		return fmt.Errorf("unsupported Content-Type: %s", ct)
	}
	return nil
}

// writeResponse marshals and writes the OTLP response.
func (h *HTTPHandler) writeResponse(w http.ResponseWriter, r *http.Request, msg proto.Message) {
	ct := r.Header.Get(headerContentType)
	if ct == contentTypeJSON {
		w.Header().Set(headerContentType, contentTypeJSON)
		data, err := protojson.Marshal(msg)
		if err != nil {
			writeOTLPError(w, http.StatusInternalServerError, "failed to marshal response")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	} else {
		w.Header().Set(headerContentType, contentTypeProtobuf)
		data, err := proto.Marshal(msg)
		if err != nil {
			writeOTLPError(w, http.StatusInternalServerError, "failed to marshal response")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// writeOTLPError writes an OTLP-compliant error response.
func writeOTLPError(w http.ResponseWriter, statusCode int, msg string) {
	// OTLP HTTP spec: errors are returned as Status protobuf
	status := &spb.Status{
		Code:    int32(statusCode), // #nosec G115 -- HTTP status code always fits int32
		Message: msg,
	}
	data, err := proto.Marshal(status)
	if err != nil {
		http.Error(w, msg, statusCode)
		return
	}
	w.Header().Set(headerContentType, contentTypeProtobuf)
	w.WriteHeader(statusCode)
	_, _ = w.Write(data)
}
