package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// genRankedLists draws two ranked lists over a shared keypath universe so
// that some keypaths appear in both, some in one, some in neither.
func genRankedLists(t *rapid.T) ([]*Memory, []*SemanticHit, int) {
	n := rapid.IntRange(0, 12).Draw(t, "universe")
	universe := make([]string, n)
	for i := range universe {
		universe[i] = fmt.Sprintf("kp_%d", i)
	}
	pick := func(label string) []string {
		if n == 0 {
			return nil
		}
		perm := rapid.Permutation(universe).Draw(t, label+"_perm")
		k := rapid.IntRange(0, n).Draw(t, label+"_len")
		return perm[:k]
	}
	var fts []*Memory
	for _, kp := range pick("fts") {
		fts = append(fts, &Memory{ProjectID: "p", Keypath: kp})
	}
	var sem []*SemanticHit
	for _, kp := range pick("sem") {
		sem = append(sem, &SemanticHit{Memory: &Memory{ProjectID: "p", Keypath: kp}})
	}
	limit := rapid.IntRange(1, 15).Draw(t, "limit")
	return fts, sem, limit
}

func rankOf(list []string, kp string) int {
	i := slices.Index(list, kp)
	if i < 0 {
		return -1
	}
	return i + 1
}

func TestRRFFuseProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		fts, sem, limit := genRankedLists(t)
		ftsKeys := make([]string, len(fts))
		for i, m := range fts {
			ftsKeys[i] = m.Keypath
		}
		semKeys := make([]string, len(sem))
		for i, h := range sem {
			semKeys[i] = h.Keypath
		}
		union := map[string]bool{}
		for _, k := range ftsKeys {
			union[k] = true
		}
		for _, k := range semKeys {
			union[k] = true
		}

		out := rrfFuse(fts, sem, limit)

		// 1. Output keys are a subset of the union, with no duplicates.
		seen := map[string]bool{}
		for _, h := range out {
			if !union[h.Keypath] {
				t.Fatalf("keypath %q not in either input", h.Keypath)
			}
			if seen[h.Keypath] {
				t.Fatalf("keypath %q returned twice", h.Keypath)
			}
			seen[h.Keypath] = true
		}
		// 2. Length is min(limit, |union|).
		if want := min(limit, len(union)); len(out) != want {
			t.Fatalf("len(out)=%d want %d (limit %d, union %d)", len(out), want, limit, len(union))
		}
		// 3. Scores are non-increasing.
		for i := 1; i < len(out); i++ {
			if out[i].Score > out[i-1].Score {
				t.Fatalf("score at %d (%v) > score at %d (%v)", i, out[i].Score, i-1, out[i-1].Score)
			}
		}
		// 6. Sources name exactly the lists the item came from.
		for _, h := range out {
			var want []string
			if rankOf(ftsKeys, h.Keypath) > 0 {
				want = append(want, "fts")
			}
			if rankOf(semKeys, h.Keypath) > 0 {
				want = append(want, "semantic")
			}
			if !reflect.DeepEqual(h.Sources, want) {
				t.Fatalf("sources for %q = %v want %v", h.Keypath, h.Sources, want)
			}
		}
		// 4. An item in both lists outranks an item at the same rank in
		// only one list.
		pos := map[string]int{}
		for i, h := range out {
			pos[h.Keypath] = i
		}
		for _, both := range ftsKeys {
			rf, rs := rankOf(ftsKeys, both), rankOf(semKeys, both)
			if rs < 0 {
				continue
			}
			r := min(rf, rs)
			for kp := range union {
				if kp == both {
					continue
				}
				kf, ks := rankOf(ftsKeys, kp), rankOf(semKeys, kp)
				onlyOne := (kf > 0) != (ks > 0)
				if !onlyOne {
					continue
				}
				single := max(kf, ks)
				if single < r {
					continue
				}
				pb, okb := pos[both]
				ps, oks := pos[kp]
				if oks && (!okb || ps < pb) {
					t.Fatalf("%q (fts %d, sem %d) should outrank %q (fts %d, sem %d)",
						both, rf, rs, kp, kf, ks)
				}
			}
		}
		// 5. Determinism.
		again := rrfFuse(fts, sem, limit)
		if !reflect.DeepEqual(out, again) {
			t.Fatalf("rrfFuse is not deterministic")
		}
	})
}

func keypathsOf(results any) []string {
	list, _ := results.([]any)
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.(map[string]any)["keypath"].(string))
	}
	return out
}

