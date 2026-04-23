//go:build integration

package ingest

import (
	"context"
	"net"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// noopTraceServer implements coltracepb.TraceServiceServer and accepts any Export.
type noopTraceServer struct {
	coltracepb.UnimplementedTraceServiceServer
}

func (*noopTraceServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// buildFatTraceRequest constructs an ExportTraceServiceRequest whose protobuf
// wire size exceeds targetBytes. Each span carries a large string attribute to
// bulk up the payload without needing an absurd number of spans.
func buildFatTraceRequest(t *testing.T, targetBytes int) *coltracepb.ExportTraceServiceRequest {
	t.Helper()
	padding := make([]byte, 4096)
	for i := range padding {
		padding[i] = 'x'
	}
	paddingStr := string(padding)

	var req coltracepb.ExportTraceServiceRequest
	req.ResourceSpans = []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "loadtest"}},
			}},
		},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: make([]*tracepb.Span, 0, targetBytes/4096+1),
		}},
	}}
	ss := req.ResourceSpans[0].ScopeSpans[0]
	size := 0
	for size < targetBytes {
		ss.Spans = append(ss.Spans, &tracepb.Span{
			TraceId:           make([]byte, 16),
			SpanId:            make([]byte, 8),
			Name:              "fat-span",
			StartTimeUnixNano: 1,
			EndTimeUnixNano:   2,
			Attributes: []*commonpb.KeyValue{{
				Key:   "padding",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: paddingStr}},
			}},
		})
		size += len(paddingStr)
	}
	return &req
}

// TestGRPC_AcceptsLargeOTLPBatch verifies that a gRPC server configured with
// MaxRecvMsgSize accepts a batch larger than the default 4 MiB limit.
//
// This test *proves the option takes effect*: without MaxRecvMsgSize the
// server would reject the 5 MiB payload; with it, the payload is accepted.
func TestGRPC_AcceptsLargeOTLPBatch(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer(
		grpc.MaxRecvMsgSize(16*1024*1024),
		grpc.MaxConcurrentStreams(1000),
	)
	coltracepb.RegisterTraceServiceServer(s, &noopTraceServer{})
	go s.Serve(lis)
	t.Cleanup(func() { s.Stop() })

	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(16*1024*1024)),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := coltracepb.NewTraceServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := buildFatTraceRequest(t, 5*1024*1024) // 5 MiB payload
	if _, err := client.Export(ctx, req); err != nil {
		t.Fatalf("Export rejected 5 MiB batch: %v", err)
	}
}
