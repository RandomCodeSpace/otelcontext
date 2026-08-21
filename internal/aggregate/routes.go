package aggregate

import (
	"strings"
	"unicode/utf8"
)

// IDPlaceholder replaces a path segment the segment rules classify as variable.
const IDPlaceholder = "{id}"

// Segment-rule thresholds from #159.
const (
	// minHexSegment is the length at which an all-hex segment is an ID.
	minHexSegment = 8
	// minBase64Segment is the length at which a base64-alphabet segment with
	// mixed character classes is an ID.
	minBase64Segment = 16
	// uuidLen is the length of a canonical dashed UUID.
	uuidLen = 36
)

// NormalizeOperation resolves the operation name of an HTTP span using the
// precedence fixed in #159:
//
//  1. http.route verbatim when present — the instrumentation already told us
//     the template, and second-guessing it can only make things worse.
//  2. otherwise url.path / http.target, normalized.
//  3. otherwise the span name, normalized only if it is shaped "<METHOD> /path".
//
// Nothing here learns or infers: the same inputs always produce the same
// output, on every process and every restart.
func NormalizeOperation(httpRoute, urlPath, spanName string) string {
	if httpRoute != "" {
		return httpRoute
	}
	if urlPath != "" {
		return NormalizePath(urlPath)
	}
	return NormalizeSpanName(spanName)
}

// NormalizePath normalizes a genuine URL path value (url.path or http.target).
// The query string and fragment are stripped first, then each segment is tested
// against the segment rules and replaced with IDPlaceholder when it matches.
//
// Input that is not a path — invalid UTF-8, or no leading '/' once the query
// and fragment are gone — is returned verbatim, query included. It is somebody
// else's string and the per-service operation cap will catch it if it is
// pathological.
func NormalizePath(path string) string {
	if path == "" {
		return path
	}
	if !utf8.ValidString(path) {
		return path
	}
	stripped := stripQueryFragment(path)
	if !strings.HasPrefix(stripped, "/") {
		return path
	}
	if out, ok := normalizeSegments("", stripped); ok {
		return out
	}
	return stripped
}

// NormalizeSpanName normalizes a span name shaped "<METHOD> /path", which is
// what OTel HTTP instrumentation emits when it has no route template. Names of
// any other shape — including anything whose first token is not a known HTTP
// method — pass through verbatim; the URL rules must never be let loose on
// arbitrary span names.
func NormalizeSpanName(name string) string {
	if name == "" {
		return name
	}
	if !utf8.ValidString(name) {
		return name
	}
	sp := strings.IndexByte(name, ' ')
	if sp <= 0 {
		return name
	}
	if _, ok := LookupMethod(name[:sp]); !ok {
		return name
	}
	path := name[sp+1:]
	stripped := stripQueryFragment(path)
	if !strings.HasPrefix(stripped, "/") {
		return name
	}
	// The method token and its separating space are carried into the builder so
	// a rewritten span name costs one allocation, not two.
	if out, ok := normalizeSegments(name[:sp+1], stripped); ok {
		return out
	}
	if len(stripped) != len(path) {
		return name[:sp+1] + stripped
	}
	return name
}

// stripQueryFragment cuts s at the first '?' or '#'.
func stripQueryFragment(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}

// normalizeSegments replaces every variable segment of p, which must start with
// '/', and returns prefix+result. It reports ok=false — having allocated
// nothing at all — when no segment matched, which is the common case for a
// service with real route templates; the caller then decides what to return.
func normalizeSegments(prefix, p string) (string, bool) {
	var b strings.Builder
	building := false
	written := 0 // bytes of p already copied into b
	pos := 1     // start of the current segment
	for pos <= len(p) {
		end := len(p)
		if j := strings.IndexByte(p[pos:], '/'); j >= 0 {
			end = pos + j
		}
		if isVariableSegment(p[pos:end]) {
			if !building {
				b.Grow(len(prefix) + len(p) + len(IDPlaceholder))
				b.WriteString(prefix)
				building = true
			}
			b.WriteString(p[written:pos])
			b.WriteString(IDPlaceholder)
			written = end
		}
		pos = end + 1
	}
	if !building {
		return "", false
	}
	b.WriteString(p[written:])
	return b.String(), true
}

// isVariableSegment applies the #159 segment rules in cost order. No regular
// expressions: these are byte scans that the compiler keeps on the stack.
func isVariableSegment(s string) bool {
	if s == "" {
		return false
	}
	if isAllDigits(s) {
		return true
	}
	if isDashedUUID(s) {
		return true
	}
	if len(s) >= minHexSegment && isAllHex(s) {
		return true
	}
	return len(s) >= minBase64Segment && isBase64Like(s)
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isAllHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

// isDashedUUID matches the canonical 8-4-4-4-12 form. The undashed 32-hex form
// is already covered by the >=8-hex rule.
func isDashedUUID(s string) bool {
	if len(s) != uuidLen {
		return false
	}
	for i := 0; i < uuidLen; i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(s[i]) {
				return false
			}
		}
	}
	return true
}

// isBase64Like matches a long segment drawn entirely from the base64 (standard
// or URL-safe) alphabet that also mixes digits, upper case and lower case.
//
// The mixed-class requirement is the guard against eating real route words: a
// sixteen-character lowercase segment like "administration/" or a CamelCase
// handler name without digits stays verbatim. It is not free of false
// positives — a long CamelCase segment containing a digit will be collapsed —
// but it is deterministic, and the alternative of entropy scoring is exactly
// the kind of learned behaviour #159 rules out.
func isBase64Like(s string) bool {
	var hasDigit, hasUpper, hasLower bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c == '+' || c == '/' || c == '=' || c == '-' || c == '_':
			// Alphabet padding and URL-safe substitutions; carries no class.
		default:
			return false
		}
	}
	return hasDigit && hasUpper && hasLower
}
