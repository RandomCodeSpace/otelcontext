package aggregate

// TemplateMiner — the ingest-owned log template miner (issue #163).
//
// Drain-style fixed-depth prefix-tree clustering (He et al., 2017), rewritten
// here with aggregate identity semantics. It is NOT the GraphRAG miner: legacy
// mode keeps `internal/graphrag`'s Drain untouched, shadow and aggregate modes
// use this one so two template ID spaces never exist.
//
// What differs from the GraphRAG implementation:
//
//   - Immutable surrogate IDs. GraphRAG rehashes Template.ID whenever tokens
//     generalize; that is fine for best-effort clustering and unusable as
//     seven-day bucket identity. Here an ID is minted once through a
//     TemplateRegistrar (dictionary kind log_template) and never changes. The
//     template TEXT evolves under a stable ID.
//   - Partitioned by (tenant, service). Each partition owns an independent
//     tree and its own mutex: no global write lock on the hot path and no
//     cross-service template mutation.
//   - Bounded per #158. At most MaxTemplatesPerService patterns per partition;
//     past the cap, lines route to the partition's pre-created __other__
//     template so totals survive while identity detail collapses.
//   - Synchronous and bounded-latency. Mine() runs on the reducer hot path:
//     no regexp, no channels, no background goroutines, token count capped.
//
// Pure stdlib. Never returns an error, never blocks on anything but its own
// partition mutex.
//
// Note on ID values: the miner guarantees a deterministic per-partition
// sequence of template registrations for a given input stream. The absolute
// uint32 values come from the registrar (in Phase 2, a database sequence), so
// they depend on that allocator's ordering, not on the miner.

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// TemplateWildcard is the placeholder token for a generalized position.
	TemplateWildcard = "<*>"

	// TemplateOther is the rendered text of a partition's overflow template.
	TemplateOther = "__other__"

	// DefaultMaxLogTemplatesPerService is the #158 per-service log-template
	// cap, wired to AGGREGATE_MAX_LOG_TEMPLATES_PER_SERVICE.
	DefaultMaxLogTemplatesPerService = 10

	defaultTemplateDepth       = 4
	defaultTemplateSimilarity  = 0.4
	defaultTemplateMaxChildren = 100
	defaultTemplateMaxTokens   = 128

	tmMaskNum   = "<NUM>"
	tmMaskUUID  = "<UUID>"
	tmMaskIP    = "<IP>"
	tmMaskHex   = "<HEX>"
	tmMaskTS    = "<TS>"
	tmMaskEmail = "<EMAIL>"
	tmMaskTrunc = "<TRUNC>"

	tmMaxAliasHops = 8
)

// --- registration contract ---

// TemplateRegistration is the request handed to a TemplateRegistrar when the
// miner mints a new template identity. It mirrors the dictionary registration
// contract for kind=log_template (#159, amended by #163): the value written to
// the dictionary is a surrogate identity whose presentation text may later
// change without the ID changing.
type TemplateRegistration struct {
	Tenant   string
	Service  string
	Template string // rendered template text at registration time
	IsOther  bool   // true for a partition's pre-created __other__ identity
}

// TemplateRegistrar allocates immutable surrogate template IDs. Phase 1 uses
// an in-memory allocator; Phase 2 replaces it with the durable aggregate
// dictionary, where registration commits atomically with the first referencing
// delta.
//
// Implementations must be safe for concurrent use and bounded in latency: the
// miner calls RegisterTemplate while holding a partition lock. A returned
// error, or a zero ID, is treated as "no identity available" — the miner then
// routes the line to the partition's overflow template and never fails.
type TemplateRegistrar interface {
	RegisterTemplate(TemplateRegistration) (uint32, error)
}

// TemplateRegistrarFunc adapts a plain function to TemplateRegistrar.
type TemplateRegistrarFunc func(TemplateRegistration) (uint32, error)

// RegisterTemplate implements TemplateRegistrar.
func (f TemplateRegistrarFunc) RegisterTemplate(r TemplateRegistration) (uint32, error) {
	return f(r)
}

// inMemoryTemplateRegistrar hands out sequential IDs starting at 1. ID 0 is
// reserved for "no identity assigned".
type inMemoryTemplateRegistrar struct{ next atomic.Uint32 }

// NewInMemoryTemplateRegistrar returns the Phase 1 provisional allocator.
// IDs are process-local and do not survive a restart.
func NewInMemoryTemplateRegistrar() TemplateRegistrar { return &inMemoryTemplateRegistrar{} }

func (r *inMemoryTemplateRegistrar) RegisterTemplate(TemplateRegistration) (uint32, error) {
	return r.next.Add(1), nil
}

