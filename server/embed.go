package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// embedStatusError marks an Ollama HTTP error response (server reachable,
// this input rejected) as opposed to a transport failure (server down).
// The backfill skips over the former and aborts on the latter.
type embedStatusError struct{ msg string }

func (e *embedStatusError) Error() string { return e.msg }

// Embedder calls a local Ollama server to produce a vector for a string
// (typically a keypath) and provides cosine-similarity ranking over cached
// vectors. A nil *Embedder means embeddings are disabled — handlers must
// treat that as a first-class state, not a misconfiguration.
type Embedder struct {
	URL    string
	Model  string
	Client *http.Client
	// Timeout bounds one Ollama call. It must cover a cold model load:
	// a 4B-parameter model takes 20s or more to load on first use.
	// Zero means defaultEmbedTimeout.
	Timeout time.Duration

	// errorLog throttles Ollama-unreachable warnings to once per model
	// per hour so a long outage doesn't flood stderr.
	errorLog     sync.Map // map[string]time.Time, keyed by model name
	errorLogCool time.Duration

	// inFlight tracks pending fire-and-forget embed goroutines so tests can
	// deterministically wait for them via WaitForPending. Prod never calls
	// Wait; the counter just exists.
	inFlight sync.WaitGroup
}

// WaitForPending blocks until every goroutine spawned by maybeEmbedKeypath
// has returned. Intended for test determinism; production code fires-and-
// forgets and never waits.
func (e *Embedder) WaitForPending() {
	if e == nil {
		return
	}
	e.inFlight.Wait()
}

// NewEmbedder returns an Embedder for the given Ollama URL, model, and
// per-call timeout. An empty (or zero) argument falls back to the
// environment, then to the default. It does NOT probe the server — the
// daemon starts even if Ollama is down, and writes that fail to embed
// silently degrade to FTS-only search.
//
// Env vars:
//
//	MEMSTATE_OLLAMA_URL      (default http://127.0.0.1:11434; a URL that ends
//	                         in /v1 selects an OpenAI-compatible API)
//	MEMSTATE_EMBED_MODEL     (default nomic-embed-text)
//	MEMSTATE_EMBED_TIMEOUT   (default 60s; Go duration syntax)
func NewEmbedder(url, model string, timeout time.Duration) *Embedder {
	if url == "" {
		url = os.Getenv("MEMSTATE_OLLAMA_URL")
	}
	if url == "" {
		url = defaultOllamaURL
	}
	if model == "" {
		model = os.Getenv("MEMSTATE_EMBED_MODEL")
	}
	if model == "" {
		model = defaultEmbedModel
	}
	if timeout == 0 {
		if v := os.Getenv("MEMSTATE_EMBED_TIMEOUT"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				fmt.Fprintf(os.Stderr, "memstated: bad MEMSTATE_EMBED_TIMEOUT %q, using %s\n",
					v, defaultEmbedTimeout)
			} else {
				timeout = d
			}
		}
	}
	if timeout == 0 {
		timeout = defaultEmbedTimeout
	}
	return &Embedder{
		URL:          url,
		Model:        model,
		Client:       &http.Client{Timeout: timeout},
		Timeout:      timeout,
		errorLogCool: time.Hour,
	}
}

const (
	defaultOllamaURL    = "http://127.0.0.1:11434"
	defaultEmbedModel   = "nomic-embed-text"
	defaultEmbedTimeout = 60 * time.Second
)

// timeout returns the per-call bound, or the default for a zero-value
// Embedder (tests build those directly).
func (e *Embedder) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return defaultEmbedTimeout
}

// qwenQueryInstruct is the retrieval instruction Qwen3-Embedding models
// expect on the query side. Documents are embedded without an instruction.
// Wording measured 2026-09-06 on 40 queries over 661 memories: this task
// description gave MRR 0.74 on vague queries against 0.66 for a generic
// "retrieve memories" wording and 0.62 with no instruction.
const qwenQueryInstruct = "Instruct: Given a question about a software project, " +
	"retrieve the engineering note that answers it\nQuery: "

// queryText wraps a search query in the retrieval format the configured
// model family was trained with. nomic-embed models want a
// "search_query: " prefix; Qwen3-Embedding models want an instruction
// line; other models get the raw text.
func (e *Embedder) queryText(text string) string {
	switch {
	case strings.HasPrefix(e.Model, "nomic-embed"):
		return "search_query: " + text
	case strings.HasPrefix(e.Model, "qwen3-embedding"):
		return qwenQueryInstruct + text
	}
	return text
}

