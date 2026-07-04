package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
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
		URL:    ollama.URL,
		Model:  "mock",
		Client: &http.Client{Timeout: 2 * time.Second},
		// No throttle in tests — we want every goroutine error visible.
		errorLogCool: 0,
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
	for _, c := range []string{
		"## Authentication provider\n\nUsing SuperTokens.\n",
		"## Database engine\n\nPostgres 15.\n",
		"## Cache layer\n\nRedis cluster.\n",
	} {
		code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
			"project_id": "my_app",
			"content":    c,
		})
		if code != 200 {
			t.Fatalf("remember failed %d: %+v (content=%q)", code, body, c)
		}
	}

	// Deterministic: block until all fire-and-forget goroutines finish.
	embedder.WaitForPending()

	// Vectors are computed from CONTENT. Query == an existing section's
	// content → identical mock-vector → cosine==1, i.e. the top hit MUST be
	// the keypath holding that content. This tests the "query finds its
	// matching content" property without depending on the mock's arithmetic
	// for similar strings.
	_, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "my_app",
		"query":      "Postgres 15.",
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
	if top["keypath"] != "my_app.database_engine" {
		t.Fatalf("identical-content query must rank my_app.database_engine first, got %v: %+v",
			top["keypath"], top)
	}
	if top["score"].(float64) < 0.999 {
		t.Fatalf("identical-content similarity should be ~1.0, got %v", top["score"])
	}
}

func TestEmbedContentReEmbedOnChangeAndDeleteOnTombstone(t *testing.T) {
	ollama := mockOllama(t)
	defer ollama.Close()
	embedder := newTestEmbedder(t, ollama)
	store := newTestStore(t)
	ts := httptest.NewServer(newRouter(store, nil, embedder))
	t.Cleanup(ts.Close)

	write := func(content string) {
		code, body := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
			"project_id": "p", "keypath": "k", "content": content,
		})
		if code != 200 {
			t.Fatalf("store: %d %+v", code, body)
		}
		embedder.WaitForPending()
	}

	vector := func() []byte {
		var blob []byte
		err := store.db.QueryRow(
			`SELECT vector FROM keypath_embeddings WHERE project_id='p' AND keypath='k'`,
		).Scan(&blob)
		if err != nil {
			t.Fatalf("read vector: %v", err)
		}
		return blob
	}

	write("first content")
	v1 := vector()

	// Superseding write must re-embed from the new content.
	write("second content entirely different")
	v2 := vector()
	if bytes.Equal(v1, v2) {
		t.Fatal("vector not recomputed after content change")
	}

	// Tombstone must remove the embedding row entirely.
	postJSON(t, ts.URL+"/api/v1/memories/delete", map[string]any{
		"project_id": "p", "keypath": "k",
	})
	has, err := store.HasKeypathEmbedding("p", "k", "mock")
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if has {
		t.Fatal("embedding row should be deleted on tombstone")
	}
}

func TestEmbedHealsMissingVectorOnUnchangedWrite(t *testing.T) {
	ollama := mockOllama(t)
	defer ollama.Close()
	embedder := newTestEmbedder(t, ollama)

	// First write goes through a server whose embedder is unreachable, so no
	// vector lands.
	deadEmbedder := &Embedder{
		URL:          "http://127.0.0.1:1",
		Model:        "mock",
		Client:       &http.Client{Timeout: 50 * time.Millisecond},
		errorLogCool: time.Hour,
	}
	store := newTestStore(t)
	tsDead := httptest.NewServer(newRouter(store, nil, deadEmbedder))
	postJSON(t, tsDead.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v",
	})
	deadEmbedder.WaitForPending()
	tsDead.Close()

	// Same store, healthy embedder, identical content: action=unchanged but
	// the missing vector must be healed.
	ts := httptest.NewServer(newRouter(store, nil, embedder))
	t.Cleanup(ts.Close)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v",
	})
	if code != 200 || body["action"] != "unchanged" {
		t.Fatalf("expected unchanged rewrite: %d %+v", code, body)
	}
	embedder.WaitForPending()

	has, err := store.HasKeypathEmbedding("p", "k", "mock")
	if err != nil || !has {
		t.Fatalf("unchanged write should heal missing embedding: has=%v err=%v", has, err)
	}
}

func TestEmbedderTaskPrefixes(t *testing.T) {
	nomic := &Embedder{Model: "nomic-embed-text"}
	if got := nomic.taskPrefix("search_query"); got != "search_query: " {
		t.Fatalf("nomic prefix: %q", got)
	}
	other := &Embedder{Model: "mxbai-embed-large"}
	if got := other.taskPrefix("search_query"); got != "" {
		t.Fatalf("non-nomic models must get no prefix: %q", got)
	}
}