func TestHTTPHybridIsDefaultAndDegradesWithoutEmbedder(t *testing.T) {
	ts := newTestServer(t) // nil embedder
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "keypath": "auth", "content": "jwt tokens expire hourly",
	})
	code, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "jwt",
	})
	if code != 200 {
		t.Fatalf("hybrid without embedder must not fail: %d %+v", code, body)
	}
	if body["mode"] != "hybrid" {
		t.Fatalf("omitted mode should default to hybrid, got %v", body["mode"])
	}
	if d, _ := body["degraded"].(string); d == "" {
		t.Fatalf("want degraded reason without embedder: %+v", body)
	}
	if _, has := body["model"]; has {
		t.Fatalf("degraded response must not report a model: %+v", body)
	}
	if got := keypathsOf(body["results"]); !slices.Equal(got, []string{"auth"}) {
		t.Fatalf("want FTS result [auth], got %v", got)
	}
	top := body["results"].([]any)[0].(map[string]any)
	if src, _ := top["sources"].([]any); len(src) != 1 || src[0] != "fts" {
		t.Fatalf("want sources [fts], got %v", top["sources"])
	}
}

func TestHTTPHybridMultiWordQueryUsesAnyToken(t *testing.T) {
	ts := newTestServer(t)
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "keypath": "auth", "content": "jwt tokens expire hourly",
	})
	query := "jwt zzzznonsense"
	_, fts := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": query, "mode": "fts",
	})
	if int(fts["total_found"].(float64)) != 0 {
		t.Fatalf("fts mode requires every token, got %+v", fts)
	}
	_, hy := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": query, "mode": "hybrid",
	})
	if got := keypathsOf(hy["results"]); !slices.Equal(got, []string{"auth"}) {
		t.Fatalf("hybrid should match on any token, got %v", got)
	}
}

func TestHTTPHybridFusesBothSides(t *testing.T) {
	ollama := mockOllama(t)
	defer ollama.Close()
	embedder := newTestEmbedder(t, ollama)
	ts := newTestServerWithEmbedder(t, embedder)
	for _, c := range []string{
		"## Database engine\n\nPostgres 15.\n",
		"## Cache layer\n\nRedis cluster.\n",
	} {
		postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
			"project_id": "my_app", "content": c,
		})
	}
	embedder.WaitForPending()

	// Identical content on the semantic side (cosine 1) and the word
	// "Postgres" on the FTS side: database_engine must come from both.
	_, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "my_app", "query": "Postgres 15.", "threshold": 0.0,
	})
	if body["mode"] != "hybrid" || body["model"] != "mock" {
		t.Fatalf("want hybrid with model, got %+v", body)
	}
	if _, has := body["degraded"]; has {
		t.Fatalf("healthy embedder must not degrade: %+v", body)
	}
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("threshold 0 admits both keypaths, got %+v", results)
	}
	top := results[0].(map[string]any)
	if top["keypath"] != "database_engine" {
		t.Fatalf("want database_engine first, got %+v", top)
	}
	src, _ := top["sources"].([]any)
	if len(src) != 2 || src[0] != "fts" || src[1] != "semantic" {
		t.Fatalf("want sources [fts semantic], got %v", top["sources"])
	}
	second := results[1].(map[string]any)
	if second["score"].(float64) >= top["score"].(float64) {
		t.Fatalf("two-source hit must outscore one-source hit: %+v", results)
	}
}

func TestHTTPHybridDegradesWhenEmbedFails(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusInternalServerError)
	}))
	defer broken.Close()
	embedder := newTestEmbedder(t, broken)
	ts := newTestServerWithEmbedder(t, embedder)
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "keypath": "auth", "content": "jwt tokens",
	})
	embedder.WaitForPending()
	code, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "jwt",
	})
	if code != 200 {
		t.Fatalf("embed failure must not fail hybrid search: %d %+v", code, body)
	}
	d, _ := body["degraded"].(string)
	if !strings.Contains(d, "embed query") {
		t.Fatalf("want degraded reason naming the embed failure, got %+v", body)
	}
	if got := keypathsOf(body["results"]); !slices.Equal(got, []string{"auth"}) {
		t.Fatalf("want FTS fallback [auth], got %v", got)
	}
}

func TestHTTPSearchUnknownMode(t *testing.T) {
	ts := newTestServer(t)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "x", "mode": "fuzzy",
	})
	if code != 400 || !strings.Contains(body["error"].(string), "hybrid|fts|semantic") {
		t.Fatalf("want 400 naming the modes, got %d %+v", code, body)
	}
}
