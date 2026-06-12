package graphrag

// Drain — fixed-depth prefix tree log template miner.
//
// Clean-room Go implementation of the Drain algorithm (He et al., 2017,
// "Drain: An Online Log Parsing Approach with Fixed Depth Tree").
//
// Pipeline:
//  1. Preprocess — mask obvious variables via regex (IP, UUID, hex, NUM, TS,
//     EMAIL) before tokenization. URLs/paths kept intact — they frequently
//     differentiate templates.
//  2. Length layer — bucket logs by token count.
//  3. Prefix tree — fixed depth (default 4). Each level descends by the
//     token at that position, with a fallback to a generalized "<*>" child.
//  4. Leaf — each leaf holds a list of log groups (templates). Compute
//     token-position match ratio simSeq = matches / N. If max >= st (default
//     0.4) merge into that template (replacing mismatches with <*>), else
//     create a new group.
//
// Thread-safe via sync.RWMutex. Bounded total templates via LRU eviction on
// LastSeen.
//
// Pure stdlib — no third-party deps, no CGO.

import (
	"container/list"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// --- Drain preprocessing regexes ---

var (
	// Order matters: timestamps first (they contain digits), UUID before hex,
	// hex before NUM.
	drainReTimestampISO = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`)
	drainReTimestampMs  = regexp.MustCompile(`\b1[0-9]{12}\b`) // Unix ms (~2001..2286)
	drainReEmail        = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	drainReIPv4         = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(?::\d+)?\b`)
	drainReIPv6         = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`)
	drainReUUID         = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	drainReHex0x        = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	drainReHexLong      = regexp.MustCompile(`\b[0-9a-fA-F]{16,}\b`)
	// No leading \b so a minus sign preceded by whitespace (" -1 ") is captured:
	// Go's \b only matches between word/non-word characters, and both " " and "-"
	// are non-word, so a bare \b-? misses the minus. Earlier masks consume IPs,
	// UUIDs, hex, timestamps, and emails so a greedy \d+ has nothing left to
	// over-match.
	drainReNum = regexp.MustCompile(`-?\d+(?:\.\d+)?\b`)
)

// Wildcard is the placeholder used for variable positions within a template.
const Wildcard = "<*>"

// Preprocess masks common variable patterns in a raw log line prior to
// tokenization. This implements Drain's configurable regex-replacement stage.
func Preprocess(line string) string {
	line = drainReTimestampISO.ReplaceAllString(line, "<TS>")
	line = drainReTimestampMs.ReplaceAllString(line, "<TS>")
	line = drainReEmail.ReplaceAllString(line, "<EMAIL>")
	line = drainReUUID.ReplaceAllString(line, "<UUID>")
	line = drainReIPv4.ReplaceAllString(line, "<IP>")
	line = drainReIPv6.ReplaceAllString(line, "<IP>")
	line = drainReHex0x.ReplaceAllString(line, "<HEX>")
	line = drainReHexLong.ReplaceAllString(line, "<HEX>")
	line = drainReNum.ReplaceAllString(line, "<NUM>")
	return line
}

// tokenize splits a preprocessed log line on whitespace.
func tokenize(line string) []string {
	return strings.Fields(line)
}

// --- Template ---

// Template represents a mined log group.
type Template struct {
	ID        uint64    `json:"id"`
	Tokens    []string  `json:"tokens"`
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Sample    string    `json:"sample,omitempty"`

	// elem is the template's position in the Drain LRU list. Never serialized.
	elem *list.Element `json:"-"`
	// leaf is a back-pointer to the owning prefix-tree leaf for O(1) eviction.
	// Never serialized; rebuilt via descend() on LoadTemplates.
	leaf *drainNode `json:"-"`
}

// TemplateString returns the template rendered as a single string.
func (t *Template) TemplateString() string {
	return strings.Join(t.Tokens, " ")
}

// templateID computes a stable FNV-64 hash of the token sequence.
func templateID(tokens []string) uint64 {
	h := fnv.New64a()
	for i, tok := range tokens {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(tok))
	}
	return h.Sum64()
}

// --- Prefix tree nodes ---

// drainNode is an internal prefix-tree node or a leaf holding templates.
type drainNode struct {
	// Internal: children keyed by token at this depth; Wildcard used as
	// generalized fallback.
	children map[string]*drainNode

	// Leaf: list of templates sharing the same length + prefix path.
	groups []*Template
}

func newDrainNode() *drainNode {
	return &drainNode{children: make(map[string]*drainNode)}
}

// --- Drain ---

// Drain is a thread-safe log template miner.
type Drain struct {
	mu sync.RWMutex

	depth        int     // maximum prefix-tree depth (levels below the length layer)
	similarityTh float64 // similarity threshold (0..1)
	maxChildren  int     // per-node child cap — additional token kinds collapse to Wildcard
	maxTemplates int     // total template cap; LRU eviction on LastSeen

	// Length-layer root: length -> prefix tree root.
	byLen map[int]*drainNode

	// Fast lookup for existing templates (ID -> template).
	templates map[uint64]*Template

	// lru tracks template recency. Front = most recent, Back = eviction victim.
	// Each Element.Value is a *Template (with Template.elem pointing here).
	lru *list.List
}

// DrainOption configures a Drain instance.
type DrainOption func(*Drain)

// WithDepth sets the prefix tree depth (default 4). Minimum 2.
func WithDepth(depth int) DrainOption {
	return func(d *Drain) {
		if depth >= 2 {
			d.depth = depth
		}
	}
}

// WithSimilarityThreshold sets st (default 0.4). Clamped to (0, 1].
func WithSimilarityThreshold(st float64) DrainOption {
	return func(d *Drain) {
		if st > 0 && st <= 1 {
			d.similarityTh = st
		}
	}
}

// WithMaxChildren sets the max distinct children per internal node
// (default 100). Beyond this, tokens collapse to Wildcard.
func WithMaxChildren(n int) DrainOption {
	return func(d *Drain) {
		if n > 0 {
			d.maxChildren = n
		}
	}
}

// WithMaxTemplates sets the total template cap (default 50000).
// When exceeded, the least-recently-seen template is evicted.
func WithMaxTemplates(n int) DrainOption {
	return func(d *Drain) {
		if n > 0 {
			d.maxTemplates = n
		}
	}
}

// NewDrain constructs a Drain miner with the supplied options.
func NewDrain(opts ...DrainOption) *Drain {
	d := &Drain{
		depth:        4,
		similarityTh: 0.4,
		maxChildren:  100,
		maxTemplates: 50000,
		byLen:        make(map[int]*drainNode),
		templates:    make(map[uint64]*Template),
		lru:          list.New(),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// TemplateCount reports the number of live templates under the read lock.
func (d *Drain) TemplateCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.templates)
}

// Match preprocesses the log line, descends the prefix tree, and either
// merges into an existing template or creates a new one. Returns the
// resulting template. Returns nil only for empty input.
func (d *Drain) Match(logLine string, ts time.Time) *Template {
	// Preprocessing and tokenization are pure functions over immutable inputs
	// — hoist them out of the lock so CPU-bound regex work runs in parallel
	// across ingestion workers. Only tree mutation + template bookkeeping
	// needs the write lock.
	masked := Preprocess(logLine)
	tokens := tokenize(masked)
	if len(tokens) == 0 {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	leaf := d.descend(tokens)
	tpl := d.matchOrCreate(leaf, tokens, logLine, ts)
	d.evictIfNeeded()
	return tpl
}

// descend walks the length layer + prefix tree, creating nodes as needed,
// honoring the maxChildren cap by collapsing overflow tokens to Wildcard.
func (d *Drain) descend(tokens []string) *drainNode {
	n := len(tokens)
	root, ok := d.byLen[n]
	if !ok {
		root = newDrainNode()
		d.byLen[n] = root
	}

	// Effective prefix depth is min(depth, n).
	effDepth := d.depth
	if effDepth > n {
		effDepth = n
	}

	cur := root
	for i := 0; i < effDepth; i++ {
		tok := tokens[i]
		// If a wildcard child already exists and token is not literal-known,
		// we still prefer an exact match first; fall back to Wildcard.
		if child, ok := cur.children[tok]; ok {
			cur = child
			continue
		}
		if tok == Wildcard {
			// The token itself is a wildcard; descend via Wildcard child.
			cur = getOrCreateChild(cur, Wildcard)
			continue
		}
		if len(cur.children) >= d.maxChildren {
			// Cap hit: route via Wildcard child.
			cur = getOrCreateChild(cur, Wildcard)
			continue
		}
		cur = getOrCreateChild(cur, tok)
	}
	return cur
}

func getOrCreateChild(n *drainNode, key string) *drainNode {
	if c, ok := n.children[key]; ok {
		return c
	}
	c := newDrainNode()
	n.children[key] = c
	return c
}

// matchOrCreate finds the best-matching template at the leaf (by
// token-position match ratio). If max similarity >= st, merge; else create.
func (d *Drain) matchOrCreate(leaf *drainNode, tokens []string, raw string, ts time.Time) *Template {
	var best *Template
	bestSim := -1.0
	for _, g := range leaf.groups {
		if len(g.Tokens) != len(tokens) {
			continue
		}
		sim := tokenSimilarity(g.Tokens, tokens)
		if sim > bestSim {
			bestSim = sim
			best = g
		}
	}

	if best != nil && bestSim >= d.similarityTh {
		// Merge: replace mismatched positions with Wildcard (never replace a
		// Wildcard with a literal).
		changed := false
		for i, tok := range tokens {
			if best.Tokens[i] == Wildcard {
				continue
			}
			if best.Tokens[i] != tok {
				best.Tokens[i] = Wildcard
				changed = true
			}
		}
		if changed {
			// Re-hash: a merged template has a new identity. Keep Count,
			// FirstSeen, Sample; update ID and registry.
			oldID := best.ID
			newID := templateID(best.Tokens)
			delete(d.templates, oldID)

			// Collision: after generalization the new token sequence may hash
			// to an existing live template. Fold `best` into the existing one
			// to avoid clobbering; this also prevents leaking `best` as an
			// orphan in its leaf's groups slice and in the LRU list.
			if existing, ok := d.templates[newID]; ok && existing != best {
				existing.Count += best.Count
				if best.LastSeen.After(existing.LastSeen) {
					existing.LastSeen = best.LastSeen
				}
				if best.FirstSeen.Before(existing.FirstSeen) {
					existing.FirstSeen = best.FirstSeen
				}
				// Remove `best` from its leaf's groups and from the LRU.
				if best.leaf != nil {
					removeTplFromLeaf(best.leaf, best)
				}
				if best.elem != nil {
					d.lru.Remove(best.elem)
					best.elem = nil
				}
				existing.Count++
				existing.LastSeen = ts
				if existing.elem != nil {
					d.lru.MoveToFront(existing.elem)
				}
				return existing
			}

			best.ID = newID
			d.templates[best.ID] = best
		}
		best.Count++
		best.LastSeen = ts
		if best.elem != nil {
			d.lru.MoveToFront(best.elem)
		}
		return best
	}

	// Create new template.
	tks := make([]string, len(tokens))
	copy(tks, tokens)
	tpl := &Template{
		ID:        templateID(tks),
		Tokens:    tks,
		Count:     1,
		FirstSeen: ts,
		LastSeen:  ts,
		Sample:    raw,
		leaf:      leaf,
	}
	leaf.groups = append(leaf.groups, tpl)
	d.templates[tpl.ID] = tpl
	tpl.elem = d.lru.PushFront(tpl)
	return tpl
}

// tokenSimilarity computes the Drain simSeq: matching positions / N.
// Wildcard positions in the template count as matches (they already subsume).
func tokenSimilarity(template, tokens []string) float64 {
	if len(template) != len(tokens) || len(tokens) == 0 {
		return 0
	}
	matches := 0
	for i, t := range template {
		if t == Wildcard || t == tokens[i] {
			matches++
		}
	}
	return float64(matches) / float64(len(tokens))
}

// evictIfNeeded drops the least-recently-seen template when over capacity.
// O(1) victim lookup via container/list; O(M) removal from the victim's
// leaf.groups slice where M is templates-per-leaf (typically small).
func (d *Drain) evictIfNeeded() {
	if len(d.templates) <= d.maxTemplates {
		return
	}
	back := d.lru.Back()
	if back == nil {
		return
	}
	victim, ok := back.Value.(*Template)
	if !ok || victim == nil {
		return
	}
	d.lru.Remove(back)
	victim.elem = nil
	delete(d.templates, victim.ID)
	if victim.leaf != nil {
		removeTplFromLeaf(victim.leaf, victim)
		victim.leaf = nil
	}
}

// removeTplFromLeaf strips the victim template from a specific leaf's groups
// slice. Leaves are typically small (tens of templates).
func removeTplFromLeaf(n *drainNode, victim *Template) {
	if n == nil || len(n.groups) == 0 {
		return
	}
	out := n.groups[:0]
	for _, g := range n.groups {
		if g != victim {
			out = append(out, g)
		}
	}
	n.groups = out
}

// Templates returns a snapshot copy of all currently known templates.
// Internal back-pointers (elem, leaf) are deliberately omitted from copies.
func (d *Drain) Templates() []Template {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Template, 0, len(d.templates))
	for _, t := range d.templates {
		out = append(out, Template{
			ID:        t.ID,
			Tokens:    append([]string(nil), t.Tokens...),
			Count:     t.Count,
			FirstSeen: t.FirstSeen,
			LastSeen:  t.LastSeen,
			Sample:    t.Sample,
		})
	}
	return out
}

// LoadTemplates restores templates from a previous snapshot. Existing state
// is cleared. Intended for startup recovery. The LRU list is rebuilt by
// inserting templates in LastSeen order (oldest first, each PushFront'd),
// so the most recently-seen template ends up at the front.
func (d *Drain) LoadTemplates(tpls []Template) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byLen = make(map[int]*drainNode)
	d.templates = make(map[uint64]*Template)
	d.lru = list.New()

	sorted := make([]Template, 0, len(tpls))
	for _, t := range tpls {
		if len(t.Tokens) == 0 {
			continue
		}
		sorted = append(sorted, t)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].LastSeen.Before(sorted[j].LastSeen)
	})

	for i := range sorted {
		t := sorted[i]
		tokens := append([]string(nil), t.Tokens...)
		tpl := &Template{
			ID:        t.ID,
			Tokens:    tokens,
			Count:     t.Count,
			FirstSeen: t.FirstSeen,
			LastSeen:  t.LastSeen,
			Sample:    t.Sample,
		}
		if tpl.ID == 0 {
			tpl.ID = templateID(tpl.Tokens)
		}
		leaf := d.descend(tokens)
		leaf.groups = append(leaf.groups, tpl)
		tpl.leaf = leaf
		d.templates[tpl.ID] = tpl
		tpl.elem = d.lru.PushFront(tpl)
	}
}

// Len returns the number of live templates.
func (d *Drain) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.templates)
}

// --- Persistence ---

// SaveDrainTemplates upserts the given templates into the drain_templates
// table under the supplied tenant. Tokens are JSON-encoded. Uses GORM's
// clause.OnConflict upsert which is dialect-aware (ON CONFLICT for SQLite/
// PostgreSQL, ON DUPLICATE KEY UPDATE for MySQL).
//
// The conflict target is the composite (tenant_id, id) primary key so the
// same Drain template hash can coexist across tenants — a future per-tenant
// Drain miner can rely on this to keep cluster IDs stable per tenant.
func SaveDrainTemplates(db *gorm.DB, tenant string, templates []Template) error {
	if db == nil || len(templates) == 0 {
		return nil
	}
	if tenant == "" {
		tenant = storage.DefaultTenantID
	}
	rows := make([]DrainTemplateRow, 0, len(templates))
	for _, t := range templates {
		tokensJSON, err := json.Marshal(t.Tokens)
		if err != nil {
			return fmt.Errorf("marshal drain tokens id=%d: %w", t.ID, err)
		}
		rows = append(rows, DrainTemplateRow{
			TenantID:  tenant,
			ID:        int64(t.ID), //nolint:gosec // intentional bit-reinterpret of FNV-64 hash for DB portability
			Tokens:    string(tokensJSON),
			Count:     t.Count,
			FirstSeen: t.FirstSeen,
			LastSeen:  t.LastSeen,
			Sample:    t.Sample,
		})
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"tokens", "count", "last_seen", "sample",
		}),
	}).CreateInBatches(&rows, 500).Error
}

// LoadDrainTemplates reads persisted Drain templates for the supplied tenant
// and returns them in a format ready to pass to Drain.LoadTemplates. Returns
// an empty slice (and nil error) if no rows match.
func LoadDrainTemplates(db *gorm.DB, tenant string) ([]Template, error) {
	if db == nil {
		return nil, nil
	}
	if tenant == "" {
		tenant = storage.DefaultTenantID
	}
	var rows []DrainTemplateRow
	if err := db.Where("tenant_id = ?", tenant).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(rows))
	for _, r := range rows {
		var tokens []string
		if err := json.Unmarshal([]byte(r.Tokens), &tokens); err != nil {
			// Skip malformed rows rather than aborting the whole load.
			continue
		}
		if len(tokens) == 0 {
			continue
		}
		out = append(out, Template{
			ID:        uint64(r.ID), //nolint:gosec // reverse bit-reinterpret — see DrainTemplateRow doc
			Tokens:    tokens,
			Count:     r.Count,
			FirstSeen: r.FirstSeen,
			LastSeen:  r.LastSeen,
			Sample:    r.Sample,
		})
	}
	return out, nil
}