// documentText wraps stored content in the document-side format for the
// configured model family. Only nomic-embed models need a prefix.
func (e *Embedder) documentText(text string) string {
	if strings.HasPrefix(e.Model, "nomic-embed") {
		return "search_document: " + text
	}
	return text
}

// embedMaxBytes caps document text sent to the embedder. Ollama's default
// context window is 2048 tokens; byte length maps to tokens at a density
// that varies with the text, so this is only the first cut — EmbedDocument
// halves and retries on context-overflow errors until the model accepts.
// Embedding a long memory by its head is a fine retrieval approximation.
const embedMaxBytes = 6000

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	// Walk back over a split multi-byte rune at the boundary.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// EmbedDocument embeds stored content (the "document" side of retrieval).
// Content that overflows the model's context window is halved and retried
// until it fits, so token-dense text cannot permanently fail to embed.
func (e *Embedder) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	doc := truncateBytes(text, embedMaxBytes)
	for {
		vec, err := e.Embed(ctx, e.documentText(doc))
		var se *embedStatusError
		if err == nil || len(doc) <= 512 ||
			!errors.As(err, &se) || !contextOverflow(se.msg) {
			return vec, err
		}
		doc = truncateBytes(doc, len(doc)/2)
	}
}

// contextOverflow reports whether an embedding server rejected the input
// because it is longer than the model's context. Ollama says the input
// "exceeds the context length"; the llama.cpp server says it "is too large
// to process".
func contextOverflow(msg string) bool {
	return strings.Contains(msg, "context length") || strings.Contains(msg, "too large to process")
}

// EmbedQuery embeds a search query (the "query" side of retrieval).
func (e *Embedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return e.Embed(ctx, e.queryText(text))
}

// openAIBase reports whether URL is the base of an OpenAI-compatible API.
// By convention such a base ends in /v1: the llama.cpp server, LM Studio,
// vLLM, and Ollama's own /v1 all serve POST {base}/embeddings.
func (e *Embedder) openAIBase() bool {
	return strings.HasSuffix(strings.TrimRight(e.URL, "/"), "/v1")
}

// Embed returns the vector for text using the configured model. It uses
// Ollama's native API (POST {URL}/api/embeddings) unless URL ends in /v1,
// where it uses the OpenAI embeddings API (POST {URL}/embeddings).
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e.openAIBase() {
		var out struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		err := e.post(ctx, strings.TrimRight(e.URL, "/")+"/embeddings", "openai-compatible /embeddings",
			map[string]string{"model": e.Model, "input": text}, &out)
		if err != nil {
			return nil, err
		}
		if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
			return nil, errors.New("embedding server returned empty embedding")
		}
		return out.Data[0].Embedding, nil
	}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	err := e.post(ctx, e.URL+"/api/embeddings", "ollama /api/embeddings",
		map[string]string{"model": e.Model, "prompt": text}, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Embedding) == 0 {
		return nil, errors.New("ollama returned empty embedding")
	}
	return out.Embedding, nil
}

