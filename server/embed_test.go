package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b  []float32
		want  float32
		label string
	}{
		{[]float32{1, 0, 0}, []float32{1, 0, 0}, 1.0, "identical"},
		{[]float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0, "opposite"},
		{[]float32{1, 0, 0}, []float32{0, 1, 0}, 0.0, "orthogonal"},
		{[]float32{3, 4}, []float32{6, 8}, 1.0, "collinear different mag"},
		{[]float32{1}, []float32{1, 2}, 0.0, "mismatch returns 0"},
		{[]float32{0, 0}, []float32{1, 1}, 0.0, "zero magnitude returns 0"},
	}
	for _, c := range cases {
		got := cosine(c.a, c.b)
		if math.Abs(float64(got-c.want)) > 1e-5 {
			t.Errorf("cosine(%v,%v) %s: got %v want %v", c.a, c.b, c.label, got, c.want)
		}
	}
}

func TestPackUnpackVector(t *testing.T) {
	v := []float32{0.1, -2.5, 3.14, 0, 1e-10}
	blob := packVector(v)
	if len(blob) != len(v)*4 {
		t.Fatalf("pack size %d want %d", len(blob), len(v)*4)
	}
	back, err := unpackVector(blob)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if len(back) != len(v) {
		t.Fatalf("roundtrip len: %d want %d", len(back), len(v))
	}
	for i := range v {
		if back[i] != v[i] {
			t.Errorf("element %d: got %v want %v", i, back[i], v[i])
		}
	}
}

// mockOllama returns a deterministic vector derived from the prompt — each
// distinct prompt produces a distinct unit vector, and the same prompt twice
// produces the same vector. Good enough to exercise write-embed + search.
func mockOllama(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			http.NotFound(w, r)
			return
		}
		var in struct{ Model, Prompt string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		// 8-dim vector seeded by prompt content.
		vec := make([]float32, 8)
		for i, ch := range in.Prompt {
			vec[i%8] += float32(ch) / 1000.0
		}
		// Normalize to unit length so cosine behaves like dot product.
		var mag float32
		for _, f := range vec {
			mag += f * f
		}
		mag = float32(math.Sqrt(float64(mag)))
		if mag > 0 {
			for i := range vec {
				vec[i] /= mag
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": vec})
	}))
}

func newTestEmbedder(t *testing.T, ollama *httptest.Server) *Embedder {
	t.Helper()
	return &Embedder{
		URL:          ollama.URL,
		Model:        "mock",
		Client:       &http.Client{Timeout: 2 * time.Second},
		errorLogCool: time.Hour,
	}
}

func TestEmbedderRoundtrip(t *testing.T) {
	ollama := mockOllama(t)
	defer ollama.Close()
	e := newTestEmbedder(t, ollama)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := e.Embed(ctx, "auth.provider")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(v) != 8 {
		t.Fatalf("vec len %d want 8", len(v))
	}
}

func TestHTTPEmbedOnWriteAndSemanticSearch(t *testing.T) {
	ollama := mockOllama(t)
	defer ollama.Close()
	embedder := newTestEmbedder(t, ollama)
	ts := newTestServerWithEmbedder(t, embedder)

	// Seed three keypaths with distinct topics so the mock produces distinct vectors.
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app",
		"content":    "## Authentication provider\n\nUsing SuperTokens.\n",
	})
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app",
		"content":    "## Database engine\n\nPostgres 15.\n",
	})
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app",
		"content":    "## Cache layer\n\nRedis cluster.\n",
	})

	// Deterministic: block until all fire-and-forget goroutines finish.
	embedder.WaitForPending()

	// Query == an existing keypath → identical mock-vector → cosine==1,
	// i.e. the top hit MUST be that keypath. This tests the "query finds
	// its matching keypath" property without depending on the mock's
	// arithmetic for similar strings.
	target := "my_app.database_engine"
	_, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "my_app",
		"query":      target,
		"mode":       "semantic",
		"threshold":  0.0,
		"limit":      10,
	})
	if body["mode"] != "semantic" {
		t.Fatalf("want semantic mode, got %+v", body["mode"])
	}
	results, _ := body["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("want 3 embedded keypaths in hits, got %d: %+v", len(results), results)
	}
	top := results[0].(map[string]any)
	if top["keypath"] != target {
		t.Fatalf("identical-string query must rank %s first, got %v: %+v",
			target, top["keypath"], top)
	}
	if top["score"].(float64) < 0.999 {
		t.Fatalf("identical-string similarity should be ~1.0, got %v", top["score"])
	}
}

func TestHTTPSemanticSearchWithoutEmbedderReturns503(t *testing.T) {
	ts := newTestServer(t) // nil embedder
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "keypath": "x", "content": "v",
	})
	code, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "anything", "mode": "semantic",
	})
	if code != 503 {
		t.Fatalf("want 503 without embedder, got %d %+v", code, body)
	}
}

func TestHTTPOllamaDownStillWrites(t *testing.T) {
	// Embedder points at an unreachable address; the daemon must still
	// accept writes and FTS searches.
	embedder := &Embedder{
		URL:          "http://127.0.0.1:1", // guaranteed unreachable
		Model:        "mock",
		Client:       &http.Client{Timeout: 50 * time.Millisecond},
		errorLogCool: time.Hour,
	}
	ts := newTestServerWithEmbedder(t, embedder)

	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "keypath": "auth", "content": "jwt",
	})
	if code != 200 {
		t.Fatalf("write should succeed when Ollama down: %d %+v", code, body)
	}
	// FTS still works.
	_, fts := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "jwt", "mode": "fts",
	})
	if int(fts["total_found"].(float64)) != 1 {
		t.Fatalf("fts should find the row: %+v", fts)
	}
}
