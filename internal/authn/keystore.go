package authn

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gopkg.in/yaml.v3"
)

// MaxKeyFileBytes bounds how much of an operator-supplied key file is read.
// 1 MiB is ~20k entries; anything larger is a misconfiguration, not a
// deployment.
const MaxKeyFileBytes = 1 << 20

// keyEntry is one loaded credential. The key material itself is never stored:
// only its SHA-256 digest, so a heap dump or a stray %+v cannot leak a usable
// bearer token.
type keyEntry struct {
	digest [sha256.Size]byte
	tenant string
}

// KeyStore maps bearer keys to tenants using digest-only storage and
// constant-time comparison. A nil or empty store is disabled and every Lookup
// misses, which is what keeps the default (no keys file) deployment on the
// legacy shared-API_KEY path.
type KeyStore struct {
	entries []keyEntry
	tenants []string // sorted, unique — for logging counts only
}

// LoadKeyStore reads API_TENANT_KEYS_FILE. Format is chosen by extension:
// `.json` for JSON, `.yaml`/`.yml` for YAML. Both carry the same shape — an
// object mapping bearer key to tenant ID:
//
//	{"3f7c…": "acme", "9b21…": "acme", "c40d…": "beta"}
//
// Multiple keys may map to the same tenant (rotation, per-agent keys).
// The load is startup-only; there is no reload path by design, so a key file
// swap is an explicit restart.
//
// Refused: unreadable files, files readable or writable by group/other,
// unknown extensions, empty files, empty keys, keys carrying whitespace or
// control characters, tenant IDs the storage sanitizer rejects, and duplicate
// keys. Errors never quote key material.
func LoadKeyStore(path string) (*KeyStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	var pairs []pair
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".json":
		pairs, err = decodeJSONPairs(raw)
	case ".yaml", ".yml":
		pairs, err = decodeYAMLPairs(raw)
	default:
		return nil, fmt.Errorf("tenant keys file %q: unsupported extension %q (want .json, .yaml, or .yml)", path, ext)
	}
	if err != nil {
		return nil, fmt.Errorf("tenant keys file %q: %w", path, err)
	}
	store, err := newKeyStore(pairs)
	if err != nil {
		return nil, fmt.Errorf("tenant keys file %q: %w", path, err)
	}
	return store, nil
}

// pair is one key→tenant definition carrying its source position so errors can
// point at a line without quoting the credential.
type pair struct {
	key    string
	tenant string
	where  string // "entry 3" or "line 7"
}

// readKeyFile enforces the file-permission contract before reading a byte.
// Anything group- or world-accessible is refused with the actual mode so the
// operator can see what to chmod.
func readKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("tenant keys file %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("tenant keys file %q: %w", path, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("tenant keys file %q: is a directory", path)
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("tenant keys file %q: permissions %04o are too permissive — group/other access must be removed (chmod 600)", path, mode)
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("tenant keys file %q: %w", path, err)
	}
	if len(raw) > MaxKeyFileBytes {
		return nil, fmt.Errorf("tenant keys file %q: larger than %d bytes", path, MaxKeyFileBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("tenant keys file %q: file is empty", path)
	}
	return raw, nil
}

// decodeJSONPairs streams the object so duplicate keys are caught rather than
// silently last-write-wins as encoding/json would otherwise do.
func decodeJSONPairs(raw []byte) ([]pair, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("invalid JSON: want an object of \"key\": \"tenant\" pairs")
	}
	var out []pair
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		k, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("invalid JSON: non-string key")
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("invalid JSON at entry %d: %w", len(out)+1, err)
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("entry %d: tenant must be a string", len(out)+1)
		}
		out = append(out, pair{key: k, tenant: s, where: fmt.Sprintf("entry %d", len(out)+1)})
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return out, nil
}

