package ingest

import (
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	spb "google.golang.org/genproto/googleapis/rpc/status"
)

const (
	defaultMaxBodyBytes = 4 * 1024 * 1024 // 4MB
	contentTypeProtobuf = "application/x-protobuf"
	contentTypeJSON     = "application/json"
)

// HTTPHandler provides HTTP OTLP endpoints that delegate to the existing gRPC Export() methods.
type HTTPHandler struct {
	traces       *TraceServer
	logs         *LogsServer
	metrics      *MetricsServer
	maxBodyBytes int64
}

// NewHTTPHandler creates an HTTP OTLP handler wrapping the existing gRPC servers.
func NewHTTPHandler(traces *TraceServer, logs *LogsServer, metrics *MetricsServer) *HTTPHandler {
	return &HTTPHandler{
		traces:       traces,
		logs:         logs,
		metrics:      metrics,
		maxBodyBytes: defaultMaxBodyBytes,
	}
}

// SetMaxBodyBytes configures the maximum request body size.
func (h *HTTPHandler) SetMaxBodyBytes(n int64) {
	if n > 0 {
		h.maxBodyBytes = n
	}
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
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	req := &coltracepb.ExportTraceServiceRequest{}
	if err := h.unmarshal(r, body, req); err != nil {
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.traces.Export(r.Context(), req)
	if err != nil {
		slog.Error("HTTP OTLP traces export failed", "error", err)
		writeOTLPError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeResponse(w, r, resp)
}

func (h *HTTPHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	req := &collogspb.ExportLogsServiceRequest{}
	if err := h.unmarshal(r, body, req); err != nil {
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.logs.Export(r.Context(), req)
	if err != nil {
		slog.Error("HTTP OTLP logs export failed", "error", err)
		writeOTLPError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeResponse(w, r, resp)
}

func (h *HTTPHandler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	req := &colmetricspb.ExportMetricsServiceRequest{}
	if err := h.unmarshal(r, body, req); err != nil {
		writeOTLPError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.metrics.Export(r.Context(), req)
	if err != nil {
		slog.Error("HTTP OTLP metrics export failed", "error", err)
		writeOTLPError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeResponse(w, r, resp)
}

// readBody reads the request body with size limit and optional gzip decompression.
func (h *HTTPHandler) readBody(r *http.Request) ([]byte, error) {
	var reader io.Reader = http.MaxBytesReader(nil, r.Body, h.maxBodyBytes)

	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gz.Close()
		reader = gz
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
	ct := r.Header.Get("Content-Type")
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
	ct := r.Header.Get("Content-Type")
	if ct == contentTypeJSON {
		w.Header().Set("Content-Type", contentTypeJSON)
		data, err := protojson.Marshal(msg)
		if err != nil {
			writeOTLPError(w, http.StatusInternalServerError, "failed to marshal response")
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	} else {
		w.Header().Set("Content-Type", contentTypeProtobuf)
		data, err := proto.Marshal(msg)
		if err != nil {
			writeOTLPError(w, http.StatusInternalServerError, "failed to marshal response")
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}
}

// writeOTLPError writes an OTLP-compliant error response.
func writeOTLPError(w http.ResponseWriter, statusCode int, msg string) {
	// OTLP HTTP spec: errors are returned as Status protobuf
	status := &spb.Status{
		Code:    int32(statusCode),
		Message: msg,
	}
	data, err := proto.Marshal(status)
	if err != nil {
		http.Error(w, msg, statusCode)
		return
	}
	w.Header().Set("Content-Type", contentTypeProtobuf)
	w.WriteHeader(statusCode)
	w.Write(data)
}
