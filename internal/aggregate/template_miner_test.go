package aggregate

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// partitionScopedRegistrar allocates IDs from a per-(tenant, service) counter
// so ID values do not depend on how partitions interleave. The miner's own
// guarantee is a deterministic per-partition registration sequence; this
// registrar turns that into comparable absolute IDs.
type partitionScopedRegistrar struct {
	mu    sync.Mutex
	bases map[string]uint32
	next  map[string]uint32
}

func newPartitionScopedRegistrar() *partitionScopedRegistrar {
	return &partitionScopedRegistrar{
		bases: make(map[string]uint32),
		next:  make(map[string]uint32),
	}
}

func (r *partitionScopedRegistrar) RegisterTemplate(reg TemplateRegistration) (uint32, error) {
	key := reg.Tenant + "\x00" + reg.Service
	r.mu.Lock()
	defer r.mu.Unlock()
	base, ok := r.bases[key]
	if !ok {
		// Deterministic per-partition ID space, derived from the partition key
		// so it does not depend on which partition registered first.
		h := fnv.New32a()
		_, _ = h.Write([]byte(key))
		base = 1_000_000 + (h.Sum32()%1000)*1000
		r.bases[key] = base
	}
	r.next[key]++
	return base + r.next[key], nil
}

func testMiner(t *testing.T, cfg TemplateMinerConfig) *TemplateMiner {
	t.Helper()
	return NewTemplateMiner(cfg)
}

var testTime = time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)

// --- determinism ---

func TestTemplateMinerDeterministicAcrossInterleavings(t *testing.T) {
	type line struct{ service, body string }

	svcA := []string{
		"checkout order 1001 failed for shard alpha",
		"checkout order 1002 failed for shard alpha",
		"cart item 44 added by user 9001",
		"payment gateway timeout after 250 ms",
		"checkout order 1003 failed for shard bravo",
		"cart item 45 added by user 9002",
	}
	svcB := []string{
		"index rebuild started for tenant acme",
		"index rebuild finished in 1200 ms",
		"query 0xdeadbeefcafe returned 17 rows",
		"index rebuild started for tenant globex",
		"query 0xfeedface returned 3 rows",
	}

	sequential := make([]line, 0, len(svcA)+len(svcB))
	for _, b := range svcA {
		sequential = append(sequential, line{"checkout", b})
	}
	for _, b := range svcB {
		sequential = append(sequential, line{"search", b})
	}

	interleaved := make([]line, 0, len(svcA)+len(svcB))
	for i := 0; i < len(svcA) || i < len(svcB); i++ {
		if i < len(svcB) {
			interleaved = append(interleaved, line{"search", svcB[i]})
		}
		if i < len(svcA) {
			interleaved = append(interleaved, line{"checkout", svcA[i]})
		}
	}

	run := func(lines []line) (map[string]uint32, map[string]string) {
		m := testMiner(t, TemplateMinerConfig{Registrar: newPartitionScopedRegistrar()})
		ids := make(map[string]uint32)
		texts := make(map[string]string)
		for _, l := range lines {
			id, isOther := m.MineAt("acme", l.service, "INFO", l.body, testTime)
			if isOther {
				t.Fatalf("unexpected overflow for %q", l.body)
			}
			ids[l.service+"|"+l.body] = id
		}
		for k, id := range ids {
			txt, ok := m.TemplateText(id)
			if !ok {
				t.Fatalf("no text for id %d (%s)", id, k)
			}
			texts[k] = txt
		}
		return ids, texts
	}

	idsSeq, textSeq := run(sequential)
	idsInt, textInt := run(interleaved)

	if len(idsSeq) != len(idsInt) {
		t.Fatalf("id map sizes differ: %d vs %d", len(idsSeq), len(idsInt))
	}
	for k, want := range idsSeq {
		if got := idsInt[k]; got != want {
			t.Errorf("%s: id %d != %d across interleavings", k, got, want)
		}
		if got, want := textInt[k], textSeq[k]; got != want {
			t.Errorf("%s: text %q != %q across interleavings", k, got, want)
		}
	}
}