// decodeYAMLPairs walks the document node-by-node for the same reason:
// yaml.v3 rejects duplicate keys only in strict mode on some shapes, and the
// node walk also gives line numbers for free.
func decodeYAMLPairs(raw []byte) ([]pair, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML: empty document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid YAML: want a mapping of key: tenant pairs")
	}
	out := make([]pair, 0, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Kind != yaml.ScalarNode || v.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: key and tenant must both be scalars", k.Line)
		}
		out = append(out, pair{key: k.Value, tenant: v.Value, where: fmt.Sprintf("line %d", k.Line)})
	}
	return out, nil
}

// newKeyStore validates and digests a set of key→tenant definitions. Unexported
// because the pair type is internal; callers holding an already-parsed mapping
// go through NewKeyStoreFromMap and get exactly the same validation.
func newKeyStore(pairs []pair) (*KeyStore, error) {
	if len(pairs) == 0 {
		return nil, fmt.Errorf("no entries found")
	}
	s := &KeyStore{entries: make([]keyEntry, 0, len(pairs))}
	seen := make(map[[sha256.Size]byte]struct{}, len(pairs))
	tenants := make(map[string]struct{}, len(pairs))
	for _, p := range pairs {
		if err := validateKey(p.key); err != nil {
			return nil, fmt.Errorf("%s: %w", p.where, err)
		}
		tenant := storage.SanitizeTenantID(p.tenant)
		if tenant == "" || tenant != strings.TrimSpace(p.tenant) {
			return nil, fmt.Errorf("%s: invalid tenant ID (empty, over %d bytes, or contains control characters)", p.where, storage.MaxTenantIDLength)
		}
		digest := sha256.Sum256([]byte(p.key))
		if _, dup := seen[digest]; dup {
			return nil, fmt.Errorf("%s: duplicate key", p.where)
		}
		seen[digest] = struct{}{}
		tenants[tenant] = struct{}{}
		s.entries = append(s.entries, keyEntry{digest: digest, tenant: tenant})
	}
	s.tenants = make([]string, 0, len(tenants))
	for t := range tenants {
		s.tenants = append(s.tenants, t)
	}
	sort.Strings(s.tenants)
	return s, nil
}

// NewKeyStoreFromMap builds a store from an in-memory mapping. Iteration order
// of a Go map is random, so entries are sorted by tenant then key digest to
// keep error messages deterministic.
func NewKeyStoreFromMap(m map[string]string) (*KeyStore, error) {
	pairs := make([]pair, 0, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		pairs = append(pairs, pair{key: k, tenant: m[k], where: fmt.Sprintf("entry %d", i+1)})
	}
	return newKeyStore(pairs)
}

// validateKey rejects credentials that cannot be transported safely or that
// are obviously unset. Whitespace and control characters are refused because
// they cannot survive an HTTP header or a gRPC metadata value intact, so they
// would authenticate in tests and fail in production.
func validateKey(k string) error {
	if k == "" {
		return fmt.Errorf("empty key")
	}
	for _, r := range k {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("key contains whitespace or control characters")
		}
	}
	return nil
}

// Enabled reports whether any tenant key is configured.
func (s *KeyStore) Enabled() bool { return s != nil && len(s.entries) > 0 }

// Len is the number of configured keys.
func (s *KeyStore) Len() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}

// Tenants returns the sorted, unique tenants covered by the store. Safe to
// log: tenant IDs are not credentials.
func (s *KeyStore) Tenants() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.tenants))
	copy(out, s.tenants)
	return out
}

// Lookup returns the tenant bound to key. The presented key is digested and
// compared against every stored digest with subtle.ConstantTimeCompare, and
// the scan never exits early, so neither the match position nor a partial
// prefix match is observable through timing.
func (s *KeyStore) Lookup(key string) (string, bool) {
	if !s.Enabled() || key == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(key))
	var (
		tenant  string
		matched int
	)
	for i := range s.entries {
		eq := subtle.ConstantTimeCompare(digest[:], s.entries[i].digest[:])
		if eq == 1 {
			tenant = s.entries[i].tenant
			matched = 1
		}
	}
	if matched != 1 {
		return "", false
	}
	return tenant, true
}
