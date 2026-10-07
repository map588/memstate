package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWithEmbedder(t, nil)
}

func newTestServerWithEmbedder(t *testing.T, embedder *Embedder) *httptest.Server {
	t.Helper()
	store := newTestStore(t)
	ts := httptest.NewServer(newRouter(store, nil, embedder))
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, url string, in any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(in)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out
}

func TestHTTPHealth(t *testing.T) {
	ts := newTestServer(t)
	code, body := getJSON(t, ts.URL+"/health")
	if code != 200 || body["service"] != "memstate" || body["version"] == "" {
		t.Fatalf("bad health: %d %+v", code, body)
	}
	if _, has := body["embed_model"]; has {
		t.Fatalf("embed_model must be absent when embeddings are disabled: %+v", body)
	}
}

func TestHTTPHealthReportsEmbedConfig(t *testing.T) {
	daemonIdleTimeout = 30 * time.Minute
	t.Cleanup(func() { daemonIdleTimeout = 0 })
	ts := newTestServerWithEmbedder(t, &Embedder{Model: "qwen3-embedding", URL: "http://o:1", Timeout: 45 * time.Second})
	_, body := getJSON(t, ts.URL+"/health")
	want := map[string]any{
		"embed_model":        "qwen3-embedding",
		"semantic_threshold": float64(defaultThreshold),
		"embedding_url":      "http://o:1",
		"embed_timeout":      "45s",
		"idle_timeout":       "30m0s",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s: got %v want %v (body %+v)", k, body[k], v, body)
		}
	}
}

func TestHTTPStoreAndGet(t *testing.T) {
	ts := newTestServer(t)

	code, body := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "smoke", "keypath": "config.port", "content": "8080",
	})
	if code != 200 || body["action"] != "created" {
		t.Fatalf("store1: %d %+v", code, body)
	}

	code, body = postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "smoke", "keypath": "config.port", "content": "9090",
	})
	if code != 200 || body["action"] != "superseded" || body["superseded"] == nil {
		t.Fatalf("store2: %d %+v", code, body)
	}

	// /keypaths with include_content
	code, body = postJSON(t, ts.URL+"/api/v1/keypaths", map[string]any{
		"project_id": "smoke", "keypath": "config", "include_content": true,
	})
	if code != 200 || int(body["total_count"].(float64)) != 1 {
		t.Fatalf("keypaths: %d %+v", code, body)
	}
	mems := body["memories"].([]any)
	if mems[0].(map[string]any)["content"] != "9090" {
		t.Fatalf("wrong content: %+v", mems)
	}

	// /tree
	code, body = getJSON(t, ts.URL+"/api/v1/tree?project_id=smoke")
	if code != 200 || body["project_id"] != "smoke" {
		t.Fatalf("tree: %d %+v", code, body)
	}

	// /projects
	code, body = getJSON(t, ts.URL+"/api/v1/projects")
	projects := body["projects"].([]any)
	if code != 200 || len(projects) != 1 {
		t.Fatalf("projects: %d %+v", code, body)
	}
}

func TestHTTPDeleteRecursive(t *testing.T) {
	ts := newTestServer(t)
	for _, kp := range []string{"auth.provider", "auth.session.ttl", "db.engine"} {
		postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
			"project_id": "p", "keypath": kp, "content": "x",
		})
	}
	code, body := postJSON(t, ts.URL+"/api/v1/memories/delete", map[string]any{
		"project_id": "p", "keypath": "auth", "recursive": true,
	})
	if code != 200 || int(body["deleted_count"].(float64)) != 2 {
		t.Fatalf("recursive delete: %d %+v", code, body)
	}
}

func TestHTTPSearch(t *testing.T) {
	ts := newTestServer(t)
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "db.engine", "content": "postgres with pgvector",
	})
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "cache", "content": "redis cluster",
	})
	code, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "redis",
	})
	if code != 200 {
		t.Fatalf("search: %d %+v", code, body)
	}
	if int(body["total_found"].(float64)) != 1 {
		t.Fatalf("want 1 hit: %+v", body)
	}
}

func TestHTTPRejectSoftDeletedProject(t *testing.T) {
	ts := newTestServer(t)
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v",
	})
	postJSON(t, ts.URL+"/api/v1/projects/delete", map[string]any{"project_id": "p"})

	code, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "v",
	})
	if code != 409 {
		t.Fatalf("expected 409 for soft-deleted project, got %d %+v", code, body)
	}
}

