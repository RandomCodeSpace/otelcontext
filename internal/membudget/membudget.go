// Package membudget detects the memory budget available to the process —
// (in order) cgroup v2, cgroup v1, then host RAM from /proc/meminfo — so
// consumers can scale their allocations against the actual quota instead of
// hardcoding sizes. Used by the GOMEMLIMIT bootstrap in package main and the
// SQLite cache/mmap sizing in internal/storage.
package membudget

import (
	"os"
	"strconv"
	"strings"
)

// Detect returns the applicable memory budget in bytes and its source,
// or (0, "") if nothing usable is found. cgroup limits take precedence over
// host RAM so containerized deployments pace against their actual quota.
func Detect() (int64, string) {
	if v, ok := readCgroupBytes("/sys/fs/cgroup/memory.max"); ok { // cgroup v2
		return v, "cgroup.v2"
	}
	if v, ok := readCgroupBytes("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok { // cgroup v1
		return v, "cgroup.v1"
	}
	if v, ok := readMemTotal("/proc/meminfo"); ok {
		return v, "meminfo.MemTotal"
	}
	return 0, ""
}

// readCgroupBytes reads a cgroup memory-limit file. Returns (bytes, true) only for
// a concrete limit; "max", empty, non-numeric, or a near-max "unlimited" sentinel
// (>= 1 PiB, used by cgroup v1) returns (0, false) so the caller falls through.
func readCgroupBytes(path string) (int64, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed cgroup sysfs path, not user input
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || v >= (1<<50) {
		return 0, false
	}
	return v, true
}

// readMemTotal parses MemTotal (in kB) from a /proc/meminfo-formatted file and
// returns it in bytes.
func readMemTotal(path string) (int64, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed /proc/meminfo path, not user input
	if err != nil {
		return 0, false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil && kb > 0 {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}