// --- facts ---

// TemplateFact is the per-log-line record handed to GraphRAG. GraphRAG does no
// mining of its own in shadow and aggregate modes; it consumes these.
type TemplateFact struct {
	Tenant     string
	Service    string
	Severity   string
	TemplateID uint32
	Template   string
	Timestamp  time.Time
	IsOther    bool
}

// --- configuration ---

// TemplateMinerConfig configures a TemplateMiner. Zero values take defaults.
type TemplateMinerConfig struct {
	// MaxTemplatesPerService caps distinct mined templates per
	// (tenant, service). The overflow template is reserved capacity and does
	// not count against it. Default DefaultMaxLogTemplatesPerService.
	MaxTemplatesPerService int

	// Depth is the prefix-tree depth below the token-length layer.
	// Default 4, minimum 1.
	Depth int

	// SimilarityThreshold is Drain's simSeq threshold in (0, 1]. Default 0.4.
	SimilarityThreshold float64

	// MaxChildren caps distinct children per tree node; overflow tokens route
	// through the wildcard child. Default 100.
	MaxChildren int

	// MaxTokens bounds tokens taken from one log body, keeping Mine() latency
	// independent of body size. Default 128.
	MaxTokens int

	// Registrar allocates template IDs. Default NewInMemoryTemplateRegistrar().
	Registrar TemplateRegistrar

	// OnFact, when set, is called synchronously for every mined line —
	// including overflow lines — outside the partition lock. Keep it cheap.
	OnFact func(TemplateFact)
}

// --- miner ---

// TemplateMiner mines log templates, partitioned by (tenant, service).
// The zero value is not usable; construct one with NewTemplateMiner.
type TemplateMiner struct {
	maxTemplates int
	depth        int
	similarity   float64
	maxChildren  int
	maxTokens    int

	reg TemplateRegistrar
	// onFact is swapped atomically: the engine installs GraphRAG's sink after
	// construction (the miner has to exist before the engine that owns the
	// dictionary it mints IDs from), while MineAt reads it on the hot path.
	onFact atomic.Pointer[func(TemplateFact)]

	// mu guards the partition map only. It is taken for writing exactly once
	// per new (tenant, service) pair; mining an existing partition takes it
	// for reading.
	mu    sync.RWMutex
	parts map[tmPartKey]*tmPartition

	// idxMu guards the presentation index. Lock order is always
	// partition mutex -> idxMu, never the reverse.
	idxMu sync.RWMutex
	text  map[uint32]string
	alias map[uint32]uint32

	// stageMu guards the durable-state staging maps (#200 Q4). Lock order is
	// partition mutex -> stageMu, never the reverse.
	//
	// pending holds IDENTITY-critical mutations — a new template, a pattern
	// generalization, an alias change. They ride the next group commit, so
	// they become durable in the same transaction as the delta that used the
	// identity. A periodic snapshot alone would let a crash acknowledge a
	// bucket whose NameID the reloaded miner has never heard of.
	//
	// dirty holds the non-identity counters. Losing one costs a count, not an
	// identity, so they take the cheap periodic path instead.
	stageMu sync.Mutex
	pending map[uint32]TemplateRow
	dirty   map[uint32]TemplateStatRow
}

// NewTemplateMiner builds a miner from cfg, applying defaults.
func NewTemplateMiner(cfg TemplateMinerConfig) *TemplateMiner {
	m := &TemplateMiner{
		maxTemplates: cfg.MaxTemplatesPerService,
		depth:        cfg.Depth,
		similarity:   cfg.SimilarityThreshold,
		maxChildren:  cfg.MaxChildren,
		maxTokens:    cfg.MaxTokens,
		reg:          cfg.Registrar,
		parts:        make(map[tmPartKey]*tmPartition),
		text:         make(map[uint32]string),
		alias:        make(map[uint32]uint32),
		pending:      make(map[uint32]TemplateRow),
		dirty:        make(map[uint32]TemplateStatRow),
	}
	if m.maxTemplates <= 0 {
		m.maxTemplates = DefaultMaxLogTemplatesPerService
	}
	if m.depth <= 0 {
		m.depth = defaultTemplateDepth
	}
	if m.similarity <= 0 || m.similarity > 1 {
		m.similarity = defaultTemplateSimilarity
	}
	if m.maxChildren <= 0 {
		m.maxChildren = defaultTemplateMaxChildren
	}
	if m.maxTokens <= 0 {
		m.maxTokens = defaultTemplateMaxTokens
	}
	if m.reg == nil {
		m.reg = NewInMemoryTemplateRegistrar()
	}
	m.SetFactSink(cfg.OnFact)
	return m
}

