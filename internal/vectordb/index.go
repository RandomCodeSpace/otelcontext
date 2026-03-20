// Package vectordb provides an embedded TF-IDF / cosine-similarity vector index
// for semantic log search. It is a pure-Go, no-CGO, in-process accelerator.
// The relational DB remains the source of truth; this index is fully rebuildable.
package vectordb

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// LogVector represents an indexed log entry.
type LogVector struct {
	LogID     uint
	ServiceName string
	Severity  string
	Body      string
	vec       map[string]float64 // TF-IDF sparse vector
}

// SearchResult is a single similarity hit.
type SearchResult struct {
	LogID       uint
	ServiceName string
	Severity    string
	Body        string
	Score       float64 // cosine similarity 0.0–1.0
}

// Index is a thread-safe in-memory TF-IDF vector index for log bodies.
// Only ERROR and WARN logs are indexed to keep it small and relevant.
type Index struct {
	mu      sync.RWMutex
	docs    []LogVector          // indexed log vectors
	idf     map[string]float64   // global IDF table
	maxSize int                  // FIFO eviction cap
	dirty   bool                 // IDF needs recompute
}

// New creates a new Index with the given maximum entry cap.
func New(maxSize int) *Index {
	if maxSize <= 0 {
		maxSize = 100_000
	}
	return &Index{
		maxSize: maxSize,
		idf:     make(map[string]float64),
	}
}

// Add adds a log to the index. Thread-safe.
func (idx *Index) Add(logID uint, serviceName, severity, body string) {
	if !shouldIndex(severity) {
		return
	}
	tokens := tokenize(body)
	if len(tokens) == 0 {
		return
	}
	tf := computeTF(tokens)

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// FIFO eviction — copy to new slice to release old backing array memory
	if len(idx.docs) >= idx.maxSize {
		keep := idx.docs[idx.maxSize/10:]
		newDocs := make([]LogVector, len(keep), idx.maxSize)
		copy(newDocs, keep)
		idx.docs = newDocs
		idx.dirty = true
	}

	idx.docs = append(idx.docs, LogVector{
		LogID:       logID,
		ServiceName: serviceName,
		Severity:    severity,
		Body:        body,
		vec:         tf,
	})
	idx.dirty = true
}

// Search finds the top-k logs most similar to the query string.
func (idx *Index) Search(query string, k int) []SearchResult {
	if k <= 0 {
		k = 10
	}
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	queryTF := computeTF(tokens)

	idx.mu.Lock()
	if idx.dirty {
		idx.recomputeIDF()
		idx.dirty = false
	}
	// Snapshot IDF and docs for the query (avoids holding lock during scoring).
	idfSnap := make(map[string]float64, len(idx.idf))
	for k, v := range idx.idf {
		idfSnap[k] = v
	}
	docs := make([]LogVector, len(idx.docs))
	copy(docs, idx.docs)
	idx.mu.Unlock()

	// Build TF-IDF query vector.
	queryVec := make(map[string]float64, len(queryTF))
	for term, tf := range queryTF {
		queryVec[term] = tf * idfSnap[term]
	}
	queryNorm := vecNorm(queryVec)
	if queryNorm == 0 {
		return nil
	}

	type scored struct {
		doc   LogVector
		score float64
	}
	results := make([]scored, 0, len(docs))
	for _, doc := range docs {
		docVec := make(map[string]float64, len(doc.vec))
		for term, tf := range doc.vec {
			docVec[term] = tf * idfSnap[term]
		}
		score := cosineSimilarity(queryVec, queryNorm, docVec)
		if score > 0 {
			results = append(results, scored{doc, score})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})
	if len(results) > k {
		results = results[:k]
	}

	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = SearchResult{
			LogID:       r.doc.LogID,
			ServiceName: r.doc.ServiceName,
			Severity:    r.doc.Severity,
			Body:        r.doc.Body,
			Score:       r.score,
		}
	}
	return out
}

// Size returns the current number of indexed documents.
func (idx *Index) Size() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.docs)
}

// recomputeIDF rebuilds the IDF table from current docs. Must be called with mu held.
func (idx *Index) recomputeIDF() {
	df := make(map[string]int, len(idx.idf))
	for _, doc := range idx.docs {
		for term := range doc.vec {
			df[term]++
		}
	}
	n := float64(len(idx.docs))
	// Replace the entire IDF map to drop stale terms from evicted docs
	newIDF := make(map[string]float64, len(df))
	for term, count := range df {
		newIDF[term] = math.Log(n/float64(count)) + 1
	}
	idx.idf = newIDF
}

// shouldIndex returns true for severity levels worth indexing.
func shouldIndex(severity string) bool {
	s := strings.ToUpper(severity)
	return s == "ERROR" || s == "WARN" || s == "WARNING" || s == "FATAL" || s == "CRITICAL"
}

// tokenize splits text into lowercase alpha tokens, removing stop words.
func tokenize(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) > 2 && !isStopWord(w) {
			out = append(out, w)
		}
	}
	return out
}

// computeTF returns term-frequency (count / total) for a token list.
func computeTF(tokens []string) map[string]float64 {
	counts := make(map[string]int, len(tokens))
	for _, t := range tokens {
		counts[t]++
	}
	total := float64(len(tokens))
	tf := make(map[string]float64, len(counts))
	for term, count := range counts {
		tf[term] = float64(count) / total
	}
	return tf
}

func vecNorm(v map[string]float64) float64 {
	var sum float64
	for _, val := range v {
		sum += val * val
	}
	return math.Sqrt(sum)
}

func cosineSimilarity(a map[string]float64, normA float64, b map[string]float64) float64 {
	normB := vecNorm(b)
	if normA == 0 || normB == 0 {
		return 0
	}
	var dot float64
	for term, va := range a {
		if vb, ok := b[term]; ok {
			dot += va * vb
		}
	}
	return dot / (normA * normB)
}

var stopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "was": {}, "not": {},
	"with": {}, "this": {}, "that": {}, "from": {}, "has": {}, "but": {},
	"have": {}, "its": {}, "been": {}, "also": {}, "than": {}, "into": {},
}

func isStopWord(w string) bool {
	_, ok := stopWords[w]
	return ok
}
