//go:build browser && !windows

package browser_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type applicationMapEdge struct {
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	CallCount    int64   `json:"call_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
}

type applicationMapFixture struct {
	Services             int                  `json:"services"`
	ConnectedServices    int                  `json:"connected_services"`
	DisconnectedServices int                  `json:"disconnected_services"`
	Dependencies         int                  `json:"dependencies"`
	AcknowledgedSpans    int                  `json:"acknowledged_spans"`
	AttemptedSpans       int                  `json:"attempted_spans"`
	ExportErrors         []string             `json:"export_errors"`
	ServiceNames         []string             `json:"service_names"`
	Edges                []applicationMapEdge `json:"edges"`
}

type applicationMapGraph struct {
	Nodes []struct {
		ID string `json:"id"`
	} `json:"nodes"`
	Edges []applicationMapEdge `json:"edges"`
}

// exercise the same real OTLP emitter that supplies the reviewable preview.
func injectApplicationMap(t *testing.T, baseURL, shape string) applicationMapFixture {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("application-map fixture requires python3: ", err)
	}
	reportPath := filepath.Join(t.TempDir(), "fixture.json")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, filepath.Join("..", "..", "scripts", "simulate_application_map.py"),
		"--url", baseURL, "--shape", shape, "--services", "150", "--report", reportPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("export %s application map: %v\n%s", shape, err, output)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture applicationMapFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Services != 150 || fixture.AttemptedSpans == 0 || fixture.AcknowledgedSpans != fixture.AttemptedSpans || len(fixture.ExportErrors) != 0 {
		t.Fatalf("incomplete OTLP fixture: %s", data)
	}
	t.Logf("real OTLP fixture: %d services, %d dependencies, %d acknowledged spans", fixture.Services, fixture.Dependencies, fixture.AcknowledgedSpans)
	return fixture
}

func applicationMapGraphMatches(graph applicationMapGraph, fixture applicationMapFixture) bool {
	if len(graph.Nodes) != fixture.Services || len(graph.Edges) != fixture.Dependencies {
		return false
	}
	names := make([]string, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		names = append(names, node.ID)
	}
	wantNames := append([]string(nil), fixture.ServiceNames...)
	sort.Strings(names)
	sort.Strings(wantNames)
	if fmt.Sprint(names) != fmt.Sprint(wantNames) {
		return false
	}
	edges := make(map[string]bool, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges[edge.Source+">"+edge.Target] = true
	}
	for _, edge := range fixture.Edges {
		if !edges[edge.Source+">"+edge.Target] {
			return false
		}
	}
	return true
}

func applicationMapMismatch(graph applicationMapGraph, fixture applicationMapFixture) string {
	missingNames := make(map[string]bool, len(fixture.ServiceNames))
	missingEdges := make(map[string]bool, len(fixture.Edges))
	for _, name := range fixture.ServiceNames {
		missingNames[name] = true
	}
	for _, edge := range fixture.Edges {
		missingEdges[edge.Source+">"+edge.Target] = true
	}
	for _, node := range graph.Nodes {
		delete(missingNames, node.ID)
	}
	zeroCalls := 0
	for _, edge := range graph.Edges {
		delete(missingEdges, edge.Source+">"+edge.Target)
		if edge.CallCount == 0 {
			zeroCalls++
		}
	}
	summarize := func(missing map[string]bool) string {
		keys := make([]string, 0, len(missing))
		for key := range missing {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 10 {
			return strings.Join(keys[:10], ", ") + fmt.Sprintf(" (+%d more)", len(keys)-10)
		}
		return strings.Join(keys, ", ")
	}
	return fmt.Sprintf("got %d/%d services, %d/%d dependencies (%d with zero calls); missing services [%s]; missing edges [%s]",
		len(graph.Nodes), fixture.Services, len(graph.Edges), fixture.Dependencies, zeroCalls, summarize(missingNames), summarize(missingEdges))
}

func waitApplicationMap(t *testing.T, baseURL string, fixture applicationMapFixture, checks ...func(applicationMapGraph) bool) applicationMapGraph {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/system/graph", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			_ = response.Body.Close()
			var graph applicationMapGraph
			decodeErr := json.Unmarshal(body, &graph)
			if readErr == nil && response.StatusCode == http.StatusOK && decodeErr == nil {
				matched := applicationMapGraphMatches(graph, fixture)
				for _, check := range checks {
					matched = matched && check(graph)
				}
				if matched {
					return graph
				}
				last = applicationMapMismatch(graph, fixture)
				if len(checks) != 0 {
					last += "; waiting for metric update"
				}
			} else {
				last = fmt.Sprintf("graph response status=%d read=%v decode=%v", response.StatusCode, readErr, decodeErr)
			}
		} else if last == "" {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("expected %d services and %d directed dependencies: %s", fixture.Services, fixture.Dependencies, last)
		case <-ticker.C:
		}
	}
}
