package aggregate

import "testing"

type normCase struct {
	name string
	in   string
	want string
}

// runNormCases exercises a single-string normalization function against a
// case table. Shared by the NormalizePath and NormalizeSpanName tests.
func runNormCases(t *testing.T, fnName string, fn func(string) string, cases []normCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fn(tc.in); got != tc.want {
				t.Fatalf("%s(%q) = %q, want %q", fnName, tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizePath(t *testing.T) {
	cases := []normCase{
		// Untouched paths.
		{"empty", "", ""},
		{"root", "/", "/"},
		{"static", "/users", "/users"},
		{"static multi", "/api/v1/health", "/api/v1/health"},
		{"template already", "/users/{userId}/orders", "/users/{userId}/orders"},
		{"short hex", "/objects/deadbee", "/objects/deadbee"},
		{"short numeric-looking word", "/v2/status", "/v2/status"},
		{"long lowercase word", "/docs/administrationguide", "/docs/administrationguide"},
		{"camel case without digits", "/api/GetUserProfileByName", "/api/GetUserProfileByName"},
		{"percent encoded", "/search/%20term", "/search/%20term"},

		// All-numeric.
		{"numeric", "/users/1234", "/users/{id}"},
		{"numeric zero", "/users/0", "/users/{id}"},
		{"numeric single digit", "/p/7/c", "/p/{id}/c"},
		{"two numerics", "/users/1234/orders/987", "/users/{id}/orders/{id}"},
		{"leading numeric", "/1234/detail", "/{id}/detail"},
		{"trailing slash", "/users/1234/", "/users/{id}/"},
		{"long numeric", "/events/1750000000000", "/events/{id}"},

		// UUID.
		{"uuid", "/users/550e8400-e29b-41d4-a716-446655440000", "/users/{id}"},
		{"uuid upper", "/users/550E8400-E29B-41D4-A716-446655440000", "/users/{id}"},
		{"uuid nil", "/t/00000000-0000-0000-0000-000000000000", "/t/{id}"},
		{"uuid wrong dashes", "/t/550e8400e29b-41d4-a716-4466554400001", "/t/550e8400e29b-41d4-a716-4466554400001"},

		// >= 8 hex.
		{"hex 8", "/objects/deadbeef", "/objects/{id}"},
		{"hex 32", "/traces/4bf92f3577b34da6a3ce929d0e0e4736", "/traces/{id}"},
		{"hex mixed case", "/objects/DeadBeef00", "/objects/{id}"},

		// Base64-like >= 16 with mixed classes.
		{"base64 16", "/files/dGhpc0lzQVRlc3Qx", "/files/{id}"},
		{"base64 url safe", "/files/aBc-dEf_ghIj123456", "/files/{id}"},
		{"base64 padded", "/files/dGhpc0lzQVRlc3Q=", "/files/{id}"},
		{"base64 too short", "/files/dGhpc0lzQQ==", "/files/dGhpc0lzQQ=="},
		{"long but single class", "/files/abcdefghijklmnopqrst", "/files/abcdefghijklmnopqrst"},
		{"long upper and lower no digit", "/files/AbCdEfGhIjKlMnOpQr", "/files/AbCdEfGhIjKlMnOpQr"},
		{"long with disallowed char", "/files/this.is.a.long.file.name", "/files/this.is.a.long.file.name"},

		// Query and fragment stripping.
		{"query stripped", "/users/1234?foo=bar", "/users/{id}"},
		{"fragment stripped", "/users/1234#section", "/users/{id}"},
		{"query then fragment", "/users/1234?a=b#c", "/users/{id}"},
		{"fragment then query", "/users/1234#c?a=b", "/users/{id}"},
		{"query on static path", "/health?verbose=1", "/health"},
		{"query with slashes", "/users/1234?next=/a/b/9", "/users/{id}"},
		{"bare query", "/?a=b", "/"},

		// Empty segments survive as-is.
		{"double slash", "//users//1234", "//users//{id}"},

		// Not-a-path: verbatim, query included.
		{"no leading slash", "users/1234", "users/1234"},
		{"relative with query", "not a path?q=1", "not a path?q=1"},
		{"scheme", "https://example.com/users/1234", "https://example.com/users/1234"},
		{"bare word", "checkout", "checkout"},
		{"invalid utf8", "/users/\xff\xfe", "/users/\xff\xfe"},
		{"query only", "?a=b", "?a=b"},
	}
	runNormCases(t, "NormalizePath", NormalizePath, cases)
}

func TestNormalizePathIsDeterministic(t *testing.T) {
	const in = "/users/1234/orders/550e8400-e29b-41d4-a716-446655440000/items/deadbeef"
	first := NormalizePath(in)
	for range 100 {
		if got := NormalizePath(in); got != first {
			t.Fatalf("NormalizePath is not deterministic: %q then %q", first, got)
		}
	}
	if first != "/users/{id}/orders/{id}/items/{id}" {
		t.Fatalf("NormalizePath = %q", first)
	}
}

func TestNormalizeSpanName(t *testing.T) {
	cases := []normCase{
		{"method and path", "GET /users/1234/orders/987", "GET /users/{id}/orders/{id}"},
		{"method and static path", "POST /orders", "POST /orders"},
		{"lowercase method preserved", "get /users/1", "get /users/{id}"},
		{"query stripped", "GET /users/1234?x=1", "GET /users/{id}"},
		{"query on static path", "GET /health?x=1", "GET /health"},
		{"uuid segment", "DELETE /users/550e8400-e29b-41d4-a716-446655440000", "DELETE /users/{id}"},
		{"every method", "OPTIONS /a/1", "OPTIONS /a/{id}"},

		// Not the "<METHOD> /path" shape: verbatim.
		{"empty", "", ""},
		{"no space", "GET", "GET"},
		{"leading space", " /users/1", " /users/1"},
		{"unknown method", "FROBNICATE /users/1234", "FROBNICATE /users/1234"},
		{"protocol prefix", "HTTP GET", "HTTP GET"},
		{"db statement", "SELECT users", "SELECT users"},
		{"messaging", "orders.created send", "orders.created send"},
		{"no leading slash in path", "GET users/1234", "GET users/1234"},
		{"internal span name", "checkout.reserveInventory", "checkout.reserveInventory"},
		{"span name with numbers", "process batch 1234", "process batch 1234"},
		{"invalid utf8", "GET /users/\xff", "GET /users/\xff"},
	}
	runNormCases(t, "NormalizeSpanName", NormalizeSpanName, cases)
}

func TestNormalizeSpanNameAcceptsEveryKnownMethod(t *testing.T) {
	for m := MethodGet; m < MethodOther; m++ {
		in := m.String() + " /users/1234"
		want := m.String() + " /users/{id}"
		if got := NormalizeSpanName(in); got != want {
			t.Errorf("NormalizeSpanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeOperationPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		route    string
		urlPath  string
		spanName string
		want     string
	}{
		{
			name:     "route wins verbatim",
			route:    "/users/{userId}/orders/{orderId}",
			urlPath:  "/users/1234/orders/987",
			spanName: "GET /users/1234/orders/987",
			want:     "/users/{userId}/orders/{orderId}",
		},
		{
			name:     "route wins even when it looks like an id",
			route:    "/1234",
			urlPath:  "/users/1234",
			spanName: "GET /users/1234",
			want:     "/1234",
		},
		{
			name:     "url path when no route",
			urlPath:  "/users/1234/orders/987",
			spanName: "GET /users/1234/orders/987",
			want:     "/users/{id}/orders/{id}",
		},
		{
			name:     "http target with query when no route",
			urlPath:  "/users/1234?expand=orders",
			spanName: "GET /users/1234",
			want:     "/users/{id}",
		},
		{
			name:     "span name when neither",
			spanName: "GET /users/1234",
			want:     "GET /users/{id}",
		},
		{
			name:     "non-http span name passes through",
			spanName: "checkout.reserveInventory",
			want:     "checkout.reserveInventory",
		},
		{name: "all empty", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeOperation(tc.route, tc.urlPath, tc.spanName)
			if got != tc.want {
				t.Fatalf("NormalizeOperation(%q, %q, %q) = %q, want %q",
					tc.route, tc.urlPath, tc.spanName, got, tc.want)
			}
		})
	}
}

func TestNormalizePathUnchangedPathDoesNotAllocate(t *testing.T) {
	const in = "/api/v1/checkout/reserve"
	allocs := testing.AllocsPerRun(200, func() {
		if NormalizePath(in) != in {
			t.Fatal("static path was rewritten")
		}
	})
	if allocs != 0 {
		t.Fatalf("NormalizePath allocated %v times on an unchanged path, want 0", allocs)
	}
}

func BenchmarkNormalizePathStatic(b *testing.B) {
	const in = "/api/v1/checkout/reserve"
	b.ReportAllocs()
	for range b.N {
		if NormalizePath(in) != in {
			b.Fatal("static path was rewritten")
		}
	}
}

func BenchmarkNormalizePathRewrite(b *testing.B) {
	const in = "/users/1234/orders/550e8400-e29b-41d4-a716-446655440000?expand=items"
	b.ReportAllocs()
	for range b.N {
		if NormalizePath(in) == in {
			b.Fatal("path was not rewritten")
		}
	}
}

func BenchmarkNormalizeSpanName(b *testing.B) {
	const in = "GET /users/1234/orders/987"
	b.ReportAllocs()
	for range b.N {
		if NormalizeSpanName(in) == in {
			b.Fatal("span name was not rewritten")
		}
	}
}

func BenchmarkNormalizeOperationRoute(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		if NormalizeOperation("/users/{userId}", "/users/1234", "GET /users/1234") == "" {
			b.Fatal("empty operation")
		}
	}
}
