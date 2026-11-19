package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Reranker calls a rerank server to score each search candidate against
// the query. A reranker model reads the query and one memory together, so
// it can tell a memory that answers the query from one that only shares
// words with it. A nil *Reranker means that no reranker is configured:
// search then keeps the order of rrfFuse.
type Reranker struct {
	URL    string
	Model  string
	Client *http.Client
	// Timeout bounds one rerank call. Zero means defaultRerankTimeout.
	Timeout time.Duration
}

const (
	defaultRerankTimeout = 15 * time.Second
	// rerankMaxDocs and rerankMaxBytes bound the cost of one search. The
	// model reads each candidate in full, with the prompt of the model
	// around it. Measured 2026-11-19 with Qwen3-Reranker-4B Q4_K_M on an
	// M4 Max: 20 candidates were 8250 tokens and 9.2 s, so the bound is 10
	// candidates. More parallel slots on the server made it slower.
	rerankMaxDocs  = 10
	rerankMaxBytes = 1200
)

// rerankSem admits one rerank call at a time. One call sends up to
// rerankMaxDocs sequences to the model server, which reads them in turn.
var rerankSem = make(chan struct{}, 1)

// NewReranker returns the reranker for the given server URL, model and
// per-call timeout, or nil when no model is configured. An empty (or zero)
// argument falls back to the environment, then to the default.
//
// Env vars:
//
//	MEMSTATE_RERANK_MODEL    (default empty: no reranker)
//	MEMSTATE_RERANK_URL      (default: the embedding server URL)
//	MEMSTATE_RERANK_TIMEOUT  (default 15s; Go duration syntax)
//
// The URL must be the base of an OpenAI-compatible API (it ends in /v1),
// because the request is POST {URL}/rerank. Ollama has no rerank API. For
// another URL the function prints the reason and returns nil.
func NewReranker(url, model string, timeout time.Duration, embeddingURL string) *Reranker {
	if model == "" {
		model = os.Getenv("MEMSTATE_RERANK_MODEL")
	}
	if model == "" {
		return nil
	}
	if url == "" {
		url = os.Getenv("MEMSTATE_RERANK_URL")
	}
	if url == "" {
		url = embeddingURL
	}
	if !isOpenAIBase(url) {
		fmt.Fprintf(os.Stderr, "memstated: reranker %s is off: %s is not an OpenAI-compatible base (it must end in /v1); set MEMSTATE_RERANK_URL\n",
			model, url)
		return nil
	}
	timeout = durationSetting(timeout, "MEMSTATE_RERANK_TIMEOUT", defaultRerankTimeout)
	return &Reranker{
		URL:     url,
		Model:   model,
		Client:  &http.Client{Timeout: timeout},
		Timeout: timeout,
	}
}

// timeout returns the per-call bound, or the default for a zero-value
// Reranker (tests build those directly).
func (r *Reranker) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return defaultRerankTimeout
}

// rerankResult is one entry of the server's answer: the position of a
// document in the request and its relevance score.
type rerankResult struct {
	Index int      `json:"index"`
	Score *float64 `json:"relevance_score"`
}

// Rerank returns one relevance score per document, in the order of docs.
// A higher score means a better answer to query. The request is the Jina
// rerank shape, which the llama.cpp server and vLLM serve at /v1/rerank.
func (r *Reranker) Rerank(ctx context.Context, query string, docs []string) ([]float32, error) {
	select {
	case rerankSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-rerankSem }()
	var out struct {
		Results []rerankResult `json:"results"`
	}
	err := postModelServer(ctx, r.Client, strings.TrimRight(r.URL, "/")+"/rerank", "rerank server /rerank",
		map[string]any{"model": r.Model, "query": query, "documents": docs, "top_n": len(docs)}, &out)
	if err != nil {
		return nil, err
	}
	return rerankScores(out.Results, len(docs))
}