func TestTemplateMinerSameStreamSameIDs(t *testing.T) {
	bodies := []string{
		"GET /api/users/12345/orders took 34ms",
		"GET /api/users/999/orders took 7ms",
		"worker pool saturated queue depth 4096",
		"worker pool saturated queue depth 8192",
	}
	first := make([]uint32, len(bodies))
	m1 := testMiner(t, TemplateMinerConfig{Registrar: newPartitionScopedRegistrar()})
	for i, b := range bodies {
		first[i], _ = m1.MineAt("acme", "api", "INFO", b, testTime)
	}
	m2 := testMiner(t, TemplateMinerConfig{Registrar: newPartitionScopedRegistrar()})
	for i, b := range bodies {
		got, _ := m2.MineAt("acme", "api", "INFO", b, testTime)
		if got != first[i] {
			t.Errorf("body %q: id %d on rerun, want %d", b, got, first[i])
		}
	}
}

// --- immutability ---

func TestTemplateIDStableWhileTextGeneralizes(t *testing.T) {
	m := testMiner(t, TemplateMinerConfig{})

	id1, isOther := m.MineAt("acme", "checkout", "ERROR", "checkout order failed for shard alpha", testTime)
	if isOther {
		t.Fatal("first line overflowed")
	}
	txt1, _ := m.TemplateText(id1)
	if txt1 != "checkout order failed for shard alpha" {
		t.Fatalf("initial text = %q", txt1)
	}

	id2, _ := m.MineAt("acme", "checkout", "ERROR", "checkout order failed for shard bravo", testTime)
	if id2 != id1 {
		t.Fatalf("id changed on generalization: %d -> %d", id1, id2)
	}
	txt2, _ := m.TemplateText(id1)
	if txt2 != "checkout order failed for shard "+TemplateWildcard {
		t.Fatalf("generalized text = %q", txt2)
	}

	// Further generalization still keeps the identity.
	id3, _ := m.MineAt("acme", "checkout", "ERROR", "checkout order failed for zone bravo", testTime)
	if id3 != id1 {
		t.Fatalf("id changed on second generalization: %d -> %d", id1, id3)
	}
	txt3, _ := m.TemplateText(id1)
	if txt3 == txt2 {
		t.Fatalf("text did not evolve: %q", txt3)
	}
	if !strings.Contains(txt3, TemplateWildcard) {
		t.Fatalf("expected wildcards in %q", txt3)
	}

	st, ok := m.PartitionStats("acme", "checkout")
	if !ok || st.Templates != 1 {
		t.Fatalf("stats = %+v, ok=%v", st, ok)
	}
}

func TestTemplateConvergenceKeepsSurvivorID(t *testing.T) {
	// Convergence is not reachable through best-match dynamics: a template
	// that dominates another always wins the match, so a merge can never
	// produce a pattern that already exists in the same leaf. The path exists
	// because durable identity must survive it if it ever does happen (a
	// different depth, threshold, or masking table changes the reachability
	// argument), so it is exercised directly.
	m := testMiner(t, TemplateMinerConfig{})

	older, _ := m.MineAt("acme", "search", "INFO", "index rebuild started alpha", testTime)
	newer, _ := m.MineAt("acme", "search", "INFO", "query planner rejected beta", testTime)
	if older == newer {
		t.Fatal("expected two distinct templates")
	}

	p := m.partition("acme", "search")
	p.mu.Lock()
	a, b := p.templates[older], p.templates[newer]
	if a == nil || b == nil {
		p.mu.Unlock()
		t.Fatalf("templates missing: %v %v", a, b)
	}
	// Force the identical pattern, then converge from the newer side. Both
	// sides render the same text, which is what an actual convergence means.
	a.tokens = append([]string(nil), b.tokens...)
	survivor := m.convergeLocked(p, b, a)
	p.mu.Unlock()

	if survivor.id != older {
		t.Fatalf("survivor = %d, want the older id %d", survivor.id, older)
	}

	// The retired ID stays valid and resolves to the survivor's text.
	wantText, _ := m.TemplateText(older)
	gotText, ok := m.TemplateText(newer)
	if !ok || gotText != wantText {
		t.Fatalf("retired id text = %q (ok=%v), want %q", gotText, ok, wantText)
	}

	// Lines that used to match the retired template now return the survivor.
	got, isOther := m.MineAt("acme", "search", "INFO", "query planner rejected beta", testTime)
	if isOther {
		t.Fatal("post-convergence line overflowed")
	}
	if got != older {
		t.Fatalf("post-convergence id = %d, want survivor %d", got, older)
	}

	st, _ := m.PartitionStats("acme", "search")
	if st.Converged != 1 {
		t.Fatalf("converged = %d, want 1", st.Converged)
	}
	if st.Templates != 1 {
		t.Fatalf("templates = %d, want 1", st.Templates)
	}
}

