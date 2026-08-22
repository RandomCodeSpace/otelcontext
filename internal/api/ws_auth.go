package api

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

const (
	// WSSubprotocol is the only subprotocol the server ever echoes. Defined in
	// authn so the realtime hubs can negotiate it without importing this
	// package (api already imports realtime).
	WSSubprotocol = authn.WSSubprotocol

	// wsAuthProtoPrefix marks the credential-bearing subprotocol entry.
	wsAuthProtoPrefix = authn.WSAuthProtoPrefix

	// WSTenantParam is the query parameter an operator credential uses to
	// select the single tenant a socket is scoped to.
	WSTenantParam = "tenant"
)

// WSGateOptions configures the WebSocket handshake gate.
type WSGateOptions struct {
	// Auth resolves credentials. Disabled Authenticator = pass-through, which
	// keeps /ws* open in a default development deployment exactly as before.
	Auth *authn.Authenticator
	// DefaultTenant scopes an operator socket that selects no tenant.
	DefaultTenant string
	// AllowedOrigins is WS_ALLOWED_ORIGINS: exact origins ("https://app.example.com")
	// or bare hosts ("app.example.com"). Empty means same-host only.
	AllowedOrigins []string
	// EnforceOrigin turns the origin policy on. Set when authentication is
	// enabled or APP_ENV=production.
	EnforceOrigin bool
	// ExternalTenantHeader overrides DefaultExternalTenantHeader.
	ExternalTenantHeader string
}

// IsWebSocketPath reports whether a path belongs to the /ws* namespace.
func IsWebSocketPath(path string) bool { return strings.HasPrefix(path, "/ws") }

