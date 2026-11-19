package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"pgregory.net/rapid"
)

// mockRerank serves POST /v1/rerank in the Jina shape of the llama.cpp
// server. score gives the relevance of one document; answer, when set,
// replaces the whole reply. Each request body is appended to seen.
type mockRerank struct {
	score  func(query, doc string) float64
	answer func(w http.ResponseWriter, n int)
	seen   []map[string]any
}

func (m *mockRerank) start(t *testing.T) *Reranker {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/rerank" {
			http.NotFound(w, r)
			return
		}
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		m.seen = append(m.seen, raw)
		docs, _ := raw["documents"].([]any)
		if m.answer != nil {
			m.answer(w, len(docs))
			return
		}
		query, _ := raw["query"].(string)
		results := make([]map[string]any, 0, len(docs))
		for i, d := range docs {
			results = append(results, map[string]any{"index": i, "relevance_score": m.score(query, d.(string))})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "results": results})
	}))
	t.Cleanup(srv.Close)
	return &Reranker{URL: srv.URL + "/v1", Model: "mock-reranker", Client: &http.Client{Timeout: 2 * time.Second}}
}

// A document that holds "target" is the answer; the others are not.
func targetScore(_, doc string) float64 {
	if strings.Contains(doc, "target") {
		return 0.9
	}
	return 0.1
}

// rerankFixture returns a server whose store holds five memories that all
// match the word "deploy". FTS ranks the short ones first; the memory with
// "target" is the one a reranker must put first.
func rerankFixture(t *testing.T, reranker *Reranker) string {
	t.Helper()
	store := newTestStore(t)
	for i, content := range []string{
		"deploy deploy deploy",
		"deploy deploy notes",
		"deploy checklist for the staging host",
		"deploy steps that nobody reads any more, kept for history",
		"the target answer: how the release reaches production after deploy",
	} {
		if _, _, err := store.Write("p", fmt.Sprintf("notes.n%d", i), content, WriteMeta{}, false); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(newRouter(store, nil, nil, reranker))
	t.Cleanup(ts.Close)
	return ts.URL + "/api/v1/memories/search"
}

func resultKeypaths(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, r := range body["results"].([]any) {
		out = append(out, r.(map[string]any)["keypath"].(string))
	}
	return out
}

func TestHybridSearchReranks(t *testing.T) {
	mock := &mockRerank{score: targetScore}
	url := rerankFixture(t, mock.start(t))

	// rerank=false is the order of rrfFuse: the baseline.
	code, plain := postJSON(t, url, map[string]any{"project_id": "p", "query": "deploy", "rerank": false})
	if code != 200 {
		t.Fatalf("plain search: %d %+v", code, plain)
	}
	base := resultKeypaths(t, plain)
	if len(base) != 5 || base[0] == "notes.n4" {
		t.Fatalf("the fixture must not rank the target first without a reranker: %v", base)
	}
	if _, has := plain["reranker"]; has || len(mock.seen) != 0 {
		t.Fatalf("rerank=false must not call the reranker: %+v, %d calls", plain, len(mock.seen))
	}

	// The default reranks, and a limit below the number of hits still
	// lets the reranker read each hit.
	code, body := postJSON(t, url, map[string]any{"project_id": "p", "query": "deploy", "limit": 2})
	if code != 200 {
		t.Fatalf("reranked search: %d %+v", code, body)
	}
	got := resultKeypaths(t, body)
	if len(got) != 2 || got[0] != "notes.n4" {
		t.Fatalf("the reranker must put the target first and the limit must hold: %v", got)
	}
	if body["reranker"] != "mock-reranker" {
		t.Fatalf("the response must name the reranker: %+v", body)
	}
	first := body["results"].([]any)[0].(map[string]any)
	if first["rerank_score"].(float64) < 0.89 || first["score"] == nil {
		t.Fatalf("a reranked hit carries rerank_score and keeps score: %+v", first)
	}
	if src := first["sources"].([]any); len(src) != 1 || src[0] != "fts" {
		t.Fatalf("a rerank must not change sources: %v", src)
	}
	// Equal scores keep the order of rrfFuse.
	if want := slices.DeleteFunc(slices.Clone(base), func(k string) bool { return k == "notes.n4" })[0]; got[1] != want {
		t.Fatalf("second hit: got %s, want the first of the baseline, %s", got[1], want)
	}
	sent := mock.seen[0]
	if sent["model"] != "mock-reranker" || sent["query"] != "deploy" || len(sent["documents"].([]any)) != 5 {
		t.Fatalf("request to the rerank server: %+v", sent)
	}
	if doc := sent["documents"].([]any)[0].(string); !strings.HasPrefix(doc, base[0]+"\n") {
		t.Fatalf("a document starts with its keypath: %q", doc)
	}
}

func TestHybridSearchKeepsFusionOrderWhenRerankFails(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, n int){
		"server error": func(w http.ResponseWriter, n int) { http.Error(w, "boom", 500) },
		"too few scores": func(w http.ResponseWriter, n int) {
			_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":1}]}`))
		},
		"repeated index": func(w http.ResponseWriter, n int) {
			res := make([]map[string]any, n)
			for i := range res {
				res[i] = map[string]any{"index": 0, "relevance_score": 1}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": res})
		},
		"no score": func(w http.ResponseWriter, n int) {
			res := make([]map[string]any, n)
			for i := range res {
				res[i] = map[string]any{"index": i}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": res})
		},
		"slow": func(w http.ResponseWriter, n int) { time.Sleep(300 * time.Millisecond) },
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			reranker := (&mockRerank{answer: answer}).start(t)
			reranker.Timeout = 100 * time.Millisecond
			url := rerankFixture(t, reranker)
			_, plain := postJSON(t, url, map[string]any{"project_id": "p", "query": "deploy", "rerank": false})
			code, body := postJSON(t, url, map[string]any{"project_id": "p", "query": "deploy"})
			if code != 200 {
				t.Fatalf("a failed rerank must not fail the search: %d %+v", code, body)
			}
			if !slices.Equal(resultKeypaths(t, body), resultKeypaths(t, plain)) {
				t.Fatalf("order: got %v, want the order of rrfFuse %v", resultKeypaths(t, body), resultKeypaths(t, plain))
			}
			reason, _ := body["rerank_degraded"].(string)
			if !strings.HasPrefix(reason, "rerank unavailable: ") || body["reranker"] != nil {
				t.Fatalf("the response must give the reason and no reranker name: %+v", body)
			}
		})
	}
}