// SetFactSink installs (or, with nil, removes) the log-fact consumer. Safe to
// call while mining is in flight.
func (m *TemplateMiner) SetFactSink(fn func(TemplateFact)) {
	if fn == nil {
		m.onFact.Store(nil)
		return
	}
	m.onFact.Store(&fn)
}

// factSink returns the installed sink, or nil.
func (m *TemplateMiner) factSink() func(TemplateFact) {
	if p := m.onFact.Load(); p != nil {
		return *p
	}
	return nil
}

// Mine clusters one log body and returns its template ID. isOther is true when
// the line was absorbed by the partition's overflow template — the caller must
// still count it, only the identity detail is gone. Mine never fails; a
// registrar that cannot allocate yields (0, true).
func (m *TemplateMiner) Mine(tenant, service, severity, body string) (id uint32, isOther bool) {
	return m.MineAt(tenant, service, severity, body, time.Now())
}

// MineAt is Mine with an explicit arrival time — the reducer captures one
// timestamp per Export request and passes it for every point in that request.
func (m *TemplateMiner) MineAt(tenant, service, severity, body string, at time.Time) (id uint32, isOther bool) {
	p := m.partition(tenant, service)
	tokens := tmSplitTokens(body, m.maxTokens)

	onFact := m.factSink()
	id, isOther, text := m.mine(p, tokens, body, at, onFact != nil)

	if onFact != nil {
		onFact(TemplateFact{
			Tenant:     tenant,
			Service:    service,
			Severity:   severity,
			TemplateID: id,
			Template:   text,
			Timestamp:  at,
			IsOther:    isOther,
		})
	}
	return id, isOther
}

// TemplateText returns the current rendered text for a template ID, following
// convergence aliases. IDs issued in the past always resolve: a retired ID
// resolves to its survivor's text, never to nothing.
func (m *TemplateMiner) TemplateText(id uint32) (string, bool) {
	m.idxMu.RLock()
	defer m.idxMu.RUnlock()
	for i := 0; i < tmMaxAliasHops; i++ {
		if t, ok := m.text[id]; ok {
			return t, true
		}
		next, ok := m.alias[id]
		if !ok || next == id {
			break
		}
		id = next
	}
	return "", false
}

// TemplatePartitionStats is a point-in-time view of one (tenant, service)
// partition.
type TemplatePartitionStats struct {
	Tenant            string
	Service           string
	Templates         int    // live mined templates (excludes __other__)
	OtherID           uint32 // 0 when the overflow identity is not allocated
	Overflow          uint64 // lines absorbed by __other__
	Converged         uint64 // templates retired into a surviving twin
	RegistrarFailures uint64
}

// Stats returns per-partition statistics, sorted by tenant then service.
func (m *TemplateMiner) Stats() []TemplatePartitionStats {
	m.mu.RLock()
	parts := make([]*tmPartition, 0, len(m.parts))
	for _, p := range m.parts {
		parts = append(parts, p)
	}
	m.mu.RUnlock()

	out := make([]TemplatePartitionStats, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.stats())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// PartitionStats returns statistics for one partition, if it exists.
func (m *TemplateMiner) PartitionStats(tenant, service string) (TemplatePartitionStats, bool) {
	m.mu.RLock()
	p := m.parts[tmPartKey{tenant: tenant, service: service}]
	m.mu.RUnlock()
	if p == nil {
		return TemplatePartitionStats{}, false
	}
	return p.stats(), true
}

// --- partitions ---

type tmPartKey struct {
	tenant  string
	service string
}

type tmPartition struct {
	tenant  string
	service string

	mu        sync.Mutex
	byLen     map[int]*tmNode
	templates map[uint32]*tmTemplate
	nextSeq   uint64
	otherID   uint32

	overflow          uint64
	converged         uint64
	registrarFailures uint64
}

func (p *tmPartition) stats() TemplatePartitionStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return TemplatePartitionStats{
		Tenant:            p.tenant,
		Service:           p.service,
		Templates:         len(p.templates),
		OtherID:           p.otherID,
		Overflow:          p.overflow,
		Converged:         p.converged,
		RegistrarFailures: p.registrarFailures,
	}
}

func (m *TemplateMiner) partition(tenant, service string) *tmPartition {
	k := tmPartKey{tenant: tenant, service: service}

	m.mu.RLock()
	p := m.parts[k]
	m.mu.RUnlock()
	if p != nil {
		return p
	}

	m.mu.Lock()
	if p = m.parts[k]; p == nil {
		p = &tmPartition{
			tenant:    tenant,
			service:   service,
			byLen:     make(map[int]*tmNode),
			templates: make(map[uint32]*tmTemplate),
		}
		m.parts[k] = p
	}
	m.mu.Unlock()
	return p
}

