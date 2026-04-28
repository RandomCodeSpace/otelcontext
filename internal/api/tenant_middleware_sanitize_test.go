package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// TestTenantMiddleware_SanitizesHeader verifies that an X-Tenant-ID header
// containing control characters or excessive length is rejected back to the
// configured default — preventing log injection on slog structured fields
// and silent VARCHAR truncation at the GORM layer.
func TestTenantMiddleware_SanitizesHeader(t *testing.T) {
	mw := TenantMiddleware(nil) // nil cfg → DefaultTenantID fallback
	cases := []struct {
		header     string
		wantTenant string
	}{
		{"acme", "acme"},
		{"foo\nbar", storage.DefaultTenantID},               // log-injection attempt
		{strings.Repeat("x", 200), storage.DefaultTenantID}, // over-length
		{"", storage.DefaultTenantID},                       // empty
		{"   ", storage.DefaultTenantID},                    // whitespace-only
		{"foo\x00bar", storage.DefaultTenantID},             // NUL byte
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			var got string
			h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = storage.TenantFromContext(r.Context())
			}))
			r := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
			r.Header.Set(TenantHeader, tc.header)
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != tc.wantTenant {
				t.Errorf("header=%q -> tenant=%q, want %q", tc.header, got, tc.wantTenant)
			}
		})
	}
}