func TestTemplateIDsNeverRemapped(t *testing.T) {
	m := testMiner(t, TemplateMinerConfig{MaxTemplatesPerService: 4})
	seen := make(map[uint32]string)

	bodies := []string{
		"alpha task 1 done in 5ms",
		"alpha task 2 done in 9ms",
		"bravo cache miss for key abc",
		"bravo cache miss for key xyz",
		"charlie flush wrote 12 pages",
		"delta connection reset by peer",
		"echo shard 3 rebalanced",
		"foxtrot lease expired for node 7",
	}
	for _, b := range bodies {
		id, isOther := m.MineAt("acme", "svc", "INFO", b, testTime)
		if isOther {
			continue
		}
		if prev, ok := seen[id]; ok && prev != b {
			// Same ID for a different body is fine (that is clustering); the
			// invariant under test is the reverse direction below.
			_ = prev
		}
		seen[id] = b
	}

	// Every ID ever issued must still resolve.
	for id := range seen {
		if _, ok := m.TemplateText(id); !ok {
			t.Errorf("issued id %d no longer resolves", id)
		}
	}

	// Re-mining a body must return the same ID it first got.
	firstIDs := make(map[string]uint32)
	for _, b := range bodies {
		id, _ := m.MineAt("acme", "svc", "INFO", b, testTime)
		firstIDs[b] = id
	}
	for _, b := range bodies {
		id, _ := m.MineAt("acme", "svc", "INFO", b, testTime)
		if id != firstIDs[b] {
			t.Errorf("body %q remapped from %d to %d", b, firstIDs[b], id)
		}
	}
}

// --- partition isolation ---

func TestTemplatePartitionIsolation(t *testing.T) {
	m := testMiner(t, TemplateMinerConfig{})
	body := "connection pool exhausted for shard alpha"

	idA, _ := m.MineAt("acme", "checkout", "WARN", body, testTime)
	idB, _ := m.MineAt("acme", "search", "WARN", body, testTime)
	idT, _ := m.MineAt("globex", "checkout", "WARN", body, testTime)

	if idA == idB || idA == idT || idB == idT {
		t.Fatalf("partitions share identity: %d %d %d", idA, idB, idT)
	}
	for _, id := range []uint32{idA, idB, idT} {
		txt, ok := m.TemplateText(id)
		if !ok || txt != body {
			t.Fatalf("id %d text = %q ok=%v", id, txt, ok)
		}
	}

	// Generalizing one partition must not touch the others.
	m.MineAt("acme", "checkout", "WARN", "connection pool exhausted for shard bravo", testTime)
	if txt, _ := m.TemplateText(idA); !strings.Contains(txt, TemplateWildcard) {
		t.Fatalf("checkout template did not generalize: %q", txt)
	}
	if txt, _ := m.TemplateText(idB); txt != body {
		t.Fatalf("search template mutated: %q", txt)
	}
	if txt, _ := m.TemplateText(idT); txt != body {
		t.Fatalf("globex template mutated: %q", txt)
	}

	stats := m.Stats()
	if len(stats) != 3 {
		t.Fatalf("stats len = %d, want 3", len(stats))
	}
	if stats[0].Tenant != "acme" || stats[0].Service != "checkout" {
		t.Fatalf("stats not sorted: %+v", stats)
	}
	if stats[2].Tenant != "globex" {
		t.Fatalf("stats not sorted: %+v", stats)
	}
}

