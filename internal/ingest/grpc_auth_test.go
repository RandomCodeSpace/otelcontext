package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func grpcAuthenticator(t *testing.T, operatorKey string, entries map[string]string, trustExternal bool) *authn.Authenticator {
	t.Helper()
	var store *authn.KeyStore
	if len(entries) > 0 {
		var err error
		store, err = authn.NewKeyStoreFromMap(entries)
		if err != nil {
			t.Fatalf("NewKeyStoreFromMap: %v", err)
		}
	}
	return authn.NewAuthenticator(operatorKey, store, trustExternal)
}

// conflicts installs the tenant-conflict hook for one test.
func conflicts(t *testing.T) *[][2]string {
	t.Helper()
	prev := authn.ConflictHook
	t.Cleanup(func() { authn.ConflictHook = prev })
	var got [][2]string
	authn.ConflictHook = func(surface, reason string) { got = append(got, [2]string{surface, reason}) }
	return &got
}

func mdCtx(pairs ...string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

// A tenant key binds absolutely: a contradicting x-tenant-id is ignored and
// counted, and the batch the pipeline receives carries the key's tenant —
// which is the tenant the rows are written under.
func TestGRPCAuth_TenantKeyOverridesMetadataOnExport(t *testing.T) {
	got := conflicts(t)
	unary, _ := NewGRPCAuthInterceptors(GRPCAuthOptions{
		Auth: grpcAuthenticator(t, "operator-key", map[string]string{"acme-key": "acme"}, false),
	})
	if unary == nil {
		t.Fatal("interceptors must be installed when authentication is configured")
	}

	h := newAdmissionHarness(t, 8, 0, false)
	ctx := mdCtx("authorization", "Bearer acme-key", tenantHeader, "victim")

	_, err := unary(ctx, nil, &grpc.UnaryServerInfo{}, func(authCtx context.Context, _ any) (any, error) {
		return h.traces.Export(authCtx, buildTracesRequest("svc-a", 2))
	})
	if err != nil {
		t.Fatalf("authenticated export: %v", err)
	}

	select {
	case b := <-h.pipeline.queue:
		if b.Tenant != "acme" {
			t.Fatalf("batch Tenant=%q, want acme (the key's tenant, not the metadata)", b.Tenant)
		}
	default:
		t.Fatal("no batch reached the pipeline")
	}
	if len(*got) != 1 || (*got)[0] != [2]string{"grpc", "metadata"} {
		t.Fatalf("conflict metric = %v, want one grpc/metadata record", *got)
	}
}

// A contradicting tenant.id resource attribute is ignored and counted too,
// even when OTLP_TRUST_RESOURCE_TENANT is on.
func TestGRPCAuth_TenantKeyOverridesResourceAttribute(t *testing.T) {
	got := conflicts(t)
	unary, _ := NewGRPCAuthInterceptors(GRPCAuthOptions{
		Auth: grpcAuthenticator(t, "", map[string]string{"acme-key": "acme"}, false),
	})

	h := newAdmissionHarness(t, 8, 0, true) // trustResourceTenant = true
	ctx := mdCtx("authorization", "Bearer acme-key")
	req := buildTracesRequest("svc-a", 1)
	req.ResourceSpans[0].Resource.Attributes = append(req.ResourceSpans[0].Resource.Attributes, strAttr("tenant.id", "victim"))

	if _, err := unary(ctx, nil, &grpc.UnaryServerInfo{}, func(authCtx context.Context, _ any) (any, error) {
		return h.traces.Export(authCtx, req)
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	select {
	case b := <-h.pipeline.queue:
		if b.Tenant != "acme" {
			t.Fatalf("batch Tenant=%q, want acme", b.Tenant)
		}
	default:
		t.Fatal("no batch reached the pipeline")
	}
	if len(*got) != 1 || (*got)[0] != [2]string{"grpc", "resource_attribute"} {
		t.Fatalf("conflict metric = %v, want one grpc/resource_attribute record", *got)
	}
}

// The operator key authenticates only: explicit metadata still selects the
// tenant, so an existing deployment that adds API_KEY keeps its routing.
func TestGRPCAuth_OperatorKeyHonoursMetadataTenant(t *testing.T) {
	got := conflicts(t)
	unary, _ := NewGRPCAuthInterceptors(GRPCAuthOptions{
		Auth: grpcAuthenticator(t, "operator-key", map[string]string{"acme-key": "acme"}, false),
	})

	h := newAdmissionHarness(t, 8, 0, false)
	ctx := mdCtx("authorization", "Bearer operator-key", tenantHeader, "beta")
	if _, err := unary(ctx, nil, &grpc.UnaryServerInfo{}, func(authCtx context.Context, _ any) (any, error) {
		return h.traces.Export(authCtx, buildTracesRequest("svc-a", 1))
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	select {
	case b := <-h.pipeline.queue:
		if b.Tenant != "beta" {
			t.Fatalf("batch Tenant=%q, want beta (operator keys do not bind)", b.Tenant)
		}
	default:
		t.Fatal("no batch reached the pipeline")
	}
	if len(*got) != 0 {
		t.Errorf("operator call counted a conflict: %v", *got)
	}
}

func TestGRPCAuth_RejectsBadCredentials(t *testing.T) {
	var reasons []string
	unary, stream := NewGRPCAuthInterceptors(GRPCAuthOptions{
		Auth:          grpcAuthenticator(t, "operator-key", map[string]string{"acme-key": "acme"}, false),
		OnAuthFailure: func(reason string) { reasons = append(reasons, reason) },
	})

	cases := []struct {
		name   string
		ctx    context.Context
		reason string
	}{
		{"no metadata", context.Background(), authn.ReasonMissingCredential},
		{"no authorization", mdCtx(tenantHeader, "acme"), authn.ReasonMissingCredential},
		{"bad scheme", mdCtx("authorization", "Basic acme-key"), authn.ReasonBadScheme},
		{"unknown key", mdCtx("authorization", "Bearer nope"), authn.ReasonBadKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			_, err := unary(tc.ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})
			if called {
				t.Fatal("handler ran despite failed authentication")
			}
			if grpcstatus.Code(err) != codes.Unauthenticated {
				t.Fatalf("code = %v, want Unauthenticated", grpcstatus.Code(err))
			}
			// The refusal must not tell a prober which part was wrong.
			if msg := grpcstatus.Convert(err).Message(); msg != "unauthenticated" {
				t.Errorf("message = %q, want a non-specific refusal", msg)
			}
		})
	}

	// The stream interceptor enforces exactly the same contract.
	err := stream(nil, &fakeServerStream{ctx: mdCtx("authorization", "Bearer nope")}, &grpc.StreamServerInfo{},
		func(any, grpc.ServerStream) error {
			t.Fatal("stream handler ran despite failed authentication")
			return nil
		})
	if grpcstatus.Code(err) != codes.Unauthenticated {
		t.Fatalf("stream code = %v, want Unauthenticated", grpcstatus.Code(err))
	}

	want := []string{
		authn.ReasonMissingCredential, authn.ReasonMissingCredential,
		authn.ReasonBadScheme, authn.ReasonBadKey, authn.ReasonBadKey,
	}
	if len(reasons) != len(want) {
		t.Fatalf("failure reasons = %v, want %v", reasons, want)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Errorf("reason[%d] = %q, want %q", i, reasons[i], want[i])
		}
	}
}

// The stream interceptor carries the bound identity into the handler's
// context, which is what makes a streaming RPC tenant-safe.
func TestGRPCAuth_StreamCarriesBoundTenant(t *testing.T) {
	got := conflicts(t)
	_, stream := NewGRPCAuthInterceptors(GRPCAuthOptions{
		Auth: grpcAuthenticator(t, "", map[string]string{"acme-key": "acme"}, false),
	})

	var seen string
	err := stream(nil, &fakeServerStream{ctx: mdCtx("authorization", "Bearer acme-key", tenantHeader, "victim")},
		&grpc.StreamServerInfo{}, func(_ any, ss grpc.ServerStream) error {
			seen = storage.TenantFromContext(ss.Context())
			if bound, ok := authn.BoundTenantFromContext(ss.Context()); !ok || bound != "acme" {
				t.Errorf("stream context principal = (%q,%v), want (acme,true)", bound, ok)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if seen != "acme" {
		t.Fatalf("stream tenant = %q, want acme", seen)
	}
	if len(*got) != 1 || (*got)[0] != [2]string{"grpc", "metadata"} {
		t.Fatalf("conflict metric = %v, want one grpc/metadata record", *got)
	}
}

// AUTH_TRUST_EXTERNAL: the dedicated metadata key is an identity; x-tenant-id
// still is not.
func TestGRPCAuth_ExternalIdentityMetadata(t *testing.T) {
	unary, _ := NewGRPCAuthInterceptors(GRPCAuthOptions{
		Auth:                      grpcAuthenticator(t, "", nil, true),
		ExternalTenantMetadataKey: "X-OtelContext-Tenant",
	})
	if unary == nil {
		t.Fatal("trust-external alone must install the interceptors")
	}

	var seen string
	if _, err := unary(mdCtx("x-otelcontext-tenant", "acme", tenantHeader, "victim"), nil, &grpc.UnaryServerInfo{},
		func(ctx context.Context, _ any) (any, error) {
			seen = storage.TenantFromContext(ctx)
			return nil, nil
		}); err != nil {
		t.Fatalf("injected identity: %v", err)
	}
	if seen != "acme" {
		t.Fatalf("tenant = %q, want acme", seen)
	}

	// Without the injected key the call is unauthenticated: the client-
	// controlled tenant metadata is never an identity.
	if _, err := unary(mdCtx(tenantHeader, "acme"), nil, &grpc.UnaryServerInfo{},
		func(context.Context, any) (any, error) { return nil, nil }); grpcstatus.Code(err) != codes.Unauthenticated {
		t.Fatalf("x-tenant-id alone: code = %v, want Unauthenticated", grpcstatus.Code(err))
	}
}

// No credential source configured → no interceptors, so a development
// deployment keeps today's behaviour with zero added cost.
func TestGRPCAuth_DisabledInstallsNothing(t *testing.T) {
	unary, stream := NewGRPCAuthInterceptors(GRPCAuthOptions{Auth: authn.NewAuthenticator("", nil, false)})
	if unary != nil || stream != nil {
		t.Fatal("interceptors installed without a credential source")
	}
}

// fakeServerStream is the minimal grpc.ServerStream a stream interceptor test
// needs: it only has to carry a context.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeServerStream) Context() context.Context { return s.ctx }

// The OTLP HTTP path honours the same binding: X-Tenant-ID cannot move an
// authenticated tenant key's data into another tenant.
func TestHTTPIngest_BoundTenantIgnoresClientHeader(t *testing.T) {
	got := conflicts(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", nil)
	req.Header.Set("X-Tenant-ID", "victim")
	bound := authn.WithPrincipal(req.Context(), authn.Principal{Kind: authn.KindTenant, Tenant: "acme"})
	bound = storage.WithTenantContext(bound, "acme")

	ctx := withTenantFromHTTP(req.WithContext(bound))
	if tenant := storage.TenantFromContext(ctx); tenant != "acme" {
		t.Fatalf("tenant = %q, want acme", tenant)
	}
	if len(*got) != 1 || (*got)[0] != [2]string{"http", "header"} {
		t.Fatalf("conflict metric = %v, want one http/header record", *got)
	}

	// Unbound (operator or unauthenticated) requests keep header precedence.
	plain := httptest.NewRequest(http.MethodPost, "/v1/traces", nil)
	plain.Header.Set("X-Tenant-ID", "beta")
	if tenant := storage.TenantFromContext(withTenantFromHTTP(plain)); tenant != "beta" {
		t.Fatalf("unbound tenant = %q, want beta", tenant)
	}
}