// rerankScores checks the answer of a rerank server and returns the scores
// by document position. The answer must hold one finite score for each of
// the n documents. Anything else is an error, so a server that answers for
// another request shape cannot put the hits into a random order.
func rerankScores(results []rerankResult, n int) ([]float32, error) {
	if len(results) != n {
		return nil, fmt.Errorf("rerank server returned %d scores for %d documents", len(results), n)
	}
	scores := make([]float32, n)
	seen := make([]bool, n)
	for _, res := range results {
		if res.Index < 0 || res.Index >= n || seen[res.Index] {
			return nil, fmt.Errorf("rerank server returned a bad or repeated document index %d", res.Index)
		}
		if res.Score == nil {
			return nil, fmt.Errorf("rerank server returned no relevance_score for document %d", res.Index)
		}
		s := float32(*res.Score)
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
			return nil, fmt.Errorf("rerank server returned a score that is not finite for document %d", res.Index)
		}
		seen[res.Index] = true
		scores[res.Index] = s
	}
	return scores, nil
}

// rerankDoc is the text of one memory that the reranker reads: the keypath,
// which names the subject, and the part of the content that rerankWindow
// selects for the query.
func rerankDoc(m *Memory, query string) string {
	return m.Keypath + "\n" + rerankWindow(m.Content, query)
}

// rerankWindowStep is the distance in bytes between two windows that
// rerankWindow compares.
const rerankWindowStep = 300

// rerankWindow returns at most rerankMaxBytes of content: the window that
// holds the most different words of the query. Windows with an equal count
// give the earliest one, so content with no query word gives its head.
//
// The head alone is not sufficient. Measured 2026-11-19 on a memory of 2855
// bytes whose answer started at byte 1800: the reranker gave the head
// 0.000, the full text 0.035 and this window 0.725, and the search put the
// memory at rank 7 where the fusion had it at rank 1.
func rerankWindow(content, query string) string {
	if len(content) <= rerankMaxBytes {
		return content
	}
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) > 2 {
			words[w] = true
		}
	}
	best, bestCount := truncateBytes(content, rerankMaxBytes), -1
	for start := 0; start < len(content); start += rerankWindowStep {
		// Do not start in the middle of a character.
		for start < len(content) && !utf8.RuneStart(content[start]) {
			start++
		}
		window := truncateBytes(content[start:], rerankMaxBytes)
		lower := strings.ToLower(window)
		count := 0
		for w := range words {
			if strings.Contains(lower, w) {
				count++
			}
		}
		if count > bestCount {
			best, bestCount = window, count
		}
		if start+rerankMaxBytes >= len(content) {
			break
		}
	}
	return best
}

// applyRerank orders the first len(scores) hits by score, highest first,
// and stores the score on each of them. Equal scores keep the order of
// rrfFuse. The hits after len(scores) were not scored and stay behind the
// scored ones in their order.
func applyRerank(hits []*HybridHit, scores []float32) []*HybridHit {
	n := min(len(scores), len(hits))
	type scored struct {
		hit   *HybridHit
		score float32
	}
	head := make([]scored, n)
	for i := range n {
		head[i] = scored{hits[i], scores[i]}
	}
	sort.SliceStable(head, func(i, j int) bool { return head[i].score > head[j].score })
	out := make([]*HybridHit, 0, len(hits))
	for _, s := range head {
		score := s.score
		s.hit.RerankScore = &score
		out = append(out, s.hit)
	}
	return append(out, hits[n:]...)
}

// rerankPairs lists embedding models with a reranker that works with them.
// A reranker reads text, not vectors, so the pair needs no shared vector
// space: the rule is one model family and one server for both.
var rerankPairs = []struct{ embedPrefix, reranker, note string }{
	{"qwen3-embedding", "qwen3-reranker", "use the same size (0.6b, 4b, 8b); llama.cpp serves both"},
	{"bge-m3", "bge-reranker-v2-m3", "llama.cpp serves both"},
	{"nomic-embed", "bge-reranker-v2-m3", "needs a /v1 server for the reranker; Ollama has no rerank API"},
}

// rerankHint returns the suggestion for the embedding model, or "" when
// the list holds no pair for it.
func rerankHint(embedModel string) string {
	for _, p := range rerankPairs {
		if strings.HasPrefix(embedModel, p.embedPrefix) {
			return fmt.Sprintf("%s pairs with %s (%s)", embedModel, p.reranker, p.note)
		}
	}
	return ""
}
