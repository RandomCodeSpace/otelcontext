package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
)

// hotPollCacheTTL is how long the rendered payloads of the hot polled
// endpoints (/api/system/graph, /api/metrics/dashboard, /api/stats) stay
// cached. 10s matches the UI poll cadence — steady-state polling becomes a
// map lookup plus an ETag compare instead of a SQLite query + JSON encode.
const hotPollCacheTTL = 10 * time.Second

const (
	maxCacheKeyLen    = 256
	apiCacheEntries   = 256
	apiCacheByteLimit = int64(16 << 20)
)

// cachedJSON is a fully rendered JSON response body plus its strong ETag.
// Marshalling and hashing happen once per cache fill; clients that echo
// If-None-Match then get a 304 with no body at all.
type cachedJSON struct {
	body []byte
	etag string
}

func (c *cachedJSON) CacheSizeBytes() int64 { return int64(len(c.body)) }

func newCachedJSON(v any) (*cachedJSON, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return &cachedJSON{body: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}, nil
}

// write emits the cached payload, honouring If-None-Match with a 304.
// xCache is the cache-disposition header value ("HIT" or "MISS").
func (c *cachedJSON) write(w http.ResponseWriter, r *http.Request, xCache string) {
	h := w.Header()
	h.Set("ETag", c.etag)
	h.Set("X-Cache", xCache)
	if httpconst.ETagMatch(r.Header.Get("If-None-Match"), c.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_, _ = w.Write(c.body) //nolint:gosec // G705 false positive: body is json.Marshal output of server-built values, served as application/json
}

// cachedBody preserves the service-map endpoint's existing JSON encoding and
// headers while retaining one rendered copy for cache hits and byte charging.
type cachedBody struct {
	body []byte
}

func newCachedBody(v any) (*cachedBody, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	return &cachedBody{body: b}, nil
}

func (c *cachedBody) CacheSizeBytes() int64 { return int64(len(c.body)) }

func (c *cachedBody) write(w http.ResponseWriter, xCache string) {
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	w.Header().Set("X-Cache", xCache)
	_, _ = w.Write(c.body) //nolint:gosec // G705 false positive: body is json.Marshal output of server-built values, served as application/json
}
