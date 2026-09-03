//go:build !linux

package telemetry

// processRSSReader is nil off Linux: /proc/self/statm does not exist, so
// otelcontext_process_resident_memory_bytes is omitted from /metrics rather
// than published as zero (#283).
var processRSSReader func() (int64, error)