func TestHTTPHistoryByMemoryID(t *testing.T) {
	ts := newTestServer(t)
	_, body1 := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v1",
	})
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v2",
	})
	stored := body1["stored"].(map[string]any)
	id := int64(stored["id"].(float64))

	code, body := postJSON(t, ts.URL+"/api/v1/memories/history", map[string]any{
		"memory_id": id,
	})
	if code != 200 {
		t.Fatalf("history: %d %+v", code, body)
	}
	if int(body["total_versions"].(float64)) != 2 {
		t.Fatalf("want 2 versions: %+v", body)
	}
}

func TestHTTPRejectMissingFields(t *testing.T) {
	ts := newTestServer(t)
	code, _ := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p",
	})
	if code != 400 {
		t.Fatalf("want 400, got %d", code)
	}
}

func TestHTTPRememberExplicitKeypath(t *testing.T) {
	ts := newTestServer(t)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "keypath": "auth.provider", "content": "jwt",
	})
	if code != 200 || body["method"] != "explicit" {
		t.Fatalf("explicit: %d %+v", code, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %+v", items)
	}
	it := items[0].(map[string]any)
	if it["action"] != "created" || it["keypath"] != "auth.provider" {
		t.Fatalf("item wrong: %+v", it)
	}
}

func TestHTTPRememberExtractHeadings(t *testing.T) {
	ts := newTestServer(t)
	md := "## Auth\n\nSuperTokens.\n\n## Database\n\nPostgres 15.\n"
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app", "content": md,
	})
	if code != 200 || body["method"] != "headings" {
		t.Fatalf("headings: %d %+v", code, body)
	}
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %+v", items)
	}
	keypaths := map[string]bool{}
	for _, raw := range items {
		it := raw.(map[string]any)
		keypaths[it["keypath"].(string)] = true
		if it["action"] != "created" {
			t.Fatalf("want created, got %+v", it)
		}
	}
	if !keypaths["auth"] || !keypaths["database"] {
		t.Fatalf("extracted keypaths should be top-level: %+v", keypaths)
	}
}

func TestHTTPRememberDefaultRootIsTopLevel(t *testing.T) {
	ts := newTestServer(t)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app",
		"content":    "## Auth\n\nx\n",
	})
	if code != 200 {
		t.Fatalf("code %d %+v", code, body)
	}
	items := body["items"].([]any)
	it := items[0].(map[string]any)
	if it["keypath"] != "auth" {
		t.Fatalf("default root should be top level, got %v", it["keypath"])
	}
}

func TestHTTPRememberRootOverrideExplicit(t *testing.T) {
	ts := newTestServer(t)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app",
		"content":    "## Auth\n\nx\n",
		"root":       "session.today",
	})
	if code != 200 {
		t.Fatalf("code %d %+v", code, body)
	}
	items := body["items"].([]any)
	it := items[0].(map[string]any)
	if it["keypath"] != "session.today.auth" {
		t.Fatalf("explicit root ignored: %v", it["keypath"])
	}
}

func TestHTTPRememberPreambleCaptured(t *testing.T) {
	ts := newTestServer(t)
	md := "Intro text outside any heading.\nMore intro.\n\n## Auth\n\nbody\n"
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app", "content": md,
	})
	if code != 200 {
		t.Fatalf("code %d %+v", code, body)
	}
	items := body["items"].([]any)
	kps := map[string]bool{}
	for _, raw := range items {
		it := raw.(map[string]any)
		kps[it["keypath"].(string)] = true
	}
	if _, ok := kps["preamble"]; !ok {
		t.Fatalf("preamble not captured: %+v", kps)
	}
	if _, ok := kps["auth"]; !ok {
		t.Fatalf("auth missing: %+v", kps)
	}
}

