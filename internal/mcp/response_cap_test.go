package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestTextResult_ResponseCap verifies the byte-cap defense for tool
// responses. Without it, get_trace / get_graph_snapshot / correlated_signals
// can OOM the process on adversarial input.
func TestTextResult_ResponseCap(t *testing.T) {
	t.Parallel()

	t.Run("UnderCapPassesThrough", func(t *testing.T) {
		text := strings.Repeat("x", 1024) // 1 KiB
		got := textResult(text)
		if got.IsError {
			t.Fatalf("expected success, got IsError=true: %+v", got)
		}
		if len(got.Content) != 1 || got.Content[0].Text != text {
			t.Fatalf("payload mangled: %+v", got)
		}
	})

	t.Run("AtCapPassesThrough", func(t *testing.T) {
		// Exactly at the cap is allowed. The error fires only on > cap.
		text := strings.Repeat("x", MaxToolResponseBytes)
		got := textResult(text)
		if got.IsError {
			t.Fatalf("expected at-cap to pass, got IsError=true")
		}
	})

	t.Run("OverCapErrors", func(t *testing.T) {
		text := strings.Repeat("x", MaxToolResponseBytes+1)
		got := textResult(text)
		if !got.IsError {
			t.Fatalf("expected over-cap to error, got success")
		}
		if len(got.Content) == 0 || !strings.Contains(got.Content[0].Text, "response too large") {
			t.Fatalf("expected 'response too large' marker in error message, got: %+v", got)
		}
		if !strings.Contains(got.Content[0].Text, "narrow time range") {
			t.Fatalf("expected actionable hint in error message, got: %+v", got)
		}
	})

	t.Run("EmptyTextOK", func(t *testing.T) {
		got := textResult("")
		if got.IsError {
			t.Fatalf("empty text should not error: %+v", got)
		}
	})
}

// TestResourceResult_ResponseCap verifies that resourceResult applies the same
// MaxToolResponseBytes cap as textResult — the trace_graph DB fallback can
// return an unbounded GetTrace payload without this guard.
func TestResourceResult_ResponseCap(t *testing.T) {
	t.Parallel()

	t.Run("UnderCapPassesThrough", func(t *testing.T) {
		text := strings.Repeat("x", 1024)
		got := resourceResult("OtelContext://test", "application/json", text)
		if got.IsError {
			t.Fatalf("expected success, got IsError=true: %+v", got)
		}
		if len(got.Content) != 1 || got.Content[0].Resource == nil {
			t.Fatalf("expected resource content item: %+v", got)
		}
		if got.Content[0].Resource.Text != text {
			t.Fatalf("payload mangled")
		}
	})

	t.Run("AtCapPassesThrough", func(t *testing.T) {
		text := strings.Repeat("x", MaxToolResponseBytes)
		got := resourceResult("OtelContext://test", "application/json", text)
		if got.IsError {
			t.Fatalf("expected at-cap to pass, got IsError=true")
		}
	})

	t.Run("OverCapErrors", func(t *testing.T) {
		text := strings.Repeat("x", MaxToolResponseBytes+1)
		got := resourceResult("OtelContext://test", "application/json", text)
		if !got.IsError {
			t.Fatalf("expected over-cap to error, got success")
		}
		if len(got.Content) == 0 || !strings.Contains(got.Content[0].Text, "response too large") {
			t.Fatalf("expected 'response too large' in error: %+v", got)
		}
		if !strings.Contains(got.Content[0].Text, "narrow time range") {
			t.Fatalf("expected actionable hint in error: %+v", got)
		}
	})
}

// TestCacheErrorSkip_GuardLogic verifies the guard logic that server.go uses
// to decide whether to call cache.Set. The guard is:
//
//	if !toolResult.IsError { s.cache.Set(...) }
//
// This test exercises that decision directly: an error result (IsError=true)
// must NOT be passed to Set; a successful result must be passed and then
// retrievable.
func TestCacheErrorSkip_GuardLogic(t *testing.T) {
	t.Parallel()

	c := newResultCache(5*time.Second, 64)

	// Simulate the server-side guard: only Set when !IsError.
	maybeCache := func(res ToolCallResult) {
		if !res.IsError {
			c.Set("default", "get_service_map", nil, res)
		}
	}

	// Error result: guard must prevent the Set.
	maybeCache(errorResult("GraphRAG not initialized"))
	_, hit := c.Get("default", "get_service_map", nil)
	if hit {
		t.Fatal("error result must not be stored in the cache")
	}

	// Successful result: guard must allow the Set.
	maybeCache(textResult(`{"ok":true}`))
	got, hit := c.Get("default", "get_service_map", nil)
	if !hit {
		t.Fatal("successful result must be stored in the cache")
	}
	if got.IsError {
		t.Fatalf("cached result should not be an error: %+v", got)
	}
}
