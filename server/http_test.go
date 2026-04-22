package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := newTestStore(t)
	ts := httptest.NewServer(newRouter(store, nil))
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

func TestHTTPRejectRemoteWithoutKeypath(t *testing.T) {
	ts := newTestServer(t)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "p", "content": "markdown body",
	})
	if code != 400 || !strings.Contains(body["error"].(string), "keypath") {
		t.Fatalf("expected keypath-required error: %d %+v", code, body)
	}
}
