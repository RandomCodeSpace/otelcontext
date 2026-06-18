package graphrag

import (
	"testing"
	"time"
)

// FuzzDrainMatch feeds arbitrary log lines into the Drain template miner.
// Match() is the ingestion hot path that every untrusted log body flows
// through (LogsServer.Export -> LogCallback -> Drain.Match), so it must
// never panic on malformed input and must satisfy two invariants:
//
//   - empty/whitespace-only input returns nil (no template);
//   - any non-nil result is a well-formed template (non-empty token slice,
//     stable ID matching its tokens).
//
// We also assert determinism: Preprocess + tokenize are pure functions, so
// masking the same line twice must yield the identical token sequence.
func FuzzDrainMatch(f *testing.F) {
	seeds := []string{
		"",
		"   \t\n",
		"connected to 10.0.0.1:8080 successfully",
		"trace 550e8400-e29b-41d4-a716-446655440000 done",
		"event at 2025-01-02T03:04:05Z completed",
		"notify alice@example.com now",
		"pointer 0xdeadbeef freed -1 times",
		"Database connection failed: timeout after 30s",
		"\x00\x01\x02 binary garbage \xff\xfe",
		"日本語 のログ メッセージ 42",
		"a b c d e f g h i j k l m n o p", // long token run, exceeds depth
	}
	for _, s := range seeds {
		f.Add(s)
	}

	ts := time.Unix(0, 0)
	f.Fuzz(func(t *testing.T, line string) {
		// Preprocessing must be deterministic: same input -> same masked tokens.
		m1 := tokenize(Preprocess(line))
		m2 := tokenize(Preprocess(line))
		if len(m1) != len(m2) {
			t.Fatalf("Preprocess non-deterministic token count: %d vs %d for %q", len(m1), len(m2), line)
		}
		for i := range m1 {
			if m1[i] != m2[i] {
				t.Fatalf("Preprocess non-deterministic token %d: %q vs %q for input %q", i, m1[i], m2[i], line)
			}
		}

		d := NewDrain()
		tpl := d.Match(line, ts)

		if len(m1) == 0 {
			if tpl != nil {
				t.Fatalf("Match(%q) = non-nil template for empty/whitespace tokenization, want nil", line)
			}
			return
		}

		if tpl == nil {
			t.Fatalf("Match(%q) = nil for non-empty tokenization %v", line, m1)
		}
		if len(tpl.Tokens) == 0 {
			t.Fatalf("Match(%q) produced a template with no tokens", line)
		}
		// ID must be the stable hash of the (possibly generalized) tokens.
		if got := templateID(tpl.Tokens); got != tpl.ID {
			t.Fatalf("template ID %d does not match templateID(tokens)=%d for %q", tpl.ID, got, line)
		}
	})
}