func TestRerankRequestWithoutReranker(t *testing.T) {
	url := rerankFixture(t, nil)
	code, body := postJSON(t, url, map[string]any{"project_id": "p", "query": "deploy", "rerank": true})
	if code != 400 || !strings.Contains(fmt.Sprint(body["error"]), "MEMSTATE_RERANK_MODEL") {
		t.Fatalf("rerank=true with no reranker must be refused: %d %+v", code, body)
	}
	// Without the field, and with false, the search answers as before.
	for _, req := range []map[string]any{
		{"project_id": "p", "query": "deploy"},
		{"project_id": "p", "query": "deploy", "rerank": false},
	} {
		if code, body := postJSON(t, url, req); code != 200 || body["reranker"] != nil || body["rerank_degraded"] != nil {
			t.Fatalf("%v: %d %+v", req, code, body)
		}
	}
	// rerank=true in another mode is an error also when a reranker exists.
	url = rerankFixture(t, (&mockRerank{score: targetScore}).start(t))
	code, body = postJSON(t, url, map[string]any{"project_id": "p", "query": "deploy", "mode": "fts", "rerank": true})
	if code != 400 {
		t.Fatalf("rerank=true with mode fts must be refused: %d %+v", code, body)
	}
}

// A memory of several facts holds its answer after the first 1200 bytes.
// On 2026-11-19 the query "shortcut does nothing on mac when key injected"
// had its memory at rank 1 without the reranker and at rank 7 with it: the
// reranker read only the head, which is about other faults.
func TestRerankReadsThePartThatMatchesTheQuery(t *testing.T) {
	filler := strings.Repeat("The first fault is about window focus after a resize of the display. ", 30) // 2070 bytes
	long := filler + "Fourth fault: a shortcut does nothing on a Mac when the key is injected, because the target answer is the modifier flag."
	// The summary node is a partial answer, as it was for the real model.
	mock := &mockRerank{score: func(q, doc string) float64 {
		if strings.HasPrefix(doc, "gotchas\n") {
			return 0.5
		}
		return targetScore(q, doc)
	}}
	store := newTestStore(t)
	for kp, content := range map[string]string{
		"gotchas":                "shortcut key mac injected: a short summary of the faults",
		"gotchas.four_faults":    long,
		"gotchas.other_shortcut": "shortcut key mac injected in another subject",
	} {
		if _, _, err := store.Write("p", kp, content, WriteMeta{}, false); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(newRouter(store, nil, nil, mock.start(t)))
	t.Cleanup(ts.Close)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/search",
		map[string]any{"project_id": "p", "query": "shortcut does nothing on mac when key injected"})
	if got := resultKeypaths(t, body); code != 200 || got[0] != "gotchas.four_faults" {
		t.Fatalf("the memory with the answer below byte 1200 must come first: %d %v", code, got)
	}
}

