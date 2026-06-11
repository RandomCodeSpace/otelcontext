// Package httpconst centralises HTTP header names and content-type strings
// shared by the API, MCP, and OTLP-HTTP handlers so the same literal isn't
// duplicated across packages.
package httpconst

const (
	// HeaderContentType is the canonical HTTP Content-Type header name.
	HeaderContentType = "Content-Type"

	// HeaderContentEncoding is the canonical HTTP Content-Encoding header
	// name, shared by the UI asset server and the API gzip middleware.
	HeaderContentEncoding = "Content-Encoding"

	// ContentTypeJSON is the application/json content type used by every JSON
	// response on the API and MCP surface.
	ContentTypeJSON = "application/json"
)
