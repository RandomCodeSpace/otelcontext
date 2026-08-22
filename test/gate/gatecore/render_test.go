package gatecore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderMarkdownFromFixture(t *testing.T) {
	r := passingResult()
	r.Finalize()
	md := RenderMarkdown(r, "2026-08-22-aggregate-7day-gate.json")

	for _, want := range []string{
		"# Aggregate seven-day gate",
		"## Verdict: PASS",
		"## Thresholds versus actuals",
		"## Phases",
		"## Load phases",
		"## Recovery — kill -9 on a surviving volume",
		"### Crash-interval bound",
		"## Memory",
		"## Disk — every partition",
		"## Main-tier projection",
		"## Query completeness",
		"### Aggregate-backed MCP tools, named explicitly",
		"## Durability claim demonstrated",
		"## Metric gaps found by this gate",
		"## Commands invoked",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered report is missing section %q", want)
		}
	}

	// The confinement mode, the effective limits and the durability claim are
	// contract requirements, not decoration.
	for _, want := range []string{
		"cgroup-scope", "200000 100000", "4.00 GiB",
		"Crash-durable on a surviving volume",
		"AGGREGATE_SYNCHRONOUS=NORMAL",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered report is missing required fact %q", want)
		}
	}

	// Every MCP tool the config names must appear by name.
	for _, tool := range AggregateMCPTools() {
		if !strings.Contains(md, tool.Name) {
			t.Errorf("MCP tool %q is not named in the report", tool.Name)
		}
	}

	// The projection must be labelled as one and show its sample count.
	if !strings.Contains(md, "PROJECTION — not a measurement") {
		t.Error("the projection is not labelled as a projection")
	}
	if !strings.Contains(md, "gated upper estimate") {
		t.Error("the report does not identify which projected number is gated")
	}
}

func TestRenderMarkdownShowsFailures(t *testing.T) {
	r := passingResult()
	r.Load.Sustained.P99Ms = 900
	r.Finalize()
	md := RenderMarkdown(r, "x.json")
	if !strings.Contains(md, "## Verdict: FAIL") {
		t.Error("a failing run must render a FAIL verdict")
	}
	if !strings.Contains(md, "sustained.ack_p99") {
		t.Error("the failing assertion must be named in the failure list")
	}
}

func TestRenderedNumbersComeFromTheSameStructAsTheJSON(t *testing.T) {
	// The whole point of rendering from the result struct: change one number
	// and both artefacts move together.
	r := passingResult()
	r.Memory.PeakBytes = 3*GiB + 512*MiB
	r.Finalize()

	md := RenderMarkdown(r, "x.json")
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var round Result
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if round.Memory.PeakBytes != r.Memory.PeakBytes {
		t.Fatalf("JSON round trip changed the peak: %d vs %d", round.Memory.PeakBytes, r.Memory.PeakBytes)
	}
	if !strings.Contains(md, HumanBytes(r.Memory.PeakBytes)) {
		t.Errorf("the Markdown does not carry the peak the JSON does (%s)", HumanBytes(r.Memory.PeakBytes))
	}
}

func TestWriteReportsProducesBothArtefacts(t *testing.T) {
	dir := t.TempDir()
	r := passingResult()
	r.Finalize()
	day := time.Date(2026, 8, 22, 13, 45, 0, 0, time.UTC)

	jsonPath, mdPath, err := WriteReports(dir, day, r)
	if err != nil {
		t.Fatalf("WriteReports: %v", err)
	}
	if filepath.Base(jsonPath) != "2026-08-22-aggregate-7day-gate.json" {
		t.Errorf("json name = %s", filepath.Base(jsonPath))
	}
	if filepath.Base(mdPath) != "2026-08-22-aggregate-7day-gate.md" {
		t.Errorf("markdown name = %s", filepath.Base(mdPath))
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the written JSON does not parse: %v", err)
	}
	if back.Schema != Schema || !back.Passed || len(back.Assertions) != len(r.Assertions) {
		t.Errorf("round-tripped result lost information: schema=%q passed=%t assertions=%d",
			back.Schema, back.Passed, len(back.Assertions))
	}
}

func TestMdEscapeKeepsTablesIntact(t *testing.T) {
	if got := mdEscape("a|b\nc"); got != "a\\|b c" {
		t.Errorf("mdEscape = %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:           "0 B",
		1023:        "1023 B",
		1024:        "1.00 KiB",
		4 * GiB:     "4.00 GiB",
		GiB + GiB/2: "1.50 GiB",
		-2 * MiB:    "-2.00 MiB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