// rerankWindow returns a part of the content that the reranker can read:
// in one piece, within the byte bound, and valid text.
func TestRerankWindowProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		piece := rapid.SampledFrom([]string{"alpha ", "béta ", "日本語 ", "word ", "line\n"})
		// Most draws are longer than the bound, so the window matters.
		content := strings.Join(rapid.SliceOfN(piece, 0, 900).Draw(t, "pieces"), "")
		if rapid.Bool().Draw(t, "long") {
			content += strings.Repeat("filler text ", 200)
		}
		query := rapid.SampledFrom([]string{"", "needle", "needle haystack", "alpha", "日本語 needle"}).Draw(t, "query")
		at := -1
		if rapid.Bool().Draw(t, "with_needle") {
			at = rapid.IntRange(0, len(content)).Draw(t, "at")
			for at < len(content) && !utf8.RuneStart(content[at]) {
				at++
			}
			content = content[:at] + " needle " + content[at:]
		}
		got := rerankWindow(content, query)
		if len(got) > rerankMaxBytes || !utf8.ValidString(got) || !strings.Contains(content, got) {
			t.Fatalf("window of %d bytes is not a valid part of the content within %d bytes", len(got), rerankMaxBytes)
		}
		if len(content) <= rerankMaxBytes && got != content {
			t.Fatalf("content within the bound must come back whole")
		}
		if query == "" && got != truncateBytes(content, rerankMaxBytes) {
			t.Fatalf("no query word: the window must be the head")
		}
		// A query word that the content holds one time is in the window,
		// when no other query word competes with it.
		if at >= 0 && query == "needle" && strings.Count(content, "needle") == 1 && !strings.Contains(got, "needle") {
			t.Fatalf("needle at byte %d of %d is not in the window", at, len(content))
		}
	})
}

// The CLI must show that a reranker ordered the hits, and why not when it
// did not. Before this test the CLI printed neither: it decoded the reply
// into a struct without the fields.
func TestCLISearchShowsRerank(t *testing.T) {
	addrOf := func(url string) string {
		return strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/api/v1/memories/search")
	}
	t.Setenv("MEMSTATE_ADDR", addrOf(rerankFixture(t, (&mockRerank{score: targetScore}).start(t))))
	code, out := runCLI(t, "search", "deploy", "--project", "p", "--json")
	res := decodeCLI(t, out)
	first, _ := res["results"].([]any)[0].(map[string]any)
	if code != 0 || res["reranker"] != "mock-reranker" || first["keypath"] != "notes.n4" || first["rerank_score"] == nil {
		t.Fatalf("--json must carry reranker and rerank_score: %d %v", code, res)
	}
	if _, out = runCLI(t, "search", "deploy", "--project", "p", "--no-color"); !strings.Contains(out, "reranked by mock-reranker") {
		t.Fatalf("the head line must name the reranker:\n%s", out)
	}

	failing := (&mockRerank{answer: func(w http.ResponseWriter, n int) { http.Error(w, "boom", 500) }}).start(t)
	t.Setenv("MEMSTATE_ADDR", addrOf(rerankFixture(t, failing)))
	if _, out = runCLI(t, "search", "deploy", "--project", "p", "--no-color"); !strings.Contains(out, "rerank unavailable") {
		t.Fatalf("the head line must say that the rerank failed:\n%s", out)
	}
}

// The recall hook runs on each prompt: its search must not wait for the
// reranker.
func TestRecallSearchDoesNotRerank(t *testing.T) {
	mock := &mockRerank{score: targetScore}
	url := rerankFixture(t, mock.start(t))
	addr := strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/api/v1/memories/search")
	hits, err := recallSearch(addr, "p", "deploy")
	if err != nil || len(hits) == 0 {
		t.Fatalf("recallSearch: %d hits, %v", len(hits), err)
	}
	if len(mock.seen) != 0 {
		t.Fatalf("recallSearch called the reranker %d times", len(mock.seen))
	}
}

func TestApplyRerankProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 30).Draw(t, "hits")
		hits := make([]*HybridHit, n)
		var before []string
		for i := range hits {
			hits[i] = &HybridHit{Memory: &Memory{ProjectID: "p", Keypath: fmt.Sprintf("kp_%d", i)}, Sources: []string{"fts"}}
			before = append(before, hits[i].Keypath)
		}
		// Few distinct values, so that equal scores occur.
		scores := rapid.SliceOfN(rapid.SampledFrom([]float32{-3, 0, 0.25, 0.5, 7}), 0, n).Draw(t, "scores")
		k := len(scores)

		out := applyRerank(hits, scores)

		var after []string
		for _, h := range out {
			after = append(after, h.Keypath)
		}
		// 1. The output is a permutation of the input.
		sortedBefore, sortedAfter := slices.Sorted(slices.Values(before)), slices.Sorted(slices.Values(after))
		if !slices.Equal(sortedBefore, sortedAfter) {
			t.Fatalf("not a permutation: %v -> %v", before, after)
		}
		// 2. The hits that were not scored keep their place and have no score.
		if !slices.Equal(after[k:], before[k:]) {
			t.Fatalf("unscored tail moved: %v -> %v", before[k:], after[k:])
		}
		for _, h := range out[k:] {
			if h.RerankScore != nil {
				t.Fatalf("%s was not scored but has a score", h.Keypath)
			}
		}
		// 3. The scored hits are in score order, each with its own score,
		// and equal scores keep the input order.
		for i, h := range out[:k] {
			var idx int
			_, _ = fmt.Sscanf(h.Keypath, "kp_%d", &idx)
			if h.RerankScore == nil || *h.RerankScore != scores[idx] {
				t.Fatalf("%s: score %v, want %v", h.Keypath, h.RerankScore, scores[idx])
			}
			if i == 0 {
				continue
			}
			var prev int
			_, _ = fmt.Sscanf(out[i-1].Keypath, "kp_%d", &prev)
			if scores[prev] < scores[idx] || (scores[prev] == scores[idx] && prev > idx) {
				t.Fatalf("order broken at %d: %v with scores %v", i, after, scores)
			}
		}
		// 4. Sources are untouched: the recall rule reads them.
		for _, h := range out {
			if !slices.Equal(h.Sources, []string{"fts"}) {
				t.Fatalf("sources changed: %v", h.Sources)
			}
		}
	})
}

func TestRerankScoresProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 25).Draw(t, "documents")
		want := rapid.SliceOfN(rapid.Float64Range(-50, 50), n, n).Draw(t, "scores")
		order := rapid.Permutation(slices.Collect(func(yield func(int) bool) {
			for i := range n {
				if !yield(i) {
					return
				}
			}
		})).Draw(t, "order")
		results := make([]rerankResult, n)
		for i, idx := range order {
			results[i] = rerankResult{Index: idx, Score: &want[idx]}
		}
		// A complete answer in any order gives each document its score.
		got, err := rerankScores(results, n)
		if err != nil {
			t.Fatalf("complete answer refused: %v", err)
		}
		for i := range want {
			if got[i] != float32(want[i]) {
				t.Fatalf("document %d: got %v want %v", i, got[i], want[i])
			}
		}
		// One defect makes the answer an error.
		bad := slices.Clone(results)
		at := rapid.IntRange(0, n-1).Draw(t, "at")
		inf := math.Inf(1)
		huge := 1e300 // finite as float64, infinite as float32
		switch rapid.SampledFrom([]string{"drop", "repeat", "range", "negative", "null", "inf", "overflow"}).Draw(t, "defect") {
		case "drop":
			bad = slices.Delete(bad, at, at+1)
		case "repeat":
			if n == 1 {
				bad = append(bad, bad[0])
			} else {
				bad[at].Index = bad[(at+1)%n].Index
			}
		case "range":
			bad[at].Index = n
		case "negative":
			bad[at].Index = -1
		case "null":
			bad[at].Score = nil
		case "inf":
			bad[at].Score = &inf
		case "overflow":
			bad[at].Score = &huge
		}
		if _, err := rerankScores(bad, n); err == nil {
			t.Fatalf("defective answer accepted: %+v", bad)
		}
	})
}

