package authn

import (
	"strings"
	"testing"
)

func testStore(t *testing.T) *KeyStore {
	t.Helper()
	s, err := NewKeyStoreFromMap(map[string]string{"tenant-key": "acme"})
	if err != nil {
		t.Fatalf("NewKeyStoreFromMap: %v", err)
	}
	return s
}

func TestAuthenticator_Disabled(t *testing.T) {
	var nilAuth *Authenticator
	if nilAuth.Enabled() {
		t.Error("nil authenticator must be disabled")
	}
	a := NewAuthenticator("", nil, false)
	if a.Enabled() {
		t.Error("no credential source configured must be disabled")
	}
	// A disabled authenticator authenticates nothing, so a caller cannot
	// accidentally gate traffic with it.
	if _, _, ok := a.AuthenticateToken("anything"); ok {
		t.Error("disabled authenticator must not authenticate")
	}
}

func TestAuthenticator_OperatorAndTenantKinds(t *testing.T) {
	a := NewAuthenticator("operator-key", testStore(t), false)
	if !a.Enabled() || !a.HasTenantKeys() || a.TenantKeyCount() != 1 {
		t.Fatalf("unexpected state: enabled=%v tenantKeys=%v count=%d", a.Enabled(), a.HasTenantKeys(), a.TenantKeyCount())
	}

	p, _, ok := a.AuthenticateToken("operator-key")
	if !ok || p.Kind != KindOperator || p.Bound() {
		t.Fatalf("operator key: got (%+v, %v), want unbound operator", p, ok)
	}
	p, _, ok = a.AuthenticateToken("tenant-key")
	if !ok || p.Kind != KindTenant || p.Tenant != "acme" || !p.Bound() {
		t.Fatalf("tenant key: got (%+v, %v), want bound acme", p, ok)
	}
	if _, reason, ok := a.AuthenticateToken("nope"); ok || reason != ReasonBadKey {
		t.Fatalf("unknown key: got (%q,%v), want (bad_key,false)", reason, ok)
	}
	if _, reason, ok := a.AuthenticateToken(""); ok || reason != ReasonMissingCredential {
		t.Fatalf("empty token: got (%q,%v), want (missing_header,false)", reason, ok)
	}
}

func TestAuthenticator_ExternalIdentityRequiresFlag(t *testing.T) {
	off := NewAuthenticator("operator-key", nil, false)
	if _, ok := off.ExternalPrincipal("acme"); ok {
		t.Fatal("external identity must be refused unless AUTH_TRUST_EXTERNAL is set")
	}
	on := NewAuthenticator("", nil, true)
	if !on.Enabled() {
		t.Fatal("trust-external alone enables the gate")
	}
	p, ok := on.ExternalPrincipal("acme")
	if !ok || p.Kind != KindExternal || p.Tenant != "acme" || !p.Bound() {
		t.Fatalf("external principal: got (%+v,%v)", p, ok)
	}
	// The injected value still goes through the shared tenant sanitizer.
	for _, bad := range []string{"", "   ", "ac\nme", strings.Repeat("t", 200)} {
		if _, ok := on.ExternalPrincipal(bad); ok {
			t.Errorf("sanitizer accepted %q", bad)
		}
	}
}

func TestTokenFromAuthorization(t *testing.T) {
	cases := []struct {
		header string
		token  string
		reason string
	}{
		{"", "", ReasonMissingCredential},
		{"Bearer ", "", ReasonMissingCredential},
		{"Basic abc", "", ReasonBadScheme},
		{"bearer abc", "", ReasonBadScheme}, // scheme is case-sensitive, as before
		{"Bearer abc", "abc", ""},
	}
	for _, tc := range cases {
		token, reason := TokenFromAuthorization(tc.header)
		if token != tc.token || reason != tc.reason {
			t.Errorf("TokenFromAuthorization(%q) = (%q,%q), want (%q,%q)", tc.header, token, reason, tc.token, tc.reason)
		}
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, ok := PrincipalFromContext(nil); ok { //nolint:staticcheck // nil context is the case under test
		t.Error("nil context must carry no principal")
	}
	ctx := WithPrincipal(nil, Principal{Kind: KindTenant, Tenant: "acme"}) //nolint:staticcheck // ditto
	if tenant, ok := BoundTenantFromContext(ctx); !ok || tenant != "acme" {
		t.Fatalf("BoundTenantFromContext = (%q,%v), want (acme,true)", tenant, ok)
	}
	// Operator principals never bind: tenant precedence stays with the caller.
	ctx = WithPrincipal(ctx, Principal{Kind: KindOperator})
	if _, ok := BoundTenantFromContext(ctx); ok {
		t.Error("operator principal must not bind a tenant")
	}
}

func TestRecordConflict_NilHookIsSafe(t *testing.T) {
	prev := ConflictHook
	t.Cleanup(func() { ConflictHook = prev })
	ConflictHook = nil
	RecordConflict("http", "header") // must not panic

	var got [2]string
	ConflictHook = func(surface, reason string) { got = [2]string{surface, reason} }
	RecordConflict("grpc", "metadata")
	if got != [2]string{"grpc", "metadata"} {
		t.Fatalf("hook got %v", got)
	}
}
