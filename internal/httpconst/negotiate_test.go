package httpconst

import (
	"net/http/httptest"
	"testing"
)

func TestAcceptsEncoding(t *testing.T) {
	tests := []struct {
		name   string
		header string
		enc    string
		want   bool
	}{
		{"empty header", "", "gzip", false},
		{"plain match", "gzip", "gzip", true},
		{"case insensitive", "GZip", "gzip", true},
		{"list match", "deflate, gzip, br", "gzip", true},
		{"list match br", "gzip, br", "br", true},
		{"no match", "deflate", "gzip", false},
		{"substring is not a token", "xgzipx", "gzip", false},
		{"q value accepted", "gzip;q=0.5", "gzip", true},
		{"q zero rejected", "gzip;q=0", "gzip", false},
		{"q zero decimal rejected", "gzip;q=0.000", "gzip", false},
		{"q zero in list", "br;q=0, gzip", "br", false},
		{"whitespace tolerated", " gzip ; q=1.0 , br", "gzip", true},
		{"wildcard not honoured", "*", "gzip", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tt.header != "" {
				r.Header.Set("Accept-Encoding", tt.header)
			}
			if got := AcceptsEncoding(r, tt.enc); got != tt.want {
				t.Errorf("AcceptsEncoding(%q, %q) = %v, want %v", tt.header, tt.enc, got, tt.want)
			}
		})
	}
}

func TestETagMatch(t *testing.T) {
	const etag = `"abc123"`
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{"empty header", "", false},
		{"exact match", `"abc123"`, true},
		{"no match", `"zzz"`, false},
		{"wildcard", "*", true},
		{"list match", `"zzz", "abc123"`, true},
		{"weak validator match", `W/"abc123"`, true},
		{"unquoted does not match", "abc123", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ETagMatch(tt.header, etag); got != tt.want {
				t.Errorf("ETagMatch(%q, %q) = %v, want %v", tt.header, etag, got, tt.want)
			}
		})
	}
}
