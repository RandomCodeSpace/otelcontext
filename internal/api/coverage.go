package api

import (
	"net/http"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
)

// setCoverage stamps the data-coverage header on a response.
//
// Endpoints that return a bare JSON array cannot carry an additive "coverage"
// field without changing their shape, and silently wrapping them in an
// envelope would break every existing client. The header is how those
// endpoints stay honest without breaking (#164).
func setCoverage(w http.ResponseWriter, c aggregate.Coverage) {
	if c == "" {
		return
	}
	w.Header().Set(aggregate.CoverageHeader, string(c))
}
