package config

import (
	"log/slog"
	"net"
	"strings"
)

// SelfServiceName is the OTel service.name attribute the binary attaches to
// its own self-instrumentation spans. Mirrors the literal in
// main.initTracerProvider — keep the two in sync.
const SelfServiceName = "otelcontext"

// GuardSelfInstrumentation prevents an amplification loop when
// OTEL_EXPORTER_OTLP_ENDPOINT points at the binary's own gRPC port. Without
// this, every span the OTel SDK emits would re-enter Export, generate more
// spans (one per Export call), and re-enter again — unbounded fan-out.
//
// Strategy: when the configured endpoint resolves to a loopback address, the
// own service name is auto-added to IngestExcludedServices so the ingest
// filter drops self-emitted batches. Operators can still override by setting
// the variable explicitly — the guard only ADDS, never removes.
//
// No-op when self-instrumentation is disabled (empty endpoint) or the
// endpoint is non-loopback (a separate collector, the operator's responsibility).
func (c *Config) GuardSelfInstrumentation() {
	if c == nil || c.OTelExporterEndpoint == "" {
		return
	}
	host := hostFromEndpoint(c.OTelExporterEndpoint)
	if !isLoopbackHost(host) {
		return
	}
	if hasService(c.IngestExcludedServices, SelfServiceName) {
		return
	}
	if c.IngestExcludedServices == "" {
		c.IngestExcludedServices = SelfServiceName
	} else {
		c.IngestExcludedServices = SelfServiceName + "," + c.IngestExcludedServices
	}
	slog.Warn("self-instrumentation guard: auto-excluded own service from ingest to break feedback loop",
		"endpoint", c.OTelExporterEndpoint,
		"self_service", SelfServiceName,
		"ingest_excluded_services", c.IngestExcludedServices,
	)
}

// hostFromEndpoint extracts the host portion of an OTLP endpoint string.
// Tolerates "host", "host:port", and "scheme://host:port" forms — the OTel
// SDK accepts all three. Returns the lowercase host or "" on parse failure.
func hostFromEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	// Strip scheme if present (e.g. "http://localhost:4317").
	if i := strings.Index(endpoint, "://"); i >= 0 {
		endpoint = endpoint[i+3:]
	}
	// Drop path component if present.
	if i := strings.Index(endpoint, "/"); i >= 0 {
		endpoint = endpoint[:i]
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		// No port — treat the whole thing as host.
		host = endpoint
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// isLoopbackHost reports whether host is one of the well-known loopback
// names or a literal loopback IP. The empty host is treated as loopback
// because OTel SDKs fall back to "localhost" when the endpoint is bare.
func isLoopbackHost(host string) bool {
	switch host {
	case "", "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// hasService reports whether the comma-separated list contains the given
// service name. Whitespace around list entries is tolerated so the same
// helper can validate operator-supplied lists.
func hasService(list, service string) bool {
	if list == "" || service == "" {
		return false
	}
	for _, s := range strings.Split(list, ",") {
		if strings.TrimSpace(s) == service {
			return true
		}
	}
	return false
}