func TestMigrationWipesForeignEmbedSource(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/m.db"
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.UpsertKeypathEmbedding("p", "k", "mock", 2, packVector([]float32{1, 0})); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Simulate a DB written under a different embedding scheme.
	if _, err := s.db.Exec(`UPDATE meta SET value='keypath' WHERE key='embed_source'`); err != nil {
		t.Fatalf("update meta: %v", err)
	}
	s.Close()

	s2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	has, err := s2.HasKeypathEmbedding("p", "k", "mock")
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if has {
		t.Fatal("vectors from a different embed_source must be wiped on open")
	}
	var src string
	if err := s2.db.QueryRow(`SELECT value FROM meta WHERE key='embed_source'`).Scan(&src); err != nil || src != embedSource {
		t.Fatalf("embed_source not stamped: %q err=%v", src, err)
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

func TestBackfillEmbeddings(t *testing.T) {
	ollama := mockOllama(t)
	defer ollama.Close()
	store := newTestStore(t)

	// Rows written with no embedder: no vectors exist yet.
	_, _, _ = store.Write("p", "a", "alpha content", WriteMeta{}, false)
	_, _, _ = store.Write("p", "b", "bravo content", WriteMeta{}, false)
	_, _, _ = store.Write("p", "dead", "tombstoned content", WriteMeta{}, false)
	_, _, _ = store.Delete("p", "dead")
	_, _, _ = store.Write("gone", "k", "deleted project content", WriteMeta{}, false)
	_ = store.DeleteProject("gone")

	missing, err := store.ListMissingEmbeddings("mock")
	if err != nil {
		t.Fatalf("list missing: %v", err)
	}
	if len(missing) != 2 {
		t.Fatalf("want 2 missing (tombstone + deleted project excluded), got %d: %+v",
			len(missing), missing)
	}

	embedder := newTestEmbedder(t, ollama)
	embedder.BackfillEmbeddings(store)
	embedder.WaitForPending()

	for _, kp := range []string{"a", "b"} {
		has, err := store.HasKeypathEmbedding("p", kp, "mock")
		if err != nil || !has {
			t.Fatalf("backfill missed p/%s: has=%v err=%v", kp, has, err)
		}
	}
	if has, _ := store.HasKeypathEmbedding("p", "dead", "mock"); has {
		t.Fatal("backfill must skip tombstoned keypaths")
	}
	if has, _ := store.HasKeypathEmbedding("gone", "k", "mock"); has {
		t.Fatal("backfill must skip soft-deleted projects")
	}
	if left, _ := store.ListMissingEmbeddings("mock"); len(left) != 0 {
		t.Fatalf("nothing should remain missing: %+v", left)
	}

	// Second run is a no-op (nothing missing) and must not error or hang.
	embedder.BackfillEmbeddings(store)
	embedder.WaitForPending()
}

func TestBackfillAbortsWhenOllamaDown(t *testing.T) {
	store := newTestStore(t)
	_, _, _ = store.Write("p", "a", "content", WriteMeta{}, false)

	dead := &Embedder{
		URL:          "http://127.0.0.1:1",
		Model:        "mock",
		Client:       &http.Client{Timeout: 50 * time.Millisecond},
		errorLogCool: time.Hour,
	}
	dead.BackfillEmbeddings(store)
	dead.WaitForPending() // must return promptly, not spin through timeouts

	if has, _ := store.HasKeypathEmbedding("p", "a", "mock"); has {
		t.Fatal("no vector should exist after failed backfill")
	}
}

func TestEmbedDocumentHalvesOnContextOverflow(t *testing.T) {
	// Mock rejects prompts over 1000 bytes with Ollama's context-length
	// error; EmbedDocument must halve and retry until accepted.
	var accepted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Model, Prompt string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if len(in.Prompt) > 1000 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"the input length exceeds the context length"}`))
			return
		}
		accepted = in.Prompt
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{1, 0}})
	}))
	defer srv.Close()

	e := &Embedder{URL: srv.URL, Model: "mock", Client: &http.Client{Timeout: 2 * time.Second}}
	big := strings.Repeat("dense-token-text ", 500) // 8500 bytes
	vec, err := e.EmbedDocument(context.Background(), big)
	if err != nil || len(vec) != 2 {
		t.Fatalf("embed should succeed after halving: vec=%v err=%v", vec, err)
	}
	if len(accepted) == 0 || len(accepted) > 1000 {
		t.Fatalf("accepted prompt should be halved under the limit, got %d bytes", len(accepted))
	}
}
