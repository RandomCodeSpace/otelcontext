package authn

import (
	"crypto/subtle"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// BearerPrefix is the only credential scheme accepted on every surface.
const BearerPrefix = "Bearer "

// Failure reasons reported to the auth-failure metric. They are stable
// strings: "missing_header", "bad_scheme", and "bad_key" predate this package
// and are kept byte-identical so existing dashboards keep working.
const (
	ReasonMissingCredential = "missing_header"
	ReasonBadScheme         = "bad_scheme"
	ReasonBadKey            = "bad_key"
	ReasonBadOrigin         = "bad_origin"
	ReasonBadTenant         = "bad_tenant"
)

// Authenticator resolves a bearer token into a Principal. It is shared by the
// HTTP, WebSocket, and gRPC adapters so all three surfaces agree on what a
// credential means; only credential extraction differs per transport.
//
// A zero-value / disabled Authenticator authenticates nothing and reports
// Enabled() == false, which is how a development deployment with no API_KEY,
// no tenant keys file, and no external trust keeps today's open behaviour.
type Authenticator struct {
	operatorKey []byte
	store       *KeyStore
	trust       bool
}

// NewAuthenticator wires the operator key (API_KEY), the per-tenant key store
// (API_TENANT_KEYS_FILE), and the AUTH_TRUST_EXTERNAL switch.
func NewAuthenticator(operatorKey string, store *KeyStore, trustExternal bool) *Authenticator {
	a := &Authenticator{store: store, trust: trustExternal}
	if operatorKey != "" {
		a.operatorKey = []byte(operatorKey)
	}
	return a
}

// Enabled reports whether any credential source is configured. When false the
// caller must not gate anything: authentication arrives with configuration,
// never by surprise.
func (a *Authenticator) Enabled() bool {
	if a == nil {
		return false
	}
	return len(a.operatorKey) > 0 || a.store.Enabled() || a.trust
}

// TrustExternal reports whether a proxy-injected identity header is honoured.
func (a *Authenticator) TrustExternal() bool { return a != nil && a.trust }

// HasTenantKeys reports whether per-tenant keys are configured.
func (a *Authenticator) HasTenantKeys() bool { return a != nil && a.store.Enabled() }

// TenantKeyCount is the number of configured tenant keys (never their values).
func (a *Authenticator) TenantKeyCount() int {
	if a == nil {
		return 0
	}
	return a.store.Len()
}

// Tenants lists the tenants covered by the key store.
func (a *Authenticator) Tenants() []string {
	if a == nil {
		return nil
	}
	return a.store.Tenants()
}

// AuthenticateToken resolves a raw bearer token. The operator key is checked
// first with a constant-time compare, then the tenant key store. A miss
// returns ReasonBadKey; an empty token returns ReasonMissingCredential.
func (a *Authenticator) AuthenticateToken(token string) (Principal, string, bool) {
	if !a.Enabled() {
		return Principal{}, "", false
	}
	if token == "" {
		return Principal{}, ReasonMissingCredential, false
	}
	if len(a.operatorKey) > 0 {
		got := []byte(token)
		if len(got) == len(a.operatorKey) && subtle.ConstantTimeCompare(got, a.operatorKey) == 1 {
			return Principal{Kind: KindOperator}, "", true
		}
	}
	if tenant, ok := a.store.Lookup(token); ok {
		return Principal{Kind: KindTenant, Tenant: tenant}, "", true
	}
	return Principal{}, ReasonBadKey, false
}

// ExternalPrincipal converts a proxy-injected tenant value into a bound
// principal. It returns false unless AUTH_TRUST_EXTERNAL is on and the value
// survives the shared tenant sanitizer.
//
// The injected header is trusted ONLY because the operator asserted that a
// front proxy authenticates the caller, strips inbound copies of this header,
// and makes the application ports unreachable except through it. Without those
// conditions this is an authentication bypass — see CLAUDE.md.
func (a *Authenticator) ExternalPrincipal(raw string) (Principal, bool) {
	if !a.TrustExternal() {
		return Principal{}, false
	}
	tenant := storage.SanitizeTenantID(raw)
	if tenant == "" {
		return Principal{}, false
	}
	return Principal{Kind: KindExternal, Tenant: tenant}, true
}

// TokenFromAuthorization extracts the bearer token from an Authorization
// header value. The second result is a failure reason when no usable token is
// present; callers that have a second credential carrier (WebSocket
// subprotocol, gRPC metadata) may ignore it and try that instead.
func TokenFromAuthorization(header string) (string, string) {
	if header == "" {
		return "", ReasonMissingCredential
	}
	if !strings.HasPrefix(header, BearerPrefix) {
		return "", ReasonBadScheme
	}
	token := strings.TrimPrefix(header, BearerPrefix)
	if token == "" {
		return "", ReasonMissingCredential
	}
	return token, ""
}
