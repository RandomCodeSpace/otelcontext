//go:build loadtest

package main

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"math/rand"
)

type transportTraceServer struct {
	coltracepb.UnimplementedTraceServiceServer
}

func (transportTraceServer) Export(context.Context, *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func TestDirectTLSVerifiesCertificate(t *testing.T) {
	fixture := httptest.NewTLSServer(nil)
	certificate := fixture.TLS.Certificates[0]
	fixture.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})))
	coltracepb.RegisterTraceServiceServer(server, transportTraceServer{})
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	for _, tc := range []struct {
		name, ca, serverName string
		wantError            bool
	}{
		{"trusted", caFile, "example.com", false},
		{"untrusted", "", "example.com", true},
		{"wrong-host", caFile, "wrong.example", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds, err := transportCredentials(false, tc.ca, tc.serverName)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err = coltracepb.NewTraceServiceClient(conn).Export(ctx, &coltracepb.ExportTraceServiceRequest{})
			if (err != nil) != tc.wantError {
				t.Fatalf("Export error = %v, want error %v", err, tc.wantError)
			}
		})
	}
	if _, err := transportCredentials(true, caFile, ""); err == nil {
		t.Fatal("insecure mode accepted TLS CA")
	}
}

type partialTraceClient struct{ cancel context.CancelFunc }

func (c partialTraceClient) Export(context.Context, *coltracepb.ExportTraceServiceRequest, ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
	c.cancel()
	return &coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: 1, ErrorMessage: "capacity"}}, nil
}

func TestDirectPartialRejectionDoesNotCountAsDurableACK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := directEmitter{kind: kindSpans, rate: 1000, cfg: directConfig{interval: time.Millisecond, callTimout: time.Second},
		traceClient: partialTraceClient{cancel}, rng: rand.New(rand.NewSource(1)), res: newResource("test", "test-0")}
	currentPhase.Store(phaseSustained)
	var wg sync.WaitGroup
	wg.Add(1)
	e.run(ctx, &wg)
	wg.Wait()
	if e.out.pointsSent[phaseSustained] == 0 || e.out.reqErr[phaseSustained] == 0 {
		t.Fatal("no rejected request was measured")
	}
	if e.out.pointsAcked[phaseSustained] != 0 || e.out.reqOK[phaseSustained] != 0 {
		t.Fatalf("partial response counted as ACK: %+v", e.out)
	}
}

func TestMockHostMapping(t *testing.T) {
	for i := 0; i < 200; i++ {
		res := newResource(serviceName(i), "instance")
		addMockHost(res, i, 20)
		attrs := make(map[string]string)
		for _, attr := range res.Attributes {
			attrs[attr.Key] = attr.Value.GetStringValue()
		}
		if attrs["host.id"] == "" || attrs["host.name"] == "" || attrs["host.id"] == attrs["host.name"] {
			t.Fatalf("missing or indistinguishable host identity: %v", attrs)
		}
		if attrs["service.name"] != serviceName(i) {
			t.Fatal("host mapping overwrote service identity")
		}
	}
}

func TestDirectMetricsHaveOrderedSamplesAndIndependentResetStart(t *testing.T) {
	start := uint64(time.Now().Add(-time.Hour).UnixNano())
	e := directEmitter{cfg: directConfig{interval: time.Second}, rng: rand.New(rand.NewSource(1)),
		res: newResource("test", "test-0"), startNano: start, resetAt: time.Now().Add(-6 * time.Minute)}
	last := make(map[string]uint64)
	for batch := 0; batch < 2; batch++ {
		req, _ := e.buildMetrics(7)
		for _, metric := range req.ResourceMetrics[0].ScopeMetrics[0].Metrics {
			var points []*metricspb.NumberDataPoint
			if sum := metric.GetSum(); sum != nil {
				points = sum.DataPoints
			} else {
				points = metric.GetGauge().DataPoints
			}
			for _, point := range points {
				stamp := point.TimeUnixNano / uint64(time.Millisecond)
				if stamp <= last[metric.Name] {
					t.Fatalf("%s has duplicate or reversed millisecond timestamps", metric.Name)
				}
				last[metric.Name] = stamp
				if metric.Name == "http.server.request.count" && point.StartTimeUnixNano != start {
					t.Fatal("resetting one counter changed another counter's start")
				}
				if metric.Name == "custom.reset.counter" && point.StartTimeUnixNano <= start {
					t.Fatal("counter reset did not move its start")
				}
				if point.StartTimeUnixNano > point.TimeUnixNano {
					t.Fatal("counter starts after its sample")
				}
			}
		}
	}
}
