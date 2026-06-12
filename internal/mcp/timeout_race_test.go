package mcp

import (
	"context"
	"testing"
	"time"
)

// Regression for a select race in runWithTimeout: the handler goroutine
// cancels the call context (deferred) after sending its result, so a
// fast handler could leave both select cases ready and the random pick
// reported a spurious -32001 timeout (seen as instant "exceeded 30s
// deadline" failures on loaded CI runners). A finished handler must
// always win, however the goroutines interleave.
func TestRunWithTimeout_FastHandlerNeverTimesOut(t *testing.T) {
	// Zero-value deps are safe: an unknown tool name takes toolHandler's
	// instant "unknown tool" path and the metrics defer nil-checks.
	s := &Server{callTimeout: 30 * time.Second}
	for i := 0; i < 2000; i++ {
		ctx, cancel := s.deriveCallCtx(context.Background())
		res, timedOut := s.runWithTimeout(ctx, cancel, "nonexistent_tool", nil, nil)
		if timedOut {
			t.Fatalf("iteration %d: spurious timeout from an instant handler", i)
		}
		if !res.IsError {
			t.Fatalf("iteration %d: expected unknown-tool error result", i)
		}
	}
}
