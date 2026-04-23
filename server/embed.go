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
	"sync"
	"time"
)

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
		return nil, fmt.Errorf("ollama /api/embeddings: %s: %s", resp.Status, string(raw))
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
const defaultThreshold = 0.6

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
