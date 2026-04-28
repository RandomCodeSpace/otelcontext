package storage

import (
	"strings"
	"testing"
)

func TestSanitizeTenantID(t *testing.T) {
	t.Parallel()

	t.Run("AcceptsValid", func(t *testing.T) {
		cases := []string{
			"acme",
			"team-alpha",
			"customer_42",
			"a.b.c",
			"550e8400-e29b-41d4-a716-446655440000", // UUID
			"münchen",                               // non-ASCII letters allowed
			strings.Repeat("a", MaxTenantIDLength),  // exactly at the cap
		}
		for _, in := range cases {
			if got := SanitizeTenantID(in); got != in {
				t.Errorf("SanitizeTenantID(%q) = %q, want %q (unchanged)", in, got, in)
			}
		}
	})

	t.Run("Trims", func(t *testing.T) {
		if got := SanitizeTenantID("  acme  "); got != "acme" {
			t.Errorf("got %q, want %q", got, "acme")
		}
	})

	t.Run("RejectsEmpty", func(t *testing.T) {
		for _, in := range []string{"", "   ", "\t\t"} {
			if got := SanitizeTenantID(in); got != "" {
				t.Errorf("SanitizeTenantID(%q) = %q, want empty", in, got)
			}
		}
	})

	t.Run("RejectsControlChars", func(t *testing.T) {
		// Newline injection — would corrupt slog/loki structured output if accepted.
		// Carriage return — same. NUL — silently truncates strings on some toolchains.
		// Tab is also a control character per unicode.IsControl.
		cases := []string{
			"foo\nbar",
			"foo\rbar",
			"foo\x00bar",
			"foo\x1bbar", // ESC
			"foo\tbar",
		}
		for _, in := range cases {
			if got := SanitizeTenantID(in); got != "" {
				t.Errorf("SanitizeTenantID(%q) = %q, want empty (control char rejected)", in, got)
			}
		}
	})

	t.Run("RejectsOverLength", func(t *testing.T) {
		long := strings.Repeat("x", MaxTenantIDLength+1)
		if got := SanitizeTenantID(long); got != "" {
			t.Errorf("SanitizeTenantID(over-length) = %q, want empty", got)
		}
	})

	t.Run("TrimThenLengthCheck", func(t *testing.T) {
		// Whitespace doesn't bypass the length cap — the trimmed length is what matters.
		// 130 chars total, 128 after trim → still rejected because > MaxTenantIDLength is 128.
		// The trim happens FIRST and then we measure len(s).
		// "x"*129 + leading space → trim yields "x"*129, > 128 → rejected.
		s := " " + strings.Repeat("x", MaxTenantIDLength+1)
		if got := SanitizeTenantID(s); got != "" {
			t.Errorf("SanitizeTenantID(over-length-after-trim) = %q, want empty", got)
		}
	})
}