func TestNewReranker(t *testing.T) {
	for _, k := range []string{"MEMSTATE_RERANK_MODEL", "MEMSTATE_RERANK_URL", "MEMSTATE_RERANK_TIMEOUT"} {
		t.Setenv(k, "")
	}
	if r := NewReranker("", "", 0, "http://e:1/v1"); r != nil {
		t.Fatalf("no model means no reranker: %+v", r)
	}
	// The embedding URL is the default, and the default timeout applies.
	r := NewReranker("", "m", 0, "http://e:1/v1")
	if r == nil || r.URL != "http://e:1/v1" || r.Model != "m" || r.timeout() != defaultRerankTimeout {
		t.Fatalf("defaults: %+v", r)
	}
	// Ollama's native base has no rerank API.
	if r := NewReranker("", "m", 0, "http://127.0.0.1:11434"); r != nil {
		t.Fatalf("a URL that is not a /v1 base must give no reranker: %+v", r)
	}
	// Flag beats environment.
	t.Setenv("MEMSTATE_RERANK_MODEL", "env-model")
	t.Setenv("MEMSTATE_RERANK_URL", "http://env:1/v1")
	t.Setenv("MEMSTATE_RERANK_TIMEOUT", "3s")
	if r := NewReranker("", "", 0, "http://e:1/v1"); r == nil || r.Model != "env-model" || r.URL != "http://env:1/v1" || r.timeout() != 3*time.Second {
		t.Fatalf("environment: %+v", r)
	}
	if r := NewReranker("http://flag:1/v1", "flag-model", time.Second, "http://e:1/v1"); r.Model != "flag-model" || r.URL != "http://flag:1/v1" || r.timeout() != time.Second {
		t.Fatalf("flags: %+v", r)
	}
}

func TestHTTPHealthReportsReranker(t *testing.T) {
	store := newTestStore(t)
	ts := httptest.NewServer(newRouter(store, nil, nil, &Reranker{Model: "rr", URL: "http://r:1/v1", Timeout: 4 * time.Second}))
	t.Cleanup(ts.Close)
	_, body := getJSON(t, ts.URL+"/health")
	if body["rerank_model"] != "rr" || body["rerank_url"] != "http://r:1/v1" || body["rerank_timeout"] != "4s" {
		t.Fatalf("health: %+v", body)
	}
	_, body = getJSON(t, newTestServer(t).URL+"/health")
	if _, has := body["rerank_model"]; has {
		t.Fatalf("rerank_model must be absent without a reranker: %+v", body)
	}
}

func TestRerankHint(t *testing.T) {
	if h := rerankHint("qwen3-embedding:4b"); !strings.Contains(h, "qwen3-reranker") {
		t.Fatalf("hint for qwen3: %q", h)
	}
	if h := rerankHint("some-other-model"); h != "" {
		t.Fatalf("no pair is known for this model: %q", h)
	}
}

// --- vector size ---------------------------------------------------------

// The store holds one vector length per model name, for any order of writes.
func TestOneVectorSizePerModelProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := newTestStore(t)
		first := map[string]int{}
		writes := rapid.IntRange(1, 30).Draw(rt, "writes")
		for range writes {
			model := rapid.SampledFrom([]string{"m1", "m2"}).Draw(rt, "model")
			kp := rapid.SampledFrom([]string{"a", "b", "c"}).Draw(rt, "keypath")
			dim := rapid.IntRange(1, 4).Draw(rt, "dim")
			err := s.UpsertKeypathEmbedding("p", kp, model, dim, packVector(make([]float32, dim)))
			want, known := first[model]
			var vse *vectorSizeError
			switch {
			case !known || want == dim:
				if err != nil {
					rt.Fatalf("%s dim %d refused: %v", model, dim, err)
				}
				first[model] = dim
			case !errors.As(err, &vse) || vse.Got != dim || vse.Stored != want || vse.Model != model:
				rt.Fatalf("%s holds dim %d; a vector of %d must be refused with both sizes, got %v", model, want, dim, err)
			}
		}
		for model, want := range first {
			dims, err := s.EmbeddingDims(model)
			if err != nil || len(dims) != 1 || dims[0] != want {
				rt.Fatalf("%s: dims %v err %v, want [%d]", model, dims, err, want)
			}
		}
	})
}