func TestHTTPRememberReservedAliases(t *testing.T) {
	ts := newTestServer(t)
	md := "## TODOs\n\na\n\n## Decisions\n\nb\n\n## Open Questions\n\nc\n"
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app", "content": md,
	})
	if code != 200 {
		t.Fatalf("code %d %+v", code, body)
	}
	items := body["items"].([]any)
	got := map[string]bool{}
	for _, raw := range items {
		got[raw.(map[string]any)["keypath"].(string)] = true
	}
	for _, want := range []string{"todo", "decisions", "questions"} {
		if !got[want] {
			t.Fatalf("want %s in %+v", want, got)
		}
	}
}

func TestHTTPRememberUnchangedOnIdenticalContent(t *testing.T) {
	ts := newTestServer(t)
	md := "## Auth\n\nSuperTokens.\n"
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app", "content": md,
	})
	_, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app", "content": md,
	})
	items := body["items"].([]any)
	it := items[0].(map[string]any)
	if it["action"] != "unchanged" {
		t.Fatalf("want unchanged on identical content: %+v", it)
	}
	// Identity: stored and superseded reference the same memory id.
	stored := it["stored"].(map[string]any)
	superseded := it["superseded"].(map[string]any)
	if stored["id"] != superseded["id"] {
		t.Fatalf("unchanged should return same id for stored and superseded: %+v", it)
	}
}

func TestHTTPStoreUnchangedOnIdenticalContent(t *testing.T) {
	ts := newTestServer(t)
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v1",
	})
	_, body := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v1",
	})
	if body["action"] != "unchanged" {
		t.Fatalf("want unchanged: %+v", body)
	}
}

func TestHTTPRememberProseBecomesPreamble(t *testing.T) {
	ts := newTestServer(t)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app", "content": "Just prose with no headings.",
	})
	if code != 200 {
		t.Fatalf("prose-only content should succeed as preamble: %d %+v", code, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %+v", items)
	}
	if items[0].(map[string]any)["keypath"] != "preamble" {
		t.Fatalf("prose should go to top-level preamble: %+v", items[0])
	}
}

func TestHTTPRememberRejectsWhitespaceOnly(t *testing.T) {
	ts := newTestServer(t)
	code, _ := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "content": "   \n\n\n",
	})
	if code != 400 {
		t.Fatalf("whitespace-only content should 400, got %d", code)
	}
}

func TestHTTPRememberSupersedesAcrossCalls(t *testing.T) {
	ts := newTestServer(t)
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app", "content": "## Auth\n\nv1 body.\n",
	})
	_, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app", "content": "## Auth\n\nv2 body.\n",
	})
	items := body["items"].([]any)
	it := items[0].(map[string]any)
	if it["action"] != "superseded" || it["superseded"] == nil {
		t.Fatalf("want superseded on second call: %+v", it)
	}
}

func TestHTTPWriteRevivesSoftDeletedProject(t *testing.T) {
	ts := newTestServer(t)
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v",
	})
	postJSON(t, ts.URL+"/api/v1/projects/delete", map[string]any{"project_id": "p"})

	// A write to a soft-deleted project must succeed and revive it.
	code, body := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k2", "content": "v2",
	})
	if code != 200 {
		t.Fatalf("write should revive soft-deleted project: %d %+v", code, body)
	}

	// Reads work again after the revive.
	code, body = postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "v2",
	})
	if code != 200 || int(body["total_found"].(float64)) != 1 {
		t.Fatalf("revived project should be readable: %d %+v", code, body)
	}
}

func TestHTTPCategoryTopicsRoundtripAndFilter(t *testing.T) {
	ts := newTestServer(t)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "jwt everywhere",
		"category": "decision", "topics": []string{"auth"},
	})
	if code != 200 {
		t.Fatalf("store: %d %+v", code, body)
	}
	stored := body["stored"].(map[string]any)
	if stored["category"] != "decision" {
		t.Fatalf("category not stored: %+v", stored)
	}

	code, body = postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "jwt", "category": "decision", "topics": []string{"auth"},
	})
	if code != 200 || int(body["total_found"].(float64)) != 1 {
		t.Fatalf("filtered search: %d %+v", code, body)
	}
	code, body = postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "jwt", "category": "note",
	})
	if code != 200 || int(body["total_found"].(float64)) != 0 {
		t.Fatalf("non-matching category must filter out: %d %+v", code, body)
	}
}