// post sends payload as JSON to url and decodes the reply into out. A reply
// other than 200 becomes an embedStatusError (server reachable, input
// rejected); a transport error comes back as is (server down). what names
// the endpoint in the error message.
func (e *Embedder) post(ctx context.Context, url, what string, payload, out any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return &embedStatusError{msg: fmt.Sprintf("%s: %s: %s", what, resp.Status, string(raw))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// BackfillEmbeddings eagerly embeds, in the background, every current
// keypath that lacks a vector for the configured model. Run once at daemon
// startup: it repairs the wipe after an embed-scheme migration, populates a
// freshly switched embed model, and catches up on writes that happened
// while Ollama was down. Failures log and return; the next startup or
// per-write heal retries. Nil receiver is a no-op.
func (e *Embedder) BackfillEmbeddings(store *Store) {
	if e == nil {
		return
	}
	e.inFlight.Go(func() {
		done, skipped, err := e.Backfill(store, io.Discard)
		if err != nil {
			e.maybeLog(fmt.Sprintf("backfill aborted after %d: %v", done, err))
			return
		}
		if done+skipped > 0 {
			fmt.Fprintf(os.Stderr, "memstated: backfilled %d embeddings (model %s, %d skipped)\n",
				done, e.Model, skipped)
		}
	})
}

// Backfill synchronously embeds every current keypath that lacks a vector
// for the configured model and reports how many it embedded and skipped.
// Sequential on purpose — one in-flight Ollama call at a time. Ollama
// rejecting one input (e.g. context overflow) skips that keypath; a
// transport error means Ollama is down and aborts the run. Progress lines
// go to progress, one per keypath.
func (e *Embedder) Backfill(store *Store, progress io.Writer) (done, skipped int, err error) {
	missing, err := store.ListMissingEmbeddings(e.Model)
	if err != nil {
		return 0, 0, fmt.Errorf("list missing: %w", err)
	}
	for i, m := range missing {
		ctx, cancel := context.WithTimeout(context.Background(), e.timeout())
		vec, err := e.EmbedDocument(ctx, m.Content)
		cancel()
		if err != nil {
			var se *embedStatusError
			if errors.As(err, &se) {
				skipped++
				fmt.Fprintf(os.Stderr, "memstated: backfill: skipping %s/%s: %v\n",
					m.ProjectID, m.Keypath, err)
				continue
			}
			return done, skipped, fmt.Errorf("embed %s/%s: %w", m.ProjectID, m.Keypath, err)
		}
		if err := store.UpsertKeypathEmbedding(m.ProjectID, m.Keypath, e.Model,
			len(vec), packVector(vec)); err != nil {
			return done, skipped, fmt.Errorf("upsert %s/%s: %w", m.ProjectID, m.Keypath, err)
		}
		done++
		fmt.Fprintf(progress, "[%d/%d] %s/%s (dim %d)\n", i+1, len(missing), m.ProjectID, m.Keypath, len(vec))
	}
	return done, skipped, nil
}

// Rebuild drops every vector stored for the configured model and embeds
// all current keypaths again. Use it when the model's prompt format or
// output changes, so no stale vector survives under the same model name.
func (e *Embedder) Rebuild(store *Store, progress io.Writer) (dropped int64, done, skipped int, err error) {
	dropped, err = store.DeleteEmbeddingsForModel(e.Model)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("drop vectors: %w", err)
	}
	done, skipped, err = e.Backfill(store, progress)
	return dropped, done, skipped, err
}

// maybeLog emits a warning to stderr no more than once per cool-down window
// per model, so Ollama being down for an hour produces one log line, not 3600.
func (e *Embedder) maybeLog(msg string) {
	now := time.Now()
	if prev, ok := e.errorLog.Load(e.Model); ok {
		if last, ok := prev.(time.Time); ok && now.Sub(last) < e.errorLogCool {
			return
		}
	}
	e.errorLog.Store(e.Model, now)
	fmt.Fprintf(os.Stderr, "memstated: embedder: %s\n", msg)
}

// packVector serializes a float32 slice as little-endian bytes for BLOB storage.
func packVector(v []float32) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(v)*4))
	_ = binary.Write(buf, binary.LittleEndian, v)
	return buf.Bytes()
}

// unpackVector reverses packVector.
func unpackVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("vector blob length %d not multiple of 4", len(b))
	}
	out := make([]float32, len(b)/4)
	if err := binary.Read(bytes.NewReader(b), binary.LittleEndian, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// cosine returns the cosine similarity of two equal-length vectors.
// Returns 0 for any degenerate input (length mismatch, zero magnitude).
func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	denom := float32(math.Sqrt(float64(na)) * math.Sqrt(float64(nb)))
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// defaultThreshold is the cosine-similarity floor below which semantic hits
// are discarded. Tuned loosely for nomic-embed-text; callers can override
// per-request or globally via MEMSTATE_SEMANTIC_THRESHOLD.
const defaultThreshold = 0.5

// envThreshold returns the threshold override from MEMSTATE_SEMANTIC_THRESHOLD
// (parsed as float), or defaultThreshold if unset or unparseable.
func envThreshold() float32 {
	raw := os.Getenv("MEMSTATE_SEMANTIC_THRESHOLD")
	if raw == "" {
		return defaultThreshold
	}
	f, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		return defaultThreshold
	}
	return float32(f)
}
