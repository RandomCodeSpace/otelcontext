package api

import (
	"context"
	"net/http"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// DefaultExternalTenantHeader is the dedicated identity header a front proxy
// injects when AUTH_TRUST_EXTERNAL=true. It is deliberately NOT X-Tenant-ID:
// that header stays client-controlled and untrusted, so a proxy that forgets
// to strip inbound copies of the dedicated header fails visibly rather than
// silently promoting client input to an identity.
const DefaultExternalTenantHeader = "X-OtelContext-Tenant"

// AuthGateOptions configures the HTTP authentication middleware.
type AuthGateOptions struct {
	// Auth resolves bearer tokens. A disabled Authenticator makes the gate a
	// pass-through — authentication arrives with configuration, not by default.
	Auth *authn.Authenticator
	// MCPPath is used by IsProtectedPath to gate the MCP endpoint.
	MCPPath string
	// ExternalTenantHeader overrides DefaultExternalTenantHeader.
	ExternalTenantHeader string
}

func (o AuthGateOptions) externalHeader() string {
	if o.ExternalTenantHeader != "" {
		return o.ExternalTenantHeader
	}
	return DefaultExternalTenantHeader
}

// AuthGate authenticates /api/*, /v1/*, and the MCP endpoint.
//
// Two credential classes, one gate:
//
//   - The operator key (API_KEY) authenticates and nothing more. Tenant
//     resolution keeps its historical precedence — X-Tenant-ID header, then
//     the OTLP resource attribute where trusted, then DEFAULT_TENANT — so an
//     API_KEY-only deployment behaves exactly as it did before this middleware
//     existed.
//   - A tenant key (API_TENANT_KEYS_FILE) authenticates AND binds. The bound
//     tenant is pinned onto the request context and a client-supplied
//     X-Tenant-ID is ignored and counted, never honoured.
//
// When AUTH_TRUST_EXTERNAL=true and no bearer credential is presented, the
// proxy-injected identity header supplies a bound principal.
func AuthGate(o AuthGateOptions, next http.Handler) http.Handler {
	if !o.Auth.Enabled() {
		return next
	}
	extHeader := o.externalHeader()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsProtectedPath(r.URL.Path, o.MCPPath) {
			next.ServeHTTP(w, r)
			return
		}
		principal, reason, ok := authenticateHTTP(o.Auth, r, extHeader)
		if !ok {
			recordAuthFailure(reason)
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(bindPrincipal(r, principal)))
	})
}

// authenticateHTTP resolves the principal for one request: bearer credential
// first, proxy-injected identity only when no bearer credential was presented
// at all. A presented-but-invalid credential is always a 401, even under
// AUTH_TRUST_EXTERNAL — otherwise a bad key would silently downgrade into
// whatever the header claimed.
func authenticateHTTP(a *authn.Authenticator, r *http.Request, extHeader string) (authn.Principal, string, bool) {
	token, reason := authn.TokenFromAuthorization(r.Header.Get("Authorization"))
	if token == "" {
		if reason == authn.ReasonMissingCredential {
			if p, ok := a.ExternalPrincipal(r.Header.Get(extHeader)); ok {
				return p, "", true
			}
		}
		return authn.Principal{}, reason, false
	}
	return a.AuthenticateToken(token)
}

// bindPrincipal stashes the principal on the request context and, for bound
// principals, pins the tenant so every downstream repository read and ingest
// write is scoped to it. Contradicting client assertions are counted here —
// this is the only place on the HTTP surface that sees both.
func bindPrincipal(r *http.Request, p authn.Principal) context.Context {
	ctx := authn.WithPrincipal(r.Context(), p)
	if !p.Bound() {
		return ctx
	}
	if asserted := r.Header.Get(TenantHeader); asserted != "" && storage.SanitizeTenantID(asserted) != p.Tenant {
		authn.RecordConflict("http", "header")
	}
	return storage.WithTenantContext(ctx, p.Tenant)
}
