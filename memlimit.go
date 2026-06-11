package main

import (
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/RandomCodeSpace/otelcontext/internal/membudget"
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
// it at startup and we leave it untouched. Otherwise the budget is detected via
// internal/membudget (cgroup v2 → cgroup v1 → /proc/meminfo) and the limit is
// set to pct percent of that budget. Any detection failure leaves the runtime
// default (no limit) in place — never worse than today.
func applyMemoryLimit(pct int) {
	if _, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		slog.Info("🧠 GOMEMLIMIT set by operator; honoring it",
			"soft_limit_bytes", debug.SetMemoryLimit(-1))
		return
	}
	budget, source := membudget.Detect()
	if budget <= 0 {
		slog.Warn("🧠 could not detect memory budget; GOMEMLIMIT left unset (GC target unbounded)")
		return
	}
	limit := budget / 100 * int64(pct)
	debug.SetMemoryLimit(limit)
	slog.Info("🧠 soft memory limit applied (GOMEMLIMIT)",
		"limit_mib", limit/(1<<20), "budget_mib", budget/(1<<20), "pct", pct, "source", source)
}