// --- templates and tree ---

type tmTemplate struct {
	id     uint32
	seq    uint64 // partition-local creation ordinal; lower survives convergence
	tokens []string
	// version increments on every generalization of tokens. It is what makes
	// a re-offered row from a failed commit unable to roll a newer pattern
	// backwards, and what a reload uses to pick the winner (#200 Q4).
	version uint32

	count     uint64
	firstSeen time.Time
	lastSeen  time.Time
	sample    string

	leaf *tmNode
	// aliasOf, when set, makes this entry a forwarder: it stays in its leaf so
	// matching keeps working, but every match resolves to the survivor.
	aliasOf *tmTemplate
}

type tmNode struct {
	children map[string]*tmNode
	groups   []*tmTemplate
}

func newTmNode() *tmNode { return &tmNode{children: make(map[string]*tmNode)} }

// descend walks the length layer and prefix tree, creating nodes as needed and
// routing through the wildcard child once a node hits the child cap.
func (p *tmPartition) descend(tokens []string, depth, maxChildren int) *tmNode {
	n := len(tokens)
	root, ok := p.byLen[n]
	if !ok {
		root = newTmNode()
		p.byLen[n] = root
	}

	eff := depth
	if eff > n {
		eff = n
	}

	cur := root
	for i := 0; i < eff; i++ {
		tok := tokens[i]
		if child, ok := cur.children[tok]; ok {
			cur = child
			continue
		}
		if tok == TemplateWildcard || len(cur.children) >= maxChildren {
			cur = tmChild(cur, TemplateWildcard)
			continue
		}
		cur = tmChild(cur, tok)
	}
	return cur
}

func tmChild(n *tmNode, key string) *tmNode {
	if c, ok := n.children[key]; ok {
		return c
	}
	c := newTmNode()
	n.children[key] = c
	return c
}

// mine does the whole clustering decision under the partition lock.
func (m *TemplateMiner) mine(p *tmPartition, tokens []string, raw string, at time.Time, wantText bool) (uint32, bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	m.ensureOtherLocked(p)

	// An empty body has no pattern; it still counts, so it becomes overflow.
	if len(tokens) == 0 {
		p.overflow++
		return p.otherID, true, TemplateOther
	}

	leaf := p.descend(tokens, m.depth, m.maxChildren)

	if best, sim := tmBestMatch(leaf, tokens); best != nil && sim >= m.similarity {
		best = tmResolveAlias(best)
		// Lengths are equal by construction (the length layer plus the
		// survivor keeping its twin's pattern); the bound is defensive.
		changed := false
		for i := 0; i < len(tokens) && i < len(best.tokens); i++ {
			if best.tokens[i] == TemplateWildcard {
				continue
			}
			if best.tokens[i] != tokens[i] {
				best.tokens[i] = TemplateWildcard
				changed = true
			}
		}
		if changed {
			// The ID does not move — only the text under it.
			best.version++
			m.setText(best.id, tmJoin(best.tokens))
			m.stage(p, best)
			if twin := p.findTwinLocked(best); twin != nil {
				best = m.convergeLocked(p, best, twin)
			}
		}
		best.count++
		best.lastSeen = at
		m.markDirty(best)

		if !wantText {
			return best.id, false, ""
		}
		return best.id, false, tmJoin(best.tokens)
	}

	// New pattern.
	if len(p.templates) >= m.maxTemplates {
		p.overflow++
		return p.otherID, true, TemplateOther
	}

	text := tmJoin(tokens)
	id, err := m.reg.RegisterTemplate(TemplateRegistration{
		Tenant:   p.tenant,
		Service:  p.service,
		Template: text,
	})
	if err != nil || id == 0 {
		p.registrarFailures++
		p.overflow++
		return p.otherID, true, TemplateOther
	}
	if _, dup := p.templates[id]; dup || id == p.otherID {
		// A registrar that reissues a live ID would silently merge unrelated
		// series. Refuse the identity rather than corrupt it.
		p.registrarFailures++
		p.overflow++
		return p.otherID, true, TemplateOther
	}

	t := &tmTemplate{
		id:        id,
		seq:       p.nextSeq,
		tokens:    append([]string(nil), tokens...),
		count:     1,
		firstSeen: at,
		lastSeen:  at,
		sample:    raw,
		leaf:      leaf,
	}
	p.nextSeq++
	leaf.groups = append(leaf.groups, t)
	p.templates[id] = t
	m.setText(id, text)
	m.stage(p, t)
	return id, false, text
}

