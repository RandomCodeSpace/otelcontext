package mcp

import (
	"strings"
	"testing"
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