func TestTemplateMinerConcurrentMine(t *testing.T) {
	m := testMiner(t, TemplateMinerConfig{
		MaxTemplatesPerService: 6,
		OnFact:                 func(TemplateFact) {},
	})

	const goroutines = 16
	const perGoroutine = 200
	services := []string{"checkout", "search", "cart", "payments"}
	tenants := []string{"acme", "globex"}

	var wg sync.WaitGroup
	var overflow atomic.Uint64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			svc := services[g%len(services)]
			ten := tenants[g%len(tenants)]
			for i := 0; i < perGoroutine; i++ {
				body := fmt.Sprintf("worker %d handled request %d in %d ms path /api/v1/items/%d", g, i, i%97, i)
				if _, isOther := m.MineAt(ten, svc, "INFO", body, testTime); isOther {
					overflow.Add(1)
				}
				m.TemplateText(uint32(i%32) + 1)
				m.Stats()
			}
		}(g)
	}
	wg.Wait()

	total := uint64(0)
	for _, st := range m.Stats() {
		if st.Templates > 6 {
			t.Errorf("%s/%s: %d templates over cap", st.Tenant, st.Service, st.Templates)
		}
		total += st.Overflow
	}
	if total != overflow.Load() {
		t.Errorf("overflow accounting: stats %d, callers %d", total, overflow.Load())
	}
}

// --- cap behavior ---

func TestTemplateCapRoutesToOther(t *testing.T) {
	m := testMiner(t, TemplateMinerConfig{MaxTemplatesPerService: 10})

	words := []string{
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
		"india", "juliet", "kilo", "lima", "mike", "november", "oscar",
	}
	ids := make([]uint32, 0, 10)
	for i := 0; i < 10; i++ {
		body := fmt.Sprintf("%s shard rotated cleanly", words[i])
		id, isOther := m.MineAt("acme", "svc", "INFO", body, testTime)
		if isOther {
			t.Fatalf("pattern %d overflowed early", i)
		}
		ids = append(ids, id)
	}

	st, _ := m.PartitionStats("acme", "svc")
	if st.Templates != 10 {
		t.Fatalf("templates = %d, want 10", st.Templates)
	}
	otherID := st.OtherID
	if otherID == 0 {
		t.Fatal("overflow template was not pre-created")
	}
	for _, id := range ids {
		if id == otherID {
			t.Fatalf("mined id %d collides with __other__", id)
		}
	}
	if txt, ok := m.TemplateText(otherID); !ok || txt != TemplateOther {
		t.Fatalf("__other__ text = %q ok=%v", txt, ok)
	}

	// Templates 11..15 collapse into __other__, totals preserved via isOther.
	for i := 10; i < 15; i++ {
		body := fmt.Sprintf("%s shard rotated cleanly", words[i])
		id, isOther := m.MineAt("acme", "svc", "INFO", body, testTime)
		if !isOther {
			t.Fatalf("pattern %d was admitted past the cap", i)
		}
		if id != otherID {
			t.Fatalf("pattern %d: id %d, want __other__ %d", i, id, otherID)
		}
	}

	st, _ = m.PartitionStats("acme", "svc")
	if st.Templates != 10 {
		t.Fatalf("templates = %d after overflow, want 10", st.Templates)
	}
	if st.Overflow != 5 {
		t.Fatalf("overflow = %d, want 5", st.Overflow)
	}

	// Admitted templates keep working after the cap is hit.
	id, isOther := m.MineAt("acme", "svc", "INFO", words[3]+" shard rotated cleanly", testTime)
	if isOther || id != ids[3] {
		t.Fatalf("admitted template regressed: id=%d isOther=%v want %d", id, isOther, ids[3])
	}

	// A different service has its own budget.
	if _, isOther := m.MineAt("acme", "other-svc", "INFO", words[14]+" shard rotated cleanly", testTime); isOther {
		t.Fatal("cap leaked across partitions")
	}
}

