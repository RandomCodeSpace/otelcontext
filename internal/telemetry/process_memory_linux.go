//go:build linux

package telemetry

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processRSSReader feeds otelcontext_process_resident_memory_bytes on Linux.
var processRSSReader = readStatmRSS

// readStatmRSS returns the resident set in bytes: the second field of
// /proc/self/statm (resident pages) times the page size.
func readStatmRSS() (int64, error) {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0, fmt.Errorf("/proc/self/statm: %d fields", len(fields))
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/self/statm resident %q: %w", fields[1], err)
	}
	return pages * int64(os.Getpagesize()), nil
}