// WebSocketGate authenticates the /ws* handshake and scopes the connection to
// exactly one tenant before the upgrade happens.
//
// Credential carriers, in order: `Authorization: Bearer <token>` (non-browser
// clients), then a `Sec-WebSocket-Protocol` entry of the form
// `auth.<base64url-token>` (browsers, which cannot set headers on a WebSocket).
// Query-string tokens are never accepted — they land in access logs, browser
// history, and Referer headers.
//
// Scope: a tenant-key or proxy-injected identity binds the socket to its
// tenant and any client-selected tenant is ignored and counted. An operator
// credential selects exactly one tenant via ?tenant= or X-Tenant-ID, falling
// back to DEFAULT_TENANT. There is no merged all-tenant stream.
func WebSocketGate(o WSGateOptions, next http.Handler) http.Handler {
	if !o.Auth.Enabled() && !o.EnforceOrigin {
		return next
	}
	extHeader := DefaultExternalTenantHeader
	if o.ExternalTenantHeader != "" {
		extHeader = o.ExternalTenantHeader
	}
	defaultTenant := o.DefaultTenant
	if defaultTenant == "" {
		defaultTenant = storage.DefaultTenantID
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsWebSocketPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if o.EnforceOrigin && !originAllowed(r.Header.Get("Origin"), r.Host, o.AllowedOrigins) {
			recordAuthFailure(authn.ReasonBadOrigin)
			// The rejected origin is attacker-controlled, so it is sanitized
			// before it reaches a log line. The credential-bearing subprotocol
			// header is never logged in any form.
			slog.Warn("WebSocket handshake rejected: origin not allowed",
				"origin", sanitizeLogValue(r.Header.Get("Origin")))
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if !o.Auth.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		principal, reason, ok := authenticateWS(o.Auth, r, extHeader)
		if !ok {
			recordAuthFailure(reason)
			writeUnauthorized(w)
			return
		}
		tenant, ok := wsTenantScope(r, principal, defaultTenant)
		if !ok {
			recordAuthFailure(authn.ReasonBadTenant)
			http.Error(w, "invalid tenant selection", http.StatusBadRequest)
			return
		}
		ctx := authn.WithPrincipal(r.Context(), principal)
		ctx = storage.WithTenantContext(ctx, tenant)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authenticateWS extracts a credential from the Authorization header or the
// subprotocol carrier and resolves it. The raw subprotocol header value never
// reaches a log line — only its decode outcome does.
func authenticateWS(a *authn.Authenticator, r *http.Request, extHeader string) (authn.Principal, string, bool) {
	token, reason := authn.TokenFromAuthorization(r.Header.Get("Authorization"))
	if token == "" {
		if t, found := wsTokenFromSubprotocols(r.Header.Values("Sec-WebSocket-Protocol")); found {
			token, reason = t, ""
		}
	}
	if token == "" {
		if reason == authn.ReasonMissingCredential || reason == "" {
			if p, ok := a.ExternalPrincipal(r.Header.Get(extHeader)); ok {
				return p, "", true
			}
			reason = authn.ReasonMissingCredential
		}
		return authn.Principal{}, reason, false
	}
	return a.AuthenticateToken(token)
}

// wsTokenFromSubprotocols decodes the first `auth.<base64url>` entry.
// Both padded and unpadded base64url are accepted; anything that fails to
// decode is reported as "no token" rather than being passed through, so a
// malformed entry can never be compared against a key.
func wsTokenFromSubprotocols(headers []string) (string, bool) {
	for _, h := range headers {
		for _, part := range strings.Split(h, ",") {
			entry := strings.TrimSpace(part)
			if !strings.HasPrefix(entry, wsAuthProtoPrefix) {
				continue
			}
			enc := strings.TrimPrefix(entry, wsAuthProtoPrefix)
			if enc == "" {
				continue
			}
			if raw, err := base64.RawURLEncoding.DecodeString(enc); err == nil && len(raw) > 0 {
				return string(raw), true
			}
			if raw, err := base64.URLEncoding.DecodeString(enc); err == nil && len(raw) > 0 {
				return string(raw), true
			}
			slog.Debug("WebSocket auth subprotocol entry is not valid base64url (value redacted)")
		}
	}
	return "", false
}

// wsTenantScope resolves the single tenant a socket is scoped to.
func wsTenantScope(r *http.Request, p authn.Principal, defaultTenant string) (string, bool) {
	asserted := r.URL.Query().Get(WSTenantParam)
	carrier := "query"
	if asserted == "" {
		asserted = r.Header.Get(TenantHeader)
		carrier = "header"
	}
	if p.Bound() {
		if asserted != "" && storage.SanitizeTenantID(asserted) != p.Tenant {
			authn.RecordConflict("ws", carrier)
		}
		return p.Tenant, true
	}
	if asserted == "" {
		return defaultTenant, true
	}
	tenant := storage.SanitizeTenantID(asserted)
	if tenant == "" {
		return "", false
	}
	return tenant, true
}

// sanitizeLogValue makes an attacker-controlled string safe to put in a
// structured log line: printable ASCII only, bounded length. Structured
// handlers quote most of this already; this makes it true regardless of the
// handler in use.
func sanitizeLogValue(s string) string {
	const maxLen = 128
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		out = append(out, r)
	}
	return string(out)
}

// originAllowed implements the WS_ALLOWED_ORIGINS policy. A request with no
// Origin header is a non-browser client (curl, an SDK, a collector) and is not
// subject to browser-origin rules — it still has to authenticate.
func originAllowed(origin, host string, allowed []string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if len(allowed) == 0 {
		return strings.EqualFold(u.Host, host)
	}
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.EqualFold(a, origin) || strings.EqualFold(a, u.Host) {
			return true
		}
	}
	return false
}

// WSAllowedOriginHosts reduces configured origins to bare hosts, which is the
// shape the WebSocket library's origin patterns expect.
func WSAllowedOriginHosts(allowed []string) []string {
	out := make([]string, 0, len(allowed))
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if u, err := url.Parse(a); err == nil && u.Host != "" {
			out = append(out, u.Host)
			continue
		}
		out = append(out, a)
	}
	return out
}
