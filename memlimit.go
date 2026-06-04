package main

import (
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// applyMemoryLimit sets a soft heap ceiling (GOMEMLIMIT) so the garbage collector
// paces against a bound instead of letting the next-GC target run away on a large
// heap. Without it, a deployment under sustained ingest lets RSS climb to the
// (uncapped) GC target and never returns it to the OS — observed in the SQLite
// soak as ~1.8 GB RSS held flat after load stopped. A soft limit makes the GC run
// more frequently as the heap approaches the ceiling, keeping the working set and
// RSS bounded without the hard-OOM behavior of a rlimit.
//
// If the operator already set the GOMEMLIMIT env var, the Go runtime has applied
// it at startup and we leave it untouched. Otherwise the budget is detected from
// (in order) cgroup v2, cgroup v1, then /proc/meminfo, and the limit is set to
// pct percent of that budget. Any detection failure leaves the runtime default
// (no limit) in place — never worse than today.
func applyMemoryLimit(pct int) {
	if _, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		slog.Info("🧠 GOMEMLIMIT set by operator; honoring it",
			"soft_limit_bytes", debug.SetMemoryLimit(-1))
		return
	}
	budget, source := detectMemoryBudget()
	if budget <= 0 {
		slog.Warn("🧠 could not detect memory budget; GOMEMLIMIT left unset (GC target unbounded)")
		return
	}
	limit := budget / 100 * int64(pct)
	debug.SetMemoryLimit(limit)
	slog.Info("🧠 soft memory limit applied (GOMEMLIMIT)",
		"limit_mib", limit/(1<<20), "budget_mib", budget/(1<<20), "pct", pct, "source", source)
}

// detectMemoryBudget returns the applicable memory budget in bytes and its source,
// or (0, "") if nothing usable is found. cgroup limits take precedence over host
// RAM so containerized deployments pace against their actual quota.
func detectMemoryBudget() (int64, string) {
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