func TestTemplateEmptyBodyIsOverflow(t *testing.T) {
	m := testMiner(t, TemplateMinerConfig{})
	id, isOther := m.MineAt("acme", "svc", "INFO", "   ", testTime)
	if !isOther {
		t.Fatal("blank body should be overflow")
	}
	st, _ := m.PartitionStats("acme", "svc")
	if id != st.OtherID || st.Overflow != 1 {
		t.Fatalf("id=%d stats=%+v", id, st)
	}
}

func TestTemplateRegistrarFailureDegrades(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var seq atomic.Uint32
	m := testMiner(t, TemplateMinerConfig{
		Registrar: TemplateRegistrarFunc(func(TemplateRegistration) (uint32, error) {
			if fail.Load() {
				return 0, fmt.Errorf("dictionary unavailable")
			}
			return seq.Add(1), nil
		}),
	})

	id, isOther := m.MineAt("acme", "svc", "INFO", "shard alpha went offline", testTime)
	if !isOther || id != 0 {
		t.Fatalf("expected (0, true) while the registrar is down, got (%d, %v)", id, isOther)
	}
	st, _ := m.PartitionStats("acme", "svc")
	if st.RegistrarFailures == 0 || st.Overflow != 1 {
		t.Fatalf("stats = %+v", st)
	}

	// Recovery: the overflow identity is retried on the next call.
	fail.Store(false)
	id, isOther = m.MineAt("acme", "svc", "INFO", "shard alpha went offline", testTime)
	if isOther || id == 0 {
		t.Fatalf("expected a real identity after recovery, got (%d, %v)", id, isOther)
	}
	st, _ = m.PartitionStats("acme", "svc")
	if st.OtherID == 0 {
		t.Fatal("overflow identity not allocated after recovery")
	}
}

func TestTemplateRegistrarDuplicateIDRefused(t *testing.T) {
	m := testMiner(t, TemplateMinerConfig{
		Registrar: TemplateRegistrarFunc(func(r TemplateRegistration) (uint32, error) {
			if r.IsOther {
				return 1, nil
			}
			return 2, nil // always the same live ID
		}),
	})
	if _, isOther := m.MineAt("acme", "svc", "INFO", "alpha bravo charlie delta echo", testTime); isOther {
		t.Fatal("first template should be admitted")
	}
	id, isOther := m.MineAt("acme", "svc", "INFO", "zulu yankee xray whiskey victor", testTime)
	if !isOther || id != 1 {
		t.Fatalf("duplicate id should degrade to __other__, got (%d, %v)", id, isOther)
	}
	st, _ := m.PartitionStats("acme", "svc")
	if st.RegistrarFailures != 1 {
		t.Fatalf("registrar failures = %d, want 1", st.RegistrarFailures)
	}
}

// --- facts ---

func TestTemplateFactHook(t *testing.T) {
	var facts []TemplateFact
	var mu sync.Mutex
	m := testMiner(t, TemplateMinerConfig{
		MaxTemplatesPerService: 1,
		OnFact: func(f TemplateFact) {
			mu.Lock()
			facts = append(facts, f)
			mu.Unlock()
		},
	})

	id, _ := m.MineAt("acme", "checkout", "ERROR", "order 42 rejected by risk engine", testTime)
	m.MineAt("acme", "checkout", "WARN", "totally different shape here now", testTime)

	if len(facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(facts))
	}
	f := facts[0]
	if f.Tenant != "acme" || f.Service != "checkout" || f.Severity != "ERROR" {
		t.Fatalf("fact identity = %+v", f)
	}
	if f.TemplateID != id || f.IsOther {
		t.Fatalf("fact id = %d isOther=%v, want %d false", f.TemplateID, f.IsOther, id)
	}
	if f.Template != "order "+tmMaskNum+" rejected by risk engine" {
		t.Fatalf("fact template = %q", f.Template)
	}
	if !f.Timestamp.Equal(testTime) {
		t.Fatalf("fact timestamp = %v", f.Timestamp)
	}
	if !facts[1].IsOther || facts[1].Template != TemplateOther {
		t.Fatalf("overflow fact = %+v", facts[1])
	}
}

