package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
	if !keypaths["my_app.auth"] || !keypaths["my_app.database"] {
		t.Fatalf("keypaths missing project_id prefix: %+v", keypaths)
	}
}

func TestHTTPRememberRootOverrideEmpty(t *testing.T) {
	ts := newTestServer(t)
	empty := ""
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "my_app",
		"content":    "## Auth\n\nx\n",
		"root":       empty, // explicit "" disables the project_id default
	})
	if code != 200 {
		t.Fatalf("code %d %+v", code, body)
	}
	items := body["items"].([]any)
	it := items[0].(map[string]any)
	if it["keypath"] != "auth" {
		t.Fatalf("explicit empty root should drop prefix, got %v", it["keypath"])
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
	kps := map[string]string{}
	for _, raw := range items {
		it := raw.(map[string]any)
		kps[it["keypath"].(string)] = it["stored"].(map[string]any)["content"].(string)
	}
	if _, ok := kps["my_app.preamble"]; !ok {
		t.Fatalf("preamble not captured: %+v", kps)
	}
	if _, ok := kps["my_app.auth"]; !ok {
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
	for _, want := range []string{"my_app.todo", "my_app.decisions", "my_app.questions"} {
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
	if items[0].(map[string]any)["keypath"] != "my_app.preamble" {
		t.Fatalf("prose should go to <project>.preamble: %+v", items[0])
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