func TestHTTPRememberAppliesCategoryToAllSections(t *testing.T) {
	ts := newTestServer(t)
	md := "## Auth\n\na\n\n## Database\n\nb\n"
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "content": md,
		"category": "summary", "topics": []string{"sprint"},
	})
	if code != 200 {
		t.Fatalf("remember: %d %+v", code, body)
	}
	for _, raw := range body["items"].([]any) {
		stored := raw.(map[string]any)["stored"].(map[string]any)
		if stored["category"] != "summary" {
			t.Fatalf("section missing category: %+v", stored)
		}
	}
}

func TestHTTPRejectsDroppedFields(t *testing.T) {
	ts := newTestServer(t)
	// "context" (remember) and "at_revision" (keypaths) were accepted-but-
	// ignored; they are now rejected loudly by DisallowUnknownFields.
	code, _ := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v", "context": "x",
	})
	if code != 400 {
		t.Fatalf("context field should be rejected, got %d", code)
	}
	code, _ = postJSON(t, ts.URL+"/api/v1/keypaths", map[string]any{
		"project_id": "p", "at_revision": 3,
	})
	if code != 400 {
		t.Fatalf("at_revision field should be rejected, got %d", code)
	}
}

// A write response names the versions and never echoes content: the caller
// knows what it sent, and the prior version is one history call away. The
// superseded version carries a preview of previewWordCount words.
func TestHTTPWriteResponseCarriesNoContent(t *testing.T) {
	ts := newTestServer(t)
	first := strings.Repeat("word ", 50) + "end"
	code, body := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": first,
	})
	if code != 200 {
		t.Fatalf("code %d %+v", code, body)
	}
	stored := body["stored"].(map[string]any)
	if _, has := stored["content"]; has {
		t.Fatalf("stored echoes content: %+v", stored)
	}
	if _, has := stored["preview"]; has {
		t.Fatalf("stored carries a preview: %+v", stored)
	}
	_, body = postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": "v2",
	})
	sup := body["superseded"].(map[string]any)
	if _, has := sup["content"]; has {
		t.Fatalf("superseded echoes content: %+v", sup)
	}
	words := strings.Fields(sup["preview"].(string))
	if len(words) != previewWordCount+1 || words[previewWordCount] != "…" {
		t.Fatalf("superseded preview: want %d words and a cut mark, got %q", previewWordCount, sup["preview"])
	}
	_, body = postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "content": "## K\n\nv3\n",
	})
	it := body["items"].([]any)[0].(map[string]any)
	if _, has := it["stored"].(map[string]any)["content"]; has {
		t.Fatalf("remember stored echoes content: %+v", it)
	}
	if got := it["superseded"].(map[string]any)["preview"]; got != "v2" {
		t.Fatalf("remember superseded preview = %v, want v2", got)
	}
}

// A search hit carries a preview, not the content, unless the request sets
// include_content. Without a limit a search returns defaultSearchLimit hits.
func TestHTTPSearchReturnsPreviewNotContent(t *testing.T) {
	ts := newTestServer(t)
	long := strings.Repeat("alpha ", 60) + "zebra"
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "p", "keypath": "k", "content": long,
	})
	for _, mode := range []string{"fts", "hybrid"} {
		_, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
			"project_id": "p", "query": "alpha", "mode": mode,
		})
		hit := body["results"].([]any)[0].(map[string]any)
		if _, has := hit["content"]; has {
			t.Fatalf("%s: hit carries content: %+v", mode, hit)
		}
		if n := len(strings.Fields(hit["preview"].(string))); n != previewWordCount+1 {
			t.Fatalf("%s: preview has %d words, want %d plus the cut mark", mode, n, previewWordCount)
		}
		_, body = postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
			"project_id": "p", "query": "alpha", "mode": mode, "include_content": true,
		})
		hit = body["results"].([]any)[0].(map[string]any)
		if hit["content"] != long {
			t.Fatalf("%s: include_content did not return the content: %+v", mode, hit)
		}
	}
	for i := 0; i < defaultSearchLimit+5; i++ {
		postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
			"project_id": "p", "keypath": fmt.Sprintf("many.k%d", i), "content": "common token",
		})
	}
	_, body := postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": "p", "query": "common token", "mode": "fts",
	})
	if n := len(body["results"].([]any)); n != defaultSearchLimit {
		t.Fatalf("default limit: got %d hits, want %d", n, defaultSearchLimit)
	}
}
