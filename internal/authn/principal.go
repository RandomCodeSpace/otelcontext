// Package authn holds the transport-independent authentication primitives
// shared by the HTTP, WebSocket, and gRPC surfaces: the tenant key store and
// the authenticated principal that travels on a request context.
//
// The package deliberately knows nothing about net/http or gRPC — each
// transport adapts its own credential carrier (Authorization header,
// Sec-WebSocket-Protocol entry, gRPC metadata) into the same Principal.
package authn

import "context"

const (
	// WSSubprotocol is the only WebSocket subprotocol the server echoes. A
	// browser that must carry a credential offers it alongside an
	// `auth.<base64url-token>` entry; the server selects this one, so the token
	// never appears in the negotiated protocol or in a log line.
	WSSubprotocol = "otelcontext.v1"

	// WSAuthProtoPrefix marks the credential-bearing subprotocol entry.
	WSAuthProtoPrefix = "auth."
)

// Kind classifies an authenticated credential.
type Kind string

const (
	// KindOperator is the shared API_KEY. It is authorized for every tenant,
	// so tenant selection keeps its historical precedence (explicit header or
	// metadata → trusted resource attribute → DEFAULT_TENANT).
	KindOperator Kind = "operator"

	// KindTenant is a key from API_TENANT_KEYS_FILE. Its tenant binding is
	// absolute: client-asserted tenant headers, gRPC metadata, and OTLP
	// `tenant.id` resource attributes are ignored (and counted) for the
	// lifetime of the request or connection.
	KindTenant Kind = "tenant"

	// KindExternal is an identity injected by a front proxy and trusted only
	// because AUTH_TRUST_EXTERNAL=true. It binds like KindTenant.
	KindExternal Kind = "external"
)

// Principal is the authenticated identity of a request or connection.
// The zero value means "unauthenticated" — which is the normal state of a
// development deployment with no API_KEY and no tenant keys file.
type Principal struct {
	Kind   Kind
	Tenant string
}

// Bound reports whether the principal pins the tenant irrevocably. Operator
// principals are never bound: they may address any tenant.
func (p Principal) Bound() bool {
	return (p.Kind == KindTenant || p.Kind == KindExternal) && p.Tenant != ""
}

// Authenticated reports whether a credential was presented and accepted.
func (p Principal) Authenticated() bool { return p.Kind != "" }

type principalCtxKey struct{}

// WithPrincipal returns a copy of ctx carrying the authenticated principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the principal stashed by WithPrincipal.
// The second result is false when the request was never authenticated.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}

// BoundTenantFromContext returns the tenant a bound principal pinned onto ctx.
// Operator and unauthenticated contexts return ("", false), which leaves the
// caller's existing tenant precedence untouched.
func BoundTenantFromContext(ctx context.Context) (string, bool) {
	p, ok := PrincipalFromContext(ctx)
	if !ok || !p.Bound() {
		return "", false
	}
	return p.Tenant, true
}

// ConflictHook is called when a bound principal ignores a client-asserted
// tenant. surface is "http", "ws", or "grpc"; reason names the ignored
// carrier ("header", "metadata", "resource_attribute"). Wired by main.go to a
// Prometheus counter; safe to leave nil. Never called with credential
// material — only the surface and the carrier name.
var ConflictHook func(surface, reason string)

// RecordConflict reports an ignored tenant assertion. No-op when unset.
func RecordConflict(surface, reason string) {
	if ConflictHook != nil {
		ConflictHook(surface, reason)
	}
}
