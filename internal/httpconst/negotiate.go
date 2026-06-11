package httpconst

import (
	"net/http"
	"strconv"
	"strings"
)

// AcceptsEncoding reports whether the request's Accept-Encoding header lists
// the given encoding with a non-zero q-value. Minimal token parse — the "*"
// wildcard is deliberately not honoured (browsers and collectors always name
// the encodings they support), so callers never have to guess a preference
// order for it.
func AcceptsEncoding(r *http.Request, enc string) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		token, attr, hasAttr := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(token), enc) {
			continue
		}
		if hasAttr {
			attr = strings.TrimSpace(attr)
			if qv, ok := strings.CutPrefix(attr, "q="); ok {
				if q, err := strconv.ParseFloat(strings.TrimSpace(qv), 64); err == nil && q == 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

// ETagMatch reports whether an If-None-Match header value matches the given
// entity tag. Handles the "*" wildcard, comma-separated candidate lists, and
// weak validators (W/ prefix is ignored — weak comparison is correct for
// If-None-Match per RFC 9110 §13.1.2).
func ETagMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