// ensureOtherLocked allocates the partition's overflow identity. It is
// pre-created on first use and retried on later calls if the registrar was
// unavailable.
func (m *TemplateMiner) ensureOtherLocked(p *tmPartition) {
	if p.otherID != 0 {
		return
	}
	id, err := m.reg.RegisterTemplate(TemplateRegistration{
		Tenant:   p.tenant,
		Service:  p.service,
		Template: TemplateOther,
		IsOther:  true,
	})
	if err != nil || id == 0 {
		p.registrarFailures++
		return
	}
	p.otherID = id
	m.setText(id, TemplateOther)
	m.stageRow(TemplateRow{
		ID:      id,
		Tenant:  p.tenant,
		Service: p.service,
		Tokens:  TemplateOther,
		IsOther: true,
	})
}

// tmBestMatch scores the leaf's templates by Drain's simSeq. Ties go to the
// earlier entry in the slice, which is creation order — deterministic.
func tmBestMatch(leaf *tmNode, tokens []string) (*tmTemplate, float64) {
	var best *tmTemplate
	bestSim := -1.0
	for _, g := range leaf.groups {
		if len(g.tokens) != len(tokens) {
			continue
		}
		if sim := tmSimilarity(g.tokens, tokens); sim > bestSim {
			bestSim = sim
			best = g
		}
	}
	return best, bestSim
}

// tmSimilarity is matching token positions over token count. Wildcards in the
// template count as matches.
func tmSimilarity(template, tokens []string) float64 {
	if len(template) != len(tokens) || len(tokens) == 0 {
		return 0
	}
	matches := 0
	for i, t := range template {
		if t == TemplateWildcard || t == tokens[i] {
			matches++
		}
	}
	return float64(matches) / float64(len(tokens))
}

func tmResolveAlias(t *tmTemplate) *tmTemplate {
	for i := 0; i < tmMaxAliasHops && t.aliasOf != nil; i++ {
		t = t.aliasOf
	}
	return t
}

// findTwinLocked looks for another live template with an identical pattern.
// Bounded by the per-partition cap, so this is a handful of comparisons and
// only runs when a template actually generalized.
func (p *tmPartition) findTwinLocked(t *tmTemplate) *tmTemplate {
	for _, other := range p.templates {
		if other == t || other.aliasOf != nil {
			continue
		}
		if tmTokensEqual(other.tokens, t.tokens) {
			return other
		}
	}
	return nil
}

// convergeLocked folds two identical patterns into one. The older template
// (lower partition-local sequence) survives; the retired ID keeps resolving
// through the alias index and its tree entry becomes a forwarder, so lines
// that used to match it now return the survivor. No issued ID is ever
// rewritten or reused.
func (m *TemplateMiner) convergeLocked(p *tmPartition, a, b *tmTemplate) *tmTemplate {
	survivor, retired := a, b
	if b.seq < a.seq {
		survivor, retired = b, a
	}

	survivor.count += retired.count
	if !retired.firstSeen.IsZero() && retired.firstSeen.Before(survivor.firstSeen) {
		survivor.firstSeen = retired.firstSeen
	}
	if retired.lastSeen.After(survivor.lastSeen) {
		survivor.lastSeen = retired.lastSeen
	}
	retired.count = 0
	retired.aliasOf = survivor

	delete(p.templates, retired.id)
	p.converged++

	m.aliasID(retired.id, survivor.id)
	// BOTH ends of the alias are staged. A historical series still names the
	// retired ID, so the forwarding row is as identity-critical as the
	// survivor's pattern — losing it turns a seven-day bucket into a template
	// nothing can resolve (#200 Q5).
	retired.version++
	m.stageRow(TemplateRow{
		ID:             retired.id,
		Tenant:         p.tenant,
		Service:        p.service,
		PatternVersion: retired.version,
		Tokens:         tmEncodeTokens(retired.tokens),
		Seq:            retired.seq,
		AliasOf:        survivor.id,
		Count:          retired.count,
		FirstSeen:      tmUnixNano(retired.firstSeen),
		LastSeen:       tmUnixNano(retired.lastSeen),
	})
	m.stage(p, survivor)
	return survivor
}

// --- presentation index ---

func (m *TemplateMiner) setText(id uint32, text string) {
	if id == 0 {
		return
	}
	m.idxMu.Lock()
	m.text[id] = text
	m.idxMu.Unlock()
}

