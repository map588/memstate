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

// NewEmbedder reads config from env vars and returns an Embedder. It does NOT
// probe the server — the daemon starts even if Ollama is down, and writes
// that fail to embed silently degrade to FTS-only search.
//
// Env vars:
//
//	MEMSTATE_OLLAMA_URL    (default http://127.0.0.1:11434)
//	MEMSTATE_EMBED_MODEL   (default nomic-embed-text)
//
// Pass disable=true to return a nil embedder — caller branches on that.
func NewEmbedder() *Embedder {
	url := os.Getenv("MEMSTATE_OLLAMA_URL")
	if url == "" {
		url = "http://127.0.0.1:11434"
	}
	model := os.Getenv("MEMSTATE_EMBED_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}
	return &Embedder{
		URL:          url,
		Model:        model,
		Client:       &http.Client{Timeout: 10 * time.Second},
		errorLogCool: time.Hour,
	}
}

// taskPrefix returns the retrieval task prefix for the configured model.
// nomic-embed models are trained with "search_document:"/"search_query:"
// prefixes and retrieval quality degrades without them; other models get
// the raw text.
func (e *Embedder) taskPrefix(kind string) string {
	if strings.HasPrefix(e.Model, "nomic-embed") {
		return kind + ": "
	}
	return ""
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
		vec, err := e.Embed(ctx, e.taskPrefix("search_document")+doc)
		var se *embedStatusError
		if err == nil || len(doc) <= 512 ||
			!errors.As(err, &se) || !strings.Contains(se.msg, "context length") {
			return vec, err
		}
		doc = truncateBytes(doc, len(doc)/2)
	}
}

// EmbedQuery embeds a search query (the "query" side of retrieval).
func (e *Embedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return e.Embed(ctx, e.taskPrefix("search_query")+text)
}

// Embed returns the vector for text using the configured model.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]string{"model": e.Model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, "POST",
		e.URL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, &embedStatusError{
			msg: fmt.Sprintf("ollama /api/embeddings: %s: %s", resp.Status, string(raw)),
		}
	}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embedding) == 0 {
		return nil, errors.New("ollama returned empty embedding")
	}
	return out.Embedding, nil
}

// BackfillEmbeddings eagerly embeds, in the background, every current
// keypath that lacks a vector for the configured model. Run once at daemon
// startup: it repairs the wipe after an embed-scheme migration, populates a
// freshly switched MEMSTATE_EMBED_MODEL, and catches up on writes that
// happened while Ollama was down. Sequential on purpose — one in-flight
// Ollama call at a time — and it aborts on the first embed error (Ollama is
// down; the next startup or per-write heal retries). Nil receiver is a no-op.
func (e *Embedder) BackfillEmbeddings(store *Store) {
	if e == nil {
		return
	}
	e.inFlight.Go(func() {
		missing, err := store.ListMissingEmbeddings(e.Model)
		if err != nil {
			e.maybeLog(fmt.Sprintf("backfill: list missing: %v", err))
			return
		}
		if len(missing) == 0 {
			return
		}
		done, skipped := 0, 0
		for _, m := range missing {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			vec, err := e.EmbedDocument(ctx, m.Content)
			cancel()
			if err != nil {
				// Ollama rejected this input (e.g. context overflow): skip it
				// and keep going. A transport error means Ollama is down:
				// abort — the next startup or per-write heal retries.
				var se *embedStatusError
				if errors.As(err, &se) {
					skipped++
					fmt.Fprintf(os.Stderr, "memstated: backfill: skipping %s/%s: %v\n",
						m.ProjectID, m.Keypath, err)
					continue
				}
				e.maybeLog(fmt.Sprintf("backfill aborted after %d/%d: embed %s/%s: %v",
					done, len(missing), m.ProjectID, m.Keypath, err))
				return
			}
			if err := store.UpsertKeypathEmbedding(m.ProjectID, m.Keypath, e.Model,
				len(vec), packVector(vec)); err != nil {
				e.maybeLog(fmt.Sprintf("backfill aborted after %d/%d: upsert %s/%s: %v",
					done, len(missing), m.ProjectID, m.Keypath, err))
				return
			}
			done++
		}
		fmt.Fprintf(os.Stderr, "memstated: backfilled %d embeddings (model %s, %d skipped)\n",
			done, e.Model, skipped)
	})
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
