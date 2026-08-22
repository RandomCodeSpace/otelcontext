package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func wsAuth(t *testing.T, operatorKey string, entries map[string]string, trustExternal bool) *authn.Authenticator {
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

// wsScopeEcho reports the tenant scope the gate pinned on the handshake.
func wsScopeEcho(got *string, reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		if storage.HasTenantContext(r.Context()) {
			*got = storage.TenantFromContext(r.Context())
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestWebSocketGate_CredentialCarriers(t *testing.T) {
	auth := wsAuth(t, "operator-key", map[string]string{"acme-key": "acme"}, false)
	cases := []struct {
		name       string
		header     string
		subproto   string
		query      string
		wantCode   int
		wantTenant string
	}{
		{"authorization header", "Bearer acme-key", "", "", http.StatusOK, "acme"},
		{
			"subprotocol carrier", "",
			WSSubprotocol + ", " + wsAuthProtoPrefix + base64.RawURLEncoding.EncodeToString([]byte("acme-key")),
			"", http.StatusOK, "acme",
		},
		{
			"padded base64url subprotocol", "",
			wsAuthProtoPrefix + base64.URLEncoding.EncodeToString([]byte("acme-key")),
			"", http.StatusOK, "acme",
		},
		{"query-string token is never a credential", "", "", "?token=acme-key", http.StatusUnauthorized, ""},
		{"unauthenticated handshake refused", "", "", "", http.StatusUnauthorized, ""},
		{"unknown key refused", "Bearer nope", "", "", http.StatusUnauthorized, ""},
		{"malformed subprotocol token refused", "", wsAuthProtoPrefix + "!!not-base64!!", "", http.StatusUnauthorized, ""},
		{"operator selects a tenant", "Bearer operator-key", "", "?tenant=beta", http.StatusOK, "beta"},
		{"operator defaults to DEFAULT_TENANT", "Bearer operator-key", "", "", http.StatusOK, "house"},
		{"operator invalid tenant selection", "Bearer operator-key", "", "?tenant=%20", http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var reached bool
			h := WebSocketGate(WSGateOptions{Auth: auth, DefaultTenant: "house"}, wsScopeEcho(&got, &reached))

			req := httptest.NewRequest(http.MethodGet, "/ws/events"+tc.query, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if tc.subproto != "" {
				req.Header.Set("Sec-WebSocket-Protocol", tc.subproto)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body %q)", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantCode != http.StatusOK {
				if reached {
					t.Fatal("rejected handshake still reached the hub")
				}
				return
			}
			if got != tc.wantTenant {
				t.Errorf("tenant scope = %q, want %q", got, tc.wantTenant)
			}
		})
	}
}

// A tenant-key socket is bound: ?tenant= and X-Tenant-ID are ignored and
// counted, never honoured. No merged all-tenant stream exists.
func TestWebSocketGate_TenantKeyBindingIsAbsolute(t *testing.T) {
	auth := wsAuth(t, "", map[string]string{"acme-key": "acme"}, false)
	for _, tc := range []struct{ name, query, header, wantCarrier string }{
		{"query", "?tenant=victim", "", "query"},
		{"header", "", "victim", "header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec conflictRecorder
			rec.install(t)

			var got string
			var reached bool
			h := WebSocketGate(WSGateOptions{Auth: auth, DefaultTenant: "house"}, wsScopeEcho(&got, &reached))
			req := httptest.NewRequest(http.MethodGet, "/ws/events"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer acme-key")
			if tc.header != "" {
				req.Header.Set(TenantHeader, tc.header)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got != "acme" {
				t.Fatalf("tenant scope = %q, want acme", got)
			}
			if len(rec.calls) != 1 || rec.calls[0] != [2]string{"ws", tc.wantCarrier} {
				t.Errorf("conflict metric = %v, want one ws/%s record", rec.calls, tc.wantCarrier)
			}
		})
	}
}

func TestWebSocketGate_OriginPolicy(t *testing.T) {
	auth := wsAuth(t, "operator-key", nil, false)
	cases := []struct {
		name     string
		origin   string
		allowed  []string
		wantCode int
	}{
		{"same host allowed by default", "http://example.test", nil, http.StatusOK},
		{"foreign origin refused", "http://evil.test", nil, http.StatusForbidden},
		{"allowlisted origin", "https://app.example.com", []string{"https://app.example.com"}, http.StatusOK},
		{"allowlisted bare host", "https://app.example.com", []string{"app.example.com"}, http.StatusOK},
		{"non-allowlisted origin", "https://evil.test", []string{"https://app.example.com"}, http.StatusForbidden},
		{"no origin header (non-browser client)", "", []string{"https://app.example.com"}, http.StatusOK},
		{"unparseable origin", "::::", []string{"https://app.example.com"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var reached bool
			h := WebSocketGate(WSGateOptions{
				Auth: auth, DefaultTenant: "house",
				AllowedOrigins: tc.allowed, EnforceOrigin: true,
			}, wsScopeEcho(&got, &reached))

			req := httptest.NewRequest(http.MethodGet, "http://example.test/ws/events", nil)
			req.Host = "example.test"
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			req.Header.Set("Authorization", "Bearer operator-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", w.Code, tc.wantCode)
			}
		})
	}
}

// Origin enforcement applies before authentication so a cross-origin browser
// cannot even probe for credentials, and it applies even when no credential
// source is configured (production without auth).
func TestWebSocketGate_OriginEnforcedWithoutAuth(t *testing.T) {
	var got string
	var reached bool
	h := WebSocketGate(WSGateOptions{
		Auth: authn.NewAuthenticator("", nil, false), EnforceOrigin: true,
	}, wsScopeEcho(&got, &reached))

	req := httptest.NewRequest(http.MethodGet, "http://example.test/ws/events", nil)
	req.Host = "example.test"
	req.Header.Set("Origin", "http://evil.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: want 403, got %d", w.Code)
	}
	if reached {
		t.Fatal("cross-origin handshake reached the hub")
	}
}

// Dev default: no credentials configured and no origin policy → /ws* is
// untouched, exactly as before this gate existed.
func TestWebSocketGate_DisabledIsPassthrough(t *testing.T) {
	var got string
	var reached bool
	h := WebSocketGate(WSGateOptions{Auth: authn.NewAuthenticator("", nil, false)}, wsScopeEcho(&got, &reached))
	req := httptest.NewRequest(http.MethodGet, "/ws/events", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("dev pass-through broken: code=%d reached=%v", w.Code, reached)
	}
	if got != "" {
		t.Errorf("unscoped socket pinned tenant %q", got)
	}
}

// Non-/ws paths are none of this gate's business.
func TestWebSocketGate_IgnoresNonWSPaths(t *testing.T) {
	var got string
	var reached bool
	h := WebSocketGate(WSGateOptions{Auth: wsAuth(t, "operator-key", nil, false), EnforceOrigin: true}, wsScopeEcho(&got, &reached))
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("Origin", "http://evil.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("non-ws path was gated: code=%d reached=%v", w.Code, reached)
	}
}

func TestWSAllowedOriginHosts(t *testing.T) {
	got := WSAllowedOriginHosts([]string{"https://app.example.com", " dash.example.com ", ""})
	want := []string{"app.example.com", "dash.example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