// aliasID points a retired ID at its survivor, keeping alias chains flat.
func (m *TemplateMiner) aliasID(from, to uint32) {
	if from == 0 || to == 0 || from == to {
		return
	}
	m.idxMu.Lock()
	delete(m.text, from)
	m.alias[from] = to
	for k, v := range m.alias {
		if v == from {
			m.alias[k] = to
		}
	}
	m.idxMu.Unlock()
}

// --- tokenization and masking ---

// tmSplitTokens splits a body on whitespace, masks variable-looking tokens,
// and stops after max tokens so a pathological line cannot stretch Mine().
func tmSplitTokens(body string, max int) []string {
	if body == "" {
		return nil
	}
	out := make([]string, 0, 16)
	start := -1
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			if start >= 0 {
				out = append(out, tmMaskToken(body[start:i]))
				start = -1
				if len(out) >= max {
					return append(out, tmMaskTrunc)
				}
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, tmMaskToken(body[start:]))
	}
	return out
}

// tmMaskToken replaces a variable-looking token with a stable placeholder.
// Hand-rolled rather than regexp-driven: this runs per token on the ingest hot
// path, and the classification stays deterministic.
func tmMaskToken(tok string) string {
	switch {
	case tmIsUUID(tok):
		return tmMaskUUID
	case tmIsHex(tok):
		return tmMaskHex
	case tmIsIPv4(tok):
		return tmMaskIP
	case tmIsTimestamp(tok):
		return tmMaskTS
	case tmIsEmail(tok):
		return tmMaskEmail
	case tmIsNumber(tok):
		return tmMaskNum
	}
	if !tmHasDigit(tok) {
		return tok
	}
	return tmMaskDigitRuns(tok)
}

func tmHasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

func tmIsHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func tmIsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !tmIsHexDigit(s[i]) {
				return false
			}
		}
	}
	return true
}

// tmIsHex matches 0x-prefixed hex and long bare hex runs (>= 16 chars), which
// in practice are IDs, hashes, and encoded keys.
func tmIsHex(s string) bool {
	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		for i := 2; i < len(s); i++ {
			if !tmIsHexDigit(s[i]) {
				return false
			}
		}
		return true
	}
	if len(s) < 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !tmIsHexDigit(s[i]) {
			return false
		}
	}
	return true
}

func tmIsIPv4(s string) bool {
	if len(s) < 7 {
		return false
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		if !tmIsDigits(s[i+1:]) {
			return false
		}
		s = s[:i]
	}
	groups := 0
	for {
		i := strings.IndexByte(s, '.')
		var part string
		if i < 0 {
			part = s
		} else {
			part = s[:i]
		}
		if len(part) == 0 || len(part) > 3 || !tmIsDigits(part) {
			return false
		}
		groups++
		if i < 0 {
			break
		}
		s = s[i+1:]
	}
	return groups == 4
}

// tmIsTimestamp matches a leading ISO-8601 date, with or without a time part.
func tmIsTimestamp(s string) bool {
	if len(s) < 10 {
		return false
	}
	if s[4] != '-' || s[7] != '-' {
		return false
	}
	return tmIsDigits(s[0:4]) && tmIsDigits(s[5:7]) && tmIsDigits(s[8:10])
}

func tmIsEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at >= len(s)-3 {
		return false
	}
	return strings.IndexByte(s[at+1:], '.') > 0
}

func tmIsDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// tmIsNumber matches a signed integer or decimal.
func tmIsNumber(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	dot := false
	digits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits > 0
}

// tmMaskDigitRuns replaces every maximal digit run inside a token, which is
// what turns "/api/users/42/orders" into "/api/users/<NUM>/orders".
func tmMaskDigitRuns(tok string) string {
	var b strings.Builder
	b.Grow(len(tok))
	for i := 0; i < len(tok); {
		if tok[i] >= '0' && tok[i] <= '9' {
			j := i
			for j < len(tok) && tok[j] >= '0' && tok[j] <= '9' {
				j++
			}
			b.WriteString(tmMaskNum)
			i = j
			continue
		}
		b.WriteByte(tok[i])
		i++
	}
	return b.String()
}

func tmJoin(tokens []string) string { return strings.Join(tokens, " ") }

func tmTokensEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- durable miner state (#200 Q4, Q5) --------------------------------------
//
// What is persisted, and why exactly this much:
//
//	tenant/service     the partition a template belongs to, as STRINGS — a
//	                   reload has to rebuild partitions before any dictionary
//	                   cache is warm.
//	template_id        the immutable surrogate identity. It IS the dictionary
//	                   ID and IS the log series NameID; there is no second ID
//	                   space.
//	pattern_version    monotonic per template, so a stale write cannot roll a
//	                   generalization backwards.
//	tokens             the token pattern, which is everything the prefix tree
//	                   needs to be rebuilt.
//	seq                the partition-local ordinal convergence uses to pick a
//	                   survivor. Two restarts must not pick differently.
//	is_other           marks the partition's overflow identity.
//	alias_of           the survivor a retired ID forwards to.
//	count/first/last   statistics. Non-identity, periodic path.
//
// What is NOT persisted: the raw log sample. It is a credential and PII sink,
// and exemplars already carry the raw line for the cases that need one.

// tmTokenSep joins tokens for storage. The tokenizer splits on whitespace and
// never emits an empty or NUL-bearing token, so NUL is an unambiguous
// separator that survives any body a client can send.
const tmTokenSep = "\x00"

// tmEncodeTokens renders a token pattern for storage.
func tmEncodeTokens(tokens []string) string { return strings.Join(tokens, tmTokenSep) }

// tmDecodeTokens parses a stored token pattern. An empty string is no tokens,
// not one empty token.
func tmDecodeTokens(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, tmTokenSep)
}

// tmUnixNano renders a time for storage; the zero time stores as 0.
func tmUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// tmTimeFromNano reverses tmUnixNano.
func tmTimeFromNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// stage records one template's identity state for the next group commit. The
// partition mutex must be held.
func (m *TemplateMiner) stage(p *tmPartition, t *tmTemplate) {
	var aliasOf uint32
	if t.aliasOf != nil {
		aliasOf = t.aliasOf.id
	}
	m.stageRow(TemplateRow{
		ID:             t.id,
		Tenant:         p.tenant,
		Service:        p.service,
		PatternVersion: t.version,
		Tokens:         tmEncodeTokens(t.tokens),
		Seq:            t.seq,
		AliasOf:        aliasOf,
		Count:          t.count,
		FirstSeen:      tmUnixNano(t.firstSeen),
		LastSeen:       tmUnixNano(t.lastSeen),
	})
}

// stageRow queues one identity mutation. A newer version for the same ID
// replaces an older staged one: only the latest pattern is worth committing.
func (m *TemplateMiner) stageRow(row TemplateRow) {
	if row.ID == 0 {
		return
	}
	m.stageMu.Lock()
	if prev, ok := m.pending[row.ID]; !ok || row.PatternVersion >= prev.PatternVersion {
		m.pending[row.ID] = row
	}
	m.stageMu.Unlock()
}

// markDirty records a statistics-only change. The partition mutex must be held.
func (m *TemplateMiner) markDirty(t *tmTemplate) {
	if t.id == 0 {
		return
	}
	m.stageMu.Lock()
	m.dirty[t.id] = TemplateStatRow{
		ID:        t.id,
		Count:     t.count,
		FirstSeen: tmUnixNano(t.firstSeen),
		LastSeen:  tmUnixNano(t.lastSeen),
	}
	m.stageMu.Unlock()
}

