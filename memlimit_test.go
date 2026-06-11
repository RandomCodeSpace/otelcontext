package main

import (
	"os"
	"runtime/debug"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/membudget"
)

// Detection internals (cgroup v2/v1, /proc/meminfo parsing) are tested in
// internal/membudget — these tests cover only the thin wrapper behavior.

func TestApplyMemoryLimit_HonorsOperatorGOMEMLIMIT(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	t.Setenv("GOMEMLIMIT", "1GiB")
	applyMemoryLimit(75)
	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Fatalf("applyMemoryLimit must not touch the limit when GOMEMLIMIT is set; got %d want %d", got, prev)
	}
}

func TestApplyMemoryLimit_SetsBudgetFraction(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	// Force the detection path: clear GOMEMLIMIT for this test only.
	// t.Setenv registers the restore; Unsetenv makes LookupEnv miss.
	t.Setenv("GOMEMLIMIT", "")
	if err := os.Unsetenv("GOMEMLIMIT"); err != nil {
		t.Fatalf("unset GOMEMLIMIT: %v", err)
	}

	budget, _ := membudget.Detect()
	if budget <= 0 {
		t.Skip("no detectable memory budget on this host")
	}
	applyMemoryLimit(75)
	want := budget / 100 * 75
	if got := debug.SetMemoryLimit(-1); got != want {
		t.Fatalf("applyMemoryLimit(75) set %d, want %d (budget %d)", got, want, budget)
	}
}
