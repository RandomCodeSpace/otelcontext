package mcp

import (
	"encoding/json"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
)

// TestWithCoverageIsAdditive: coverage metadata rides as an EXTRA content item
// so the primary body — often a bare JSON array — keeps its shape.
func TestWithCoverageIsAdditive(t *testing.T) {
	s := New("default", nil, nil, nil)
	s.SetAggregateMode(true)

	body := `[{"id":1}]`
	res := s.withCoverage(textResult(body), aggregate.CoverageExemplar)
	if len(res.Content) != 2 {
		t.Fatalf("content items = %d, want 2 (body + coverage)", len(res.Content))
	}
	if res.Content[0].Text != body {
		t.Fatalf("primary body was rewritten: %q", res.Content[0].Text)
	}
	var meta coverageMeta
	if err := json.Unmarshal([]byte(res.Content[1].Text), &meta); err != nil {
		t.Fatalf("decode coverage metadata: %v (%s)", err, res.Content[1].Text)
	}
	if meta.Coverage != string(aggregate.CoverageExemplar) {
		t.Errorf("coverage = %q, want %q", meta.Coverage, aggregate.CoverageExemplar)
	}
	if meta.Note == "" {
		t.Error("exemplar coverage carries no note; a missing exemplar must not read as zero events")
	}
}

// TestWithCoverageSilentInLegacyMode: legacy and shadow responses stay
// byte-for-byte what they were.
func TestWithCoverageSilentInLegacyMode(t *testing.T) {
	s := New("default", nil, nil, nil)
	res := s.withCoverage(textResult(`[]`), aggregate.CoverageExemplar)
	if len(res.Content) != 1 {
		t.Fatalf("legacy result gained content items: %d", len(res.Content))
	}
}

// TestWithCoverageSkipsErrors: an error result must not be decorated as if it
// had returned data.
func TestWithCoverageSkipsErrors(t *testing.T) {
	s := New("default", nil, nil, nil)
	s.SetAggregateMode(true)
	res := s.withCoverage(errorResult("boom"), aggregate.CoverageExemplar)
	if len(res.Content) != 1 {
		t.Fatalf("error result gained content items: %d", len(res.Content))
	}
}

// TestEveryToolDeclaresCoverage keeps the map and the dispatch surface in step:
// a tool added without a coverage entry would silently claim nothing.
func TestEveryToolDeclaresCoverage(t *testing.T) {
	for _, def := range toolDefs {
		if _, ok := toolCoverage[def.Name]; !ok {
			t.Errorf("tool %q has no coverage declaration", def.Name)
		}
	}
	if len(toolCoverage) != len(toolDefs) {
		t.Errorf("coverage map has %d entries, tool surface has %d", len(toolCoverage), len(toolDefs))
	}
}