// --- masking ---

func TestTemplateMasking(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"user 12345 logged in", "user " + tmMaskNum + " logged in"},
		{"latency -1.5 ms", "latency " + tmMaskNum + " ms"},
		{"GET /api/users/42/orders", "GET /api/users/" + tmMaskNum + "/orders"},
		{"took 34ms", "took " + tmMaskNum + "ms"},
		{"peer 10.0.0.7:5432 closed", "peer " + tmMaskIP + " closed"},
		{"trace 3f2504e0-4f89-11d3-9a0c-0305e82c3301 ended", "trace " + tmMaskUUID + " ended"},
		{"span 0xdeadbeef parent cafebabedeadbeef99", "span " + tmMaskHex + " parent " + tmMaskHex},
		{"at 2026-08-21T10:00:00Z boom", "at " + tmMaskTS + " boom"},
		{"mail ops@example.com bounced", "mail " + tmMaskEmail + " bounced"},
		{"  padded   spacing  ", "padded spacing"},
		{"no variables here", "no variables here"},
	}
	for _, c := range cases {
		got := tmJoin(tmSplitTokens(c.body, defaultTemplateMaxTokens))
		if got != c.want {
			t.Errorf("mask(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestTemplateTokenCapBoundsWork(t *testing.T) {
	body := strings.Repeat("tok ", 5000)
	tokens := tmSplitTokens(body, defaultTemplateMaxTokens)
	if len(tokens) != defaultTemplateMaxTokens+1 {
		t.Fatalf("tokens = %d, want %d", len(tokens), defaultTemplateMaxTokens+1)
	}
	if tokens[len(tokens)-1] != tmMaskTrunc {
		t.Fatalf("last token = %q", tokens[len(tokens)-1])
	}

	m := testMiner(t, TemplateMinerConfig{})
	if _, isOther := m.MineAt("acme", "svc", "INFO", body, testTime); isOther {
		t.Fatal("truncated line should still get an identity")
	}
}

func TestTemplateUnknownIDDoesNotResolve(t *testing.T) {
	m := testMiner(t, TemplateMinerConfig{})
	if _, ok := m.TemplateText(4242); ok {
		t.Fatal("unknown id resolved")
	}
}

// --- benchmarks ---

var benchBodies = []string{
	"GET /api/v1/orders/847362/items returned 200 in 34ms",
	"checkout order 847362 failed for shard alpha: downstream timeout",
	"connection to 10.4.12.9:5432 reset by peer after 1200 ms",
	"trace 3f2504e0-4f89-11d3-9a0c-0305e82c3301 span 0xdeadbeefcafe0001 ended",
	"cache miss for key user:847362:profile, refilling from postgres",
	"worker pool saturated: queue depth 4096, dropping healthy batch",
}

func BenchmarkTemplateMinerMine(b *testing.B) {
	m := NewTemplateMiner(TemplateMinerConfig{MaxTemplatesPerService: 10})
	for _, body := range benchBodies {
		m.MineAt("acme", "checkout", "INFO", body, testTime)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.MineAt("acme", "checkout", "INFO", benchBodies[i%len(benchBodies)], testTime)
	}
}

func BenchmarkTemplateMinerMineParallel(b *testing.B) {
	m := NewTemplateMiner(TemplateMinerConfig{MaxTemplatesPerService: 10})
	services := []string{"checkout", "search", "cart", "payments", "shipping", "auth"}
	for _, svc := range services {
		for _, body := range benchBodies {
			m.MineAt("acme", svc, "INFO", body, testTime)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	var n atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		i := int(n.Add(1))
		svc := services[i%len(services)]
		for pb.Next() {
			i++
			m.MineAt("acme", svc, "INFO", benchBodies[i%len(benchBodies)], testTime)
		}
	})
}

func BenchmarkTemplateMinerTokenize(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tmSplitTokens(benchBodies[i%len(benchBodies)], defaultTemplateMaxTokens)
	}
}