// After the model behind a name changes size, a rebuild is the repair.
func TestRebuildResetsVectorSize(t *testing.T) {
	var accepted []string
	srv := mockOpenAI(t, 0, &accepted) // vectors of 2
	defer srv.Close()
	s := newTestStore(t)
	if _, _, err := s.Write("p", "a", "alpha", WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertKeypathEmbedding("p", "a", "mock", 3, packVector([]float32{1, 0, 0})); err != nil {
		t.Fatal(err)
	}
	e := &Embedder{URL: srv.URL + "/v1", Model: "mock", Client: &http.Client{Timeout: 2 * time.Second}}
	if _, done, _, err := e.Rebuild(s, io.Discard); err != nil || done != 1 {
		t.Fatalf("rebuild: done %d err %v", done, err)
	}
	if dims, _ := s.EmbeddingDims("mock"); len(dims) != 1 || dims[0] != 2 {
		t.Fatalf("dims after rebuild: %v", dims)
	}
}

func TestSearchReportsVectorSizeMismatch(t *testing.T) {
	var accepted []string
	srv := mockOpenAI(t, 0, &accepted) // query vectors of 2
	defer srv.Close()
	store := newTestStore(t)
	if _, _, err := store.Write("p", "a", "alpha note", WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	// The store holds a vector of 3 under the same model name.
	if err := store.UpsertKeypathEmbedding("p", "a", "mock", 3, packVector([]float32{1, 0, 0})); err != nil {
		t.Fatal(err)
	}
	e := &Embedder{URL: srv.URL + "/v1", Model: "mock", Client: &http.Client{Timeout: 2 * time.Second}}
	ts := httptest.NewServer(newRouter(store, nil, e, nil))
	t.Cleanup(ts.Close)
	url := ts.URL + "/api/v1/memories/search"

	zero := float32(0)
	code, body := postJSON(t, url, map[string]any{"project_id": "p", "query": "alpha", "threshold": zero})
	reason, _ := body["degraded"].(string)
	if code != 200 || !strings.Contains(reason, "returns 2 values") || !strings.Contains(reason, "vectors of 3") ||
		!strings.Contains(reason, "embed rebuild --model mock") {
		t.Fatalf("hybrid must answer with the word match and say why semantic is off: %d %+v", code, body)
	}
	if got := resultKeypaths(t, body); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("hybrid results: %v", got)
	}
	code, body = postJSON(t, url, map[string]any{"project_id": "p", "query": "alpha", "mode": "semantic", "threshold": zero})
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]), "size mismatch") {
		t.Fatalf("semantic mode must refuse with 409: %d %+v", code, body)
	}
	// A write does not replace the stored vector with one of another size.
	maybeEmbedContent(store, e, "p", "a", "alpha note", true)
	e.WaitForPending()
	if dims, _ := store.EmbeddingDims("mock"); len(dims) != 1 || dims[0] != 3 {
		t.Fatalf("the store must keep its size until a rebuild: %v", dims)
	}
}

func TestCheckVector(t *testing.T) {
	nan, inf := float32(math.NaN()), float32(math.Inf(-1))
	for name, v := range map[string][]float32{
		"nan": {0.1, nan}, "inf": {inf, 0.2}, "zeros": {0, 0, 0},
	} {
		var se *embedStatusError
		if err := checkVector(v); !errors.As(err, &se) {
			t.Errorf("%s: %v must be refused as an embedStatusError, got %v", name, v, err)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		v := rapid.SliceOfN(rapid.Float32Range(-10, 10), 1, 16).Draw(t, "vector")
		v[rapid.IntRange(0, len(v)-1).Draw(t, "nonzero_at")] = 0.5
		if err := checkVector(v); err != nil {
			t.Fatalf("finite vector %v refused: %v", v, err)
		}
	})
}

// llama.cpp writes a NaN component as JSON null. The embedder must refuse
// that vector, not store zeros.
func TestEmbedRefusesNullVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[null,null,null]}]}`))
	}))
	defer srv.Close()
	e := &Embedder{URL: srv.URL + "/v1", Model: "mock", Client: &http.Client{Timeout: 2 * time.Second}}
	var se *embedStatusError
	if _, err := e.Embed(context.Background(), "x"); !errors.As(err, &se) {
		t.Fatalf("a vector of nulls must be refused, got %v", err)
	}
}
