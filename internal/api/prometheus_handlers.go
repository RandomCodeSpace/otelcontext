package api

import (
	"encoding/json"
	"errors"
	"math"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
	"github.com/RandomCodeSpace/otelcontext/internal/prometheus"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

func writePrometheusError(w http.ResponseWriter, err error) {
	status, body := prometheus.ErrorResponse(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) handlePrometheusQuery(w http.ResponseWriter, r *http.Request) {
	s.servePrometheusQuery(w, r, false)
}

func (s *Server) handlePrometheusRangeQuery(w http.ResponseWriter, r *http.Request) {
	s.servePrometheusQuery(w, r, true)
}

func (s *Server) servePrometheusQuery(w http.ResponseWriter, r *http.Request, rangeQuery bool) {
	if s.prometheusClient == nil {
		writePrometheusError(w, prometheus.NewError(http.StatusServiceUnavailable, "unavailable", "Prometheus backend is not configured"))
		return
	}
	if !s.prometheusClient.SupportsTenant(r.Context()) {
		writePrometheusError(w, prometheus.NewError(http.StatusNotImplemented, "unavailable", "PromQL is not configured for this tenant"))
		return
	}
	if len(r.URL.RawQuery) > s.prometheusClient.MaxQueryBytes() {
		writePrometheusError(w, prometheus.NewError(http.StatusRequestEntityTooLarge, "bad_data", "query parameters exceed configured maximum"))
		return
	}
	if r.Method == http.MethodPost {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			writePrometheusError(w, prometheus.NewError(http.StatusBadRequest, "bad_data", "POST queries require application/x-www-form-urlencoded"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, int64(s.prometheusClient.MaxQueryBytes()))
	}
	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
		writePrometheusError(w, prometheus.NewError(status, "bad_data", "invalid or oversized query parameters"))
		return
	}
	body, err := s.prometheusClient.Query(r.Context(), r.Form, rangeQuery)
	if err != nil {
		writePrometheusError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// metricSelectors uses Prometheus NoTranslation: OTLP names and attribute keys
// stay exact. Quoted matchers prevent user-provided names becoming PromQL code.
// Host-only resources lack service.name and use host.id, then host.name, just
// as resolveServiceIdentity does on ingest. Explicit service names still win.
func metricSelectors(name, service string) []string {
	match := `__name__=~".+"`
	if name != "" {
		match = `__name__=` + strconv.Quote(name)
	}
	if service == "" {
		return []string{"{" + match + "}"}
	}
	selectors := []string{"{" + match + `,"service.name"=` + strconv.Quote(service) + "}"}
	if strings.HasPrefix(service, topology.HostPrefix) {
		host := strconv.Quote(strings.TrimPrefix(service, topology.HostPrefix))
		selectors = append(selectors,
			"{"+match+`,"service.name"="","host.id"=`+host+"}",
			"{"+match+`,"service.name"="","host.id"="","host.name"=`+host+"}")
	}
	if service == "unknown-service" {
		selectors = append(selectors, "{"+match+`,"service.name"="","host.name"="","host.id"=""}`)
	}
	return selectors
}

func (s *Server) prometheusMetricRange(r *http.Request, defaultRange time.Duration) (time.Time, time.Time, error) {
	end := time.Now().UTC()
	var err error
	if value := r.URL.Query().Get("end"); value != "" {
		end, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	start := end.Add(-defaultRange)
	if value := r.URL.Query().Get("start"); value != "" {
		start, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if end.Before(start) || end.Sub(start) > s.prometheusClient.MaxQueryRange() {
		return time.Time{}, time.Time{}, errors.New("invalid or over-limit metric time range")
	}
	return start, end, nil
}

// This compatibility view returns evaluated samples, not original ingest
// buckets. count=1 and min=max=sum=value preserve the existing UI's sum/count
// calculation. Native histograms require PromQL; flattening them would lie.
func (s *Server) handlePrometheusMetricBuckets(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > s.prometheusClient.MaxQueryBytes() {
		writePrometheusError(w, prometheus.NewError(http.StatusRequestEntityTooLarge, "bad_data", "query parameters exceed configured maximum"))
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writePrometheusError(w, prometheus.NewError(http.StatusBadRequest, "bad_data", "metric name is required"))
		return
	}
	start, end, err := s.prometheusMetricRange(r, time.Hour)
	if err != nil {
		writePrometheusError(w, prometheus.NewError(http.StatusBadRequest, "bad_data", "invalid metric time range"))
		return
	}
	step := math.Max(30, math.Ceil(end.Sub(start).Seconds()/1000))
	params := url.Values{
		"query": {strings.Join(metricSelectors(name, r.URL.Query().Get("service_name")), " or ")},
		"start": {start.Format(time.RFC3339Nano)}, "end": {end.Format(time.RFC3339Nano)},
		"step": {strconv.FormatFloat(step, 'f', 0, 64)},
	}
	body, err := s.prometheusClient.Query(r.Context(), params, true)
	if err != nil {
		writePrometheusError(w, err)
		return
	}
	var envelope struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric     map[string]string    `json:"metric"`
				Values     [][2]json.RawMessage `json:"values"`
				Histograms json.RawMessage      `json:"histograms"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Data.ResultType != "matrix" {
		writePrometheusError(w, prometheus.NewError(http.StatusBadGateway, "internal", "invalid Prometheus matrix response"))
		return
	}
	buckets := make([]views.MetricBucket, 0)
	var estimatedBytes int64
	for _, series := range envelope.Data.Result {
		if len(series.Histograms) > 0 && string(series.Histograms) != "[]" {
			writePrometheusError(w, prometheus.NewError(http.StatusUnprocessableEntity, "execution", "native histogram metrics require /api/v1/query or /api/v1/query_range"))
			return
		}
		service := series.Metric["service.name"]
		if service == "" {
			host := series.Metric["host.id"]
			if host == "" {
				host = series.Metric["host.name"]
			}
			if host != "" {
				service = topology.HostPrefix + host
			} else {
				service = "unknown-service"
			}
		}
		attrs, _ := json.Marshal(series.Metric)
		for _, sample := range series.Values {
			estimatedBytes += int64(len(attrs)*2 + len(name)*6 + len(service)*6 + 512)
			if estimatedBytes > s.prometheusClient.MaxResponseBytes() {
				writePrometheusError(w, prometheus.NewError(http.StatusUnprocessableEntity, "execution", "metric compatibility response exceeds configured maximum; use PromQL with aggregation"))
				return
			}
			var timestamp float64
			var text string
			if json.Unmarshal(sample[0], &timestamp) != nil || json.Unmarshal(sample[1], &text) != nil {
				writePrometheusError(w, prometheus.NewError(http.StatusBadGateway, "internal", "invalid Prometheus sample"))
				return
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				writePrometheusError(w, prometheus.NewError(http.StatusBadGateway, "internal", "invalid Prometheus sample value"))
				return
			}
			// JSON cannot represent NaN/Inf; the compatibility view reports
			// sampled coverage and leaves gaps. PromQL preserves these values.
			if math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}
			buckets = append(buckets, views.MetricBucket{Name: name, ServiceName: service,
				TimeBucket: time.Unix(0, int64(timestamp*1e9)).UTC(), Min: value, Max: value, Sum: value, Count: 1, AttributesJSON: string(attrs)})
		}
	}
	sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].TimeBucket.Before(buckets[j].TimeBucket) })
	setCoverage(w, aggregate.CoverageSampled)
	w.Header().Set("OtelContext-Metric-Source", "prometheus")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buckets)
}

func (s *Server) handlePrometheusMetricNames(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > s.prometheusClient.MaxQueryBytes() {
		writePrometheusError(w, prometheus.NewError(http.StatusRequestEntityTooLarge, "bad_data", "query parameters exceed configured maximum"))
		return
	}
	start, end, err := s.prometheusMetricRange(r, s.prometheusClient.MaxQueryRange())
	if err != nil {
		writePrometheusError(w, prometheus.NewError(http.StatusBadRequest, "bad_data", "invalid metric time range"))
		return
	}
	params := url.Values{"start": {start.Format(time.RFC3339Nano)}, "end": {end.Format(time.RFC3339Nano)}, "match[]": metricSelectors("", r.URL.Query().Get("service_name"))}
	body, err := s.prometheusClient.MetricNames(r.Context(), params)
	if err != nil {
		writePrometheusError(w, err)
		return
	}
	var envelope struct {
		Data []string `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		writePrometheusError(w, prometheus.NewError(http.StatusBadGateway, "internal", "invalid Prometheus metric names"))
		return
	}
	if envelope.Data == nil {
		envelope.Data = []string{}
	}
	// Label discovery can include retained index entries outside sample
	// availability; do not imply complete data coverage from a name alone.
	setCoverage(w, aggregate.CoverageSampled)
	w.Header().Set("OtelContext-Metric-Source", "prometheus")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envelope.Data)
}
