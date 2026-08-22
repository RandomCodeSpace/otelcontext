package ingest

import (
	"context"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

// authorizationMD is the gRPC metadata key carrying the bearer credential.
// gRPC lower-cases metadata keys on the wire, so this is the canonical form.
const authorizationMD = "authorization"

// GRPCAuthOptions configures the OTLP gRPC authentication interceptors.
type GRPCAuthOptions struct {
	// Auth resolves credentials. A disabled Authenticator yields nil
	// interceptors and the server keeps its current unauthenticated behaviour
	// — which is the development default.
	Auth *authn.Authenticator
	// ExternalTenantMetadataKey is the proxy-injected identity key, honoured
	// only under AUTH_TRUST_EXTERNAL. Compared lower-cased.
	ExternalTenantMetadataKey string
	// OnAuthFailure receives the failure reason for metrics. Optional.
	OnAuthFailure func(reason string)
}

// NewGRPCAuthInterceptors returns the unary and stream interceptors enforcing
// bearer authentication on the OTLP gRPC listener.
//
// A tenant key binds absolutely: the authenticated tenant is pinned onto the
// call context, and an `x-tenant-id` metadata value that disagrees is ignored
// and counted rather than honoured. An operator key authenticates only —
// tenant precedence stays (metadata → trusted resource attribute →
// DEFAULT_TENANT), so an existing single-tenant deployment that adds API_KEY
// sees no change in where its rows land.
//
// Both results are nil when authentication is not configured, so the caller
// installs nothing and pays nothing.
func NewGRPCAuthInterceptors(o GRPCAuthOptions) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	if !o.Auth.Enabled() {
		return nil, nil
	}
	extKey := strings.ToLower(strings.TrimSpace(o.ExternalTenantMetadataKey))
	authorize := func(ctx context.Context) (context.Context, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		principal, reason, ok := authenticateGRPC(o.Auth, md, extKey)
		if !ok {
			if o.OnAuthFailure != nil {
				o.OnAuthFailure(reason)
			}
			// The reason is deliberately not returned to the peer: it would
			// tell a prober whether a key exists. Credentials are never logged.
			return nil, grpcstatus.Error(codes.Unauthenticated, "unauthenticated")
		}
		return bindGRPCPrincipal(ctx, md, principal), nil
	}

	unary := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		authCtx, err := authorize(ctx)
		if err != nil {
			return nil, err
		}
		return handler(authCtx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		authCtx, err := authorize(ss.Context())
		if err != nil {
			return err
		}
		return handler(srv, &authenticatedStream{ServerStream: ss, ctx: authCtx})
	}
	return unary, stream
}

// authenticateGRPC resolves the principal for one call.
func authenticateGRPC(a *authn.Authenticator, md metadata.MD, extKey string) (authn.Principal, string, bool) {
	var header string
	if vals := md.Get(authorizationMD); len(vals) > 0 {
		header = vals[0]
	}
	token, reason := authn.TokenFromAuthorization(header)
	if token == "" {
		if reason == authn.ReasonMissingCredential && extKey != "" {
			if vals := md.Get(extKey); len(vals) > 0 {
				if p, ok := a.ExternalPrincipal(vals[0]); ok {
					return p, "", true
				}
			}
		}
		return authn.Principal{}, reason, false
	}
	return a.AuthenticateToken(token)
}

// bindGRPCPrincipal pins the authenticated identity (and, for a bound
// principal, its tenant) onto the call context.
func bindGRPCPrincipal(ctx context.Context, md metadata.MD, p authn.Principal) context.Context {
	ctx = authn.WithPrincipal(ctx, p)
	if !p.Bound() {
		return ctx
	}
	if vals := md.Get(tenantHeader); len(vals) > 0 && storage.SanitizeTenantID(vals[0]) != p.Tenant {
		authn.RecordConflict("grpc", "metadata")
	}
	return storage.WithTenantContext(ctx, p.Tenant)
}

// authenticatedStream carries the authenticated context into a streaming
// handler. grpc.ServerStream has no setter for its context, so the standard
// approach is this thin wrapper.
type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedStream) Context() context.Context { return s.ctx }