// DrainPending returns the staged identity mutations for the next group
// commit, oldest ID first. They stay staged until Committed confirms them, so
// a failed commit re-offers them rather than acknowledging a delta whose
// template identity never became durable.
func (m *TemplateMiner) DrainPending() []TemplateRow {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	if len(m.pending) == 0 {
		return nil
	}
	out := make([]TemplateRow, 0, len(m.pending))
	for _, row := range m.pending {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Committed marks drained rows durable. A row whose pattern moved on since the
// drain stays staged: the version guard is what makes that safe.
func (m *TemplateMiner) Committed(rows []TemplateRow) {
	if len(rows) == 0 {
		return
	}
	m.stageMu.Lock()
	for _, row := range rows {
		if cur, ok := m.pending[row.ID]; ok && cur.PatternVersion <= row.PatternVersion {
			delete(m.pending, row.ID)
		}
	}
	m.stageMu.Unlock()
}

// PendingCount reports how many identity mutations are staged but not durable.
func (m *TemplateMiner) PendingCount() int {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	return len(m.pending)
}

// DrainDirtyStats returns the statistics-only updates for the periodic write
// and clears the dirty set. These are fire-and-forget: a lost batch costs a
// count that the next line restores, never an identity.
func (m *TemplateMiner) DrainDirtyStats() []TemplateStatRow {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	if len(m.dirty) == 0 {
		return nil
	}
	out := make([]TemplateStatRow, 0, len(m.dirty))
	for _, row := range m.dirty {
		out = append(out, row)
	}
	m.dirty = make(map[uint32]TemplateStatRow)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Roots returns every template ID the miner keeps alive: live templates, the
// per-partition overflow sentinels, both ends of every alias, and every staged
// mutation (#200 Q5).
//
// Both ends, deliberately. A historical series that named a retired template
// keeps that ID, its alias row, AND the survivor alive: collecting the survivor
// would leave the alias pointing at nothing, and collecting the alias would
// leave the series unresolvable.
func (m *TemplateMiner) Roots() map[uint32]struct{} {
	m.mu.RLock()
	parts := make([]*tmPartition, 0, len(m.parts))
	for _, p := range m.parts {
		parts = append(parts, p)
	}
	m.mu.RUnlock()

	out := make(map[uint32]struct{}, len(parts)*(m.maxTemplates+1))
	for _, p := range parts {
		p.mu.Lock()
		if p.otherID != 0 {
			out[p.otherID] = struct{}{}
		}
		for id := range p.templates {
			out[id] = struct{}{}
		}
		p.mu.Unlock()
	}

	m.idxMu.RLock()
	for id := range m.text {
		out[id] = struct{}{}
	}
	for from, to := range m.alias {
		out[from] = struct{}{}
		out[to] = struct{}{}
	}
	m.idxMu.RUnlock()

	m.stageMu.Lock()
	for id, row := range m.pending {
		out[id] = struct{}{}
		if row.AliasOf != 0 {
			out[row.AliasOf] = struct{}{}
		}
	}
	m.stageMu.Unlock()
	return out
}

// Restore rebuilds the miner's partitions, prefix trees, alias index and cap
// accounting from durable rows. It must run BEFORE ingest starts: a line mined
// against an empty miner would mint a second identity for a pattern that
// already has one, and both would be live.
//
// Restoring stages nothing: every row handed here is already durable.
func (m *TemplateMiner) Restore(rows []TemplateRow) {
	if len(rows) == 0 {
		return
	}
	// Deterministic order so partition sequence counters and leaf group order
	// rebuild identically on every boot.
	ordered := append([]TemplateRow(nil), rows...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Tenant != ordered[j].Tenant {
			return ordered[i].Tenant < ordered[j].Tenant
		}
		if ordered[i].Service != ordered[j].Service {
			return ordered[i].Service < ordered[j].Service
		}
		if ordered[i].Seq != ordered[j].Seq {
			return ordered[i].Seq < ordered[j].Seq
		}
		return ordered[i].ID < ordered[j].ID
	})

	byID := make(map[uint32]*tmTemplate, len(ordered))
	aliasTargets := make(map[uint32]uint32, len(ordered))
	for _, row := range ordered {
		p := m.partition(row.Tenant, row.Service)
		p.mu.Lock()
		if row.IsOther {
			p.otherID = row.ID
			p.mu.Unlock()
			m.setText(row.ID, TemplateOther)
			continue
		}
		tokens := tmDecodeTokens(row.Tokens)
		t := &tmTemplate{
			id:        row.ID,
			seq:       row.Seq,
			tokens:    tokens,
			version:   row.PatternVersion,
			count:     row.Count,
			firstSeen: tmTimeFromNano(row.FirstSeen),
			lastSeen:  tmTimeFromNano(row.LastSeen),
		}
		if row.Seq >= p.nextSeq {
			p.nextSeq = row.Seq + 1
		}
		if row.AliasOf != 0 {
			// A retired ID stays in its leaf as a forwarder so lines that used
			// to match it resolve to the survivor, and it does NOT consume a
			// slot against the per-partition cap.
			aliasTargets[row.ID] = row.AliasOf
		} else {
			p.templates[row.ID] = t
		}
		if len(tokens) > 0 {
			leaf := p.descend(tokens, m.depth, m.maxChildren)
			t.leaf = leaf
			leaf.groups = append(leaf.groups, t)
		}
		p.mu.Unlock()
		byID[row.ID] = t
		if row.AliasOf == 0 {
			m.setText(row.ID, tmJoin(tokens))
		}
	}

	// Second pass: alias edges, once every template object exists.
	m.idxMu.Lock()
	for from, to := range aliasTargets {
		m.alias[from] = to
		delete(m.text, from)
	}
	m.idxMu.Unlock()
	for from, to := range aliasTargets {
		if src, ok := byID[from]; ok {
			src.aliasOf = byID[to]
		}
	}
}

// RestoreMiner warms miner from store before ingest starts. A store that
// cannot carry templates (an older implementation) restores nothing, which is
// the pre-#200 behaviour.
func RestoreMiner(store Store, miner *TemplateMiner) (int, error) {
	gcs, ok := store.(GCStore)
	if !ok || miner == nil {
		return 0, nil
	}
	rows, err := gcs.LoadTemplates(0)
	if err != nil {
		return 0, err
	}
	miner.Restore(rows)
	return len(rows), nil
}
