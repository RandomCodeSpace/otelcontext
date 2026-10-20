// Command rawseed creates bounded correlated raw exemplars in a NEW fixture
// directory using the application's versioned migrations and repository APIs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

type options struct {
	out             string
	services, hosts int
	prefix          string
	end             time.Time
}

type checkpoint struct {
	Kind           string    `json:"kind"`
	Timestamp      time.Time `json:"timestamp"`
	ExampleTraceID string    `json:"example_trace_id"`
}

type manifest struct {
	Simulation     bool         `json:"simulation"`
	Database       string       `json:"database"`
	End            time.Time    `json:"end_exclusive"`
	Cutoff         time.Time    `json:"retention_cutoff_at_seed"`
	Services       int          `json:"services"`
	Hosts          int          `json:"hosts"`
	ServicePrefix  string       `json:"service_prefix"`
	Traces         int          `json:"traces"`
	Spans          int          `json:"spans"`
	Logs           int          `json:"logs"`
	RetainedTraces int          `json:"retained_traces_at_seed"`
	ExpiredTraces  int          `json:"expired_traces_at_seed"`
	Checkpoints    []checkpoint `json:"checkpoints"`
}

func main() {
	out := flag.String("out", "", "NEW isolated fixture directory; never an existing database")
	services := flag.Int("services", 200, "services, 1..200")
	hosts := flag.Int("hosts", 20, "mock hosts, 1..20")
	prefix := flag.String("service-prefix", "loadsim-svc-", "prefix followed by zero-padded three-digit index")
	endRaw := flag.String("end-time", "", "exclusive RFC3339 end; default is current UTC minute")
	flag.Parse()
	end := time.Now().UTC().Truncate(time.Minute)
	if *endRaw != "" {
		var err error
		end, err = time.Parse(time.RFC3339, *endRaw)
		if err != nil {
			fail(err)
		}
	}
	m, err := seed(options{*out, *services, *hosts, *prefix, end.UTC()})
	if err != nil {
		fail(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(m); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "raw fixture failed:", err)
	os.Exit(1)
}

func traceID(end time.Time, service, point int) string {
	return fmt.Sprintf("%016x%08x%08x", end.Unix(), service, point+1)
}

func checkpoints(end time.Time) []checkpoint {
	points := []checkpoint{{Kind: "expired", Timestamp: end.Add(-8 * 24 * time.Hour)}}
	for day := 7; day >= 0; day-- {
		ts := end.Add(-time.Duration(day) * 24 * time.Hour)
		if day == 7 {
			ts = ts.Add(time.Minute)
		}
		if day == 0 {
			ts = ts.Add(-time.Minute)
		}
		points = append(points, checkpoint{Kind: "history", Timestamp: ts})
	}
	for i := range points {
		points[i].ExampleTraceID = traceID(end, 0, i)
	}
	return points
}

func seed(o options) (m manifest, err error) {
	if o.out == "" || o.services < 1 || o.services > 200 || o.hosts < 1 || o.hosts > 20 || o.prefix == "" {
		return m, fmt.Errorf("out required; services 1..200, hosts 1..20 and nonempty service-prefix required")
	}
	if o.end.After(time.Now()) || o.end.Unix() < 8*86400 || o.end.Nanosecond() != 0 {
		return m, fmt.Errorf("end-time must be whole seconds in the past, at least eight days after Unix epoch")
	}
	// A newly created directory guarantees that raw.db is not an existing live
	// database. Do not add an overwrite/resume mode to this acceptance fixture.
	if err := os.Mkdir(o.out, 0o750); err != nil {
		return m, fmt.Errorf("new fixture directory: %w", err)
	}
	dbPath := filepath.Join(o.out, "raw.db")
	db, err := storage.NewDatabase("sqlite", dbPath)
	if err != nil {
		return m, err
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	defer func() {
		closeErr := repo.Close()
		if err == nil {
			err = closeErr
		}
	}()
	ctx := context.Background()
	if _, err = migrate.Up(ctx, db, "sqlite"); err != nil {
		return m, err
	}
	if _, err = migrate.RequireExact(ctx, db, "sqlite"); err != nil {
		return m, err
	}
	m = manifest{
		Simulation: true, Database: dbPath, End: o.end, Cutoff: o.end.Add(-7 * 24 * time.Hour),
		Services: o.services, Hosts: o.hosts, ServicePrefix: o.prefix, Checkpoints: checkpoints(o.end),
		RetainedTraces: o.services * 8, ExpiredTraces: o.services,
	}
	for point, cp := range m.Checkpoints {
		traces := make([]storage.Trace, 0, o.services)
		spans := make([]storage.Span, 0, o.services*2)
		logs := make([]storage.Log, 0, o.services*2)
		for service := 0; service < o.services; service++ {
			trace, ss, ls := exemplar(o, point, cp, service)
			traces = append(traces, trace)
			spans = append(spans, ss...)
			logs = append(logs, ls...)
		}
		if err = repo.BatchCreateAll(traces, spans, logs); err != nil {
			return m, err
		}
		m.Traces += len(traces)
		m.Spans += len(spans)
		m.Logs += len(logs)
	}
	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	err = os.WriteFile(filepath.Join(o.out, "manifest.json"), append(encoded, '\n'), 0o640)
	return m, err
}

func exemplar(o options, point int, cp checkpoint, service int) (storage.Trace, []storage.Span, []storage.Log) {
	name := fmt.Sprintf("%s%03d", o.prefix, service)
	host := service % o.hosts
	attrs, _ := json.Marshal(map[string]any{
		"service.name": name, "service.instance.id": fmt.Sprintf("history-svc-%03d", service),
		"host.id": fmt.Sprintf("mock-host-id-%03d", host), "host.name": fmt.Sprintf("mock-node-%03d", host),
		"host.arch": "amd64", "os.type": "linux", "host.cpu.model": "Mock 8-core server",
		"host.cpu.logical.count": 8, "host.memory.total": int64(32) << 30,
		"k8s.node.name": fmt.Sprintf("mock-node-%03d", host), "k8s.namespace.name": "loadtest",
		"fixture.kind": cp.Kind, "fixture.checkpoint": point, "fixture.simulation": true,
	})
	id := traceID(o.end, service, point)
	truncated, count := false, 2
	status := "STATUS_CODE_OK"
	severity := "INFO"
	if service%10 == 0 {
		status, severity = storage.StatusCodeError, "ERROR"
	}
	trace := storage.Trace{
		TenantID: storage.DefaultTenantID, TraceID: id, ServiceName: name,
		Duration: 100000, Status: status, Timestamp: cp.Timestamp,
		Truncated: &truncated, RetainedSpanCount: &count, ObservedSpanCount: &count,
	}
	root := storage.Span{
		TenantID: storage.DefaultTenantID, TraceID: id, SpanID: "0000000000000001",
		ServiceName: name, OperationName: "GET /fixture/retained", StartTime: cp.Timestamp,
		EndTime: cp.Timestamp.Add(100 * time.Millisecond), Duration: 100000,
		Status: status, AttributesJSON: storage.CompressedText(attrs),
	}
	child := root
	child.SpanID, child.ParentSpanID, child.OperationName = "0000000000000002", root.SpanID, "fixture.database.lookup"
	child.StartTime, child.EndTime, child.Duration = cp.Timestamp.Add(10*time.Millisecond), cp.Timestamp.Add(90*time.Millisecond), 80000
	spans := []storage.Span{root, child}
	logs := make([]storage.Log, 0, 2)
	for i, span := range spans {
		logs = append(logs, storage.Log{
			TenantID: storage.DefaultTenantID, TraceID: id, SpanID: span.SpanID,
			ServiceName: name, Severity: severity,
			Body:           fmt.Sprintf("production fixture %s checkpoint=%d service=%s span=%d", cp.Kind, point, name, i),
			Timestamp:      cp.Timestamp.Add(time.Duration(25+i*25) * time.Millisecond),
			AttributesJSON: storage.CompressedText(attrs),
		})
	}
	return trace, spans, logs
}
