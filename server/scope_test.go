package main

import (
	"errors"
	"strings"
	"testing"
)

func TestReservedWriteGate(t *testing.T) {
	s := newTestStore(t)
	allowed := []string{
		"preferences.commit_style",
		"preferences.tests.strategy",
		"profile.role",
		"host.mbp.env.go_bin",
		"host.mbp.tools.ollama.model",
		"practices.go_table_tests",
		"lessons.zsh_word_splitting",
	}
	for _, kp := range allowed {
		if _, _, err := s.Write(sharedProject, kp, "v", WriteMeta{}, false); err != nil {
			t.Errorf("%s: want accept, got %v", kp, err)
		}
	}
	rejected := []string{
		"todo.x",
		"decisions.x",
		"preamble",
		"notes.x",
		"env.x",
		"tools.x",
		"host.mbp",
		"host.mbp.env",
		"host.mbp.notes.x",
		"preferences",
		"profile",
		"practices",
		"lessons",
	}
	for _, kp := range rejected {
		_, _, err := s.Write(sharedProject, kp, "v", WriteMeta{}, false)
		if !errors.Is(err, ErrReservedWrite) {
			t.Errorf("%s: want ErrReservedWrite, got %v", kp, err)
		}
	}
	// Any other id with the reserved prefix is refused.
	if _, _, err := s.Write("_scratch", "preferences.x", "v", WriteMeta{}, false); !errors.Is(err, ErrReservedWrite) {
		t.Errorf("_scratch: want ErrReservedWrite, got %v", err)
	}
	// Normal projects stay free-form.
	if _, _, err := s.Write("my_app", "todo.x", "v", WriteMeta{}, false); err != nil {
		t.Errorf("my_app todo.x: %v", err)
	}
	// A tombstone always passes, so an old mistake can be deleted.
	if _, _, err := s.Write(sharedProject, "todo.x", "", WriteMeta{}, true); err != nil {
		t.Errorf("tombstone in _shared: %v", err)
	}
	// Nothing leaked into _shared from the rejected writes.
	mems, err := s.List(sharedProject, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != len(allowed) {
		t.Fatalf("_shared has %d memories, want %d", len(mems), len(allowed))
	}
}

func TestHTTPReservedWriteIs400(t *testing.T) {
	ts := newTestServer(t)
	code, out := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": sharedProject, "keypath": "todo.x", "content": "nope",
	})
	if code != 400 || !strings.Contains(out["error"].(string), "allowed") {
		t.Fatalf("store: code=%d out=%v", code, out)
	}
	code, out = postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "_scratch", "keypath": "preferences.x", "content": "nope",
	})
	if code != 400 || !strings.Contains(out["error"].(string), "reserved") {
		t.Fatalf("store _scratch: code=%d out=%v", code, out)
	}

	// A heading-split remember with one bad section writes nothing.
	code, out = postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": sharedProject,
		"content":    "## Preferences\nshort commits\n\n## Todo\nfinish the thing\n",
	})
	if code != 400 {
		t.Fatalf("remember: code=%d out=%v", code, out)
	}
	code, tree := getJSON(t, ts.URL+"/api/v1/tree?project_id="+sharedProject)
	if code != 200 || tree["total_memories"].(float64) != 0 {
		t.Fatalf("tree after rejected remember: code=%d %v", code, tree)
	}
}

func TestImportRespectsReservedGate(t *testing.T) {
	dst := newTestStore(t)
	data := exportOf(sharedProject,
		ExportMemory{Keypath: "preferences.x", Content: "ok", Version: 1, CreatedAt: 10},
		ExportMemory{Keypath: "todo.x", Content: "bad", Version: 1, CreatedAt: 10},
	)
	if _, err := dst.Merge(data, "", false); !errors.Is(err, ErrReservedWrite) {
		t.Fatalf("merge: want ErrReservedWrite, got %v", err)
	}
	mems, err := dst.List(sharedProject, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Fatalf("rejected import left %d memories", len(mems))
	}
	// A chain whose latest version is a tombstone is history, not a fact.
	data = exportOf(sharedProject,
		ExportMemory{Keypath: "todo.x", Content: "bad", Version: 1, CreatedAt: 10},
		ExportMemory{Keypath: "todo.x", Version: 2, Tombstone: true, CreatedAt: 11},
	)
	if _, err := dst.Merge(data, "", false); err != nil {
		t.Fatalf("tombstoned chain: %v", err)
	}
}

// An export written before the rename names the shared project "_user".
// Merge maps it, and the old name is no longer a write target.
func TestImportMapsLegacySharedProject(t *testing.T) {
	dst := newTestStore(t)
	data := exportOf(legacySharedProject,
		ExportMemory{Keypath: "practices.x", Content: "ok", Version: 1, CreatedAt: 10},
	)
	if _, err := dst.Merge(data, "", false); err != nil {
		t.Fatalf("merge: %v", err)
	}
	mems, err := dst.List(sharedProject, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 || mems[0].Keypath != "practices.x" {
		t.Fatalf("shared project after legacy import: %+v", mems)
	}
	if _, _, err := dst.Write(legacySharedProject, "preferences.x", "v", WriteMeta{}, false); !errors.Is(err, ErrReservedWrite) {
		t.Fatalf("write to the legacy name: want ErrReservedWrite, got %v", err)
	}
}

func TestSlugHost(t *testing.T) {
	cases := map[string]string{
		"Matts-MBP.local":  "matts_mbp",
		"build-01.corp.io": "build_01",
		"":                 "default",
		"_user":            "user",
	}
	for in, want := range cases {
		if got := slugHost(in); got != want {
			t.Errorf("slugHost(%q)=%q want %q", in, got, want)
		}
	}
	if hostSlug() == "" {
		t.Fatal("hostSlug() must never be empty")
	}
}

// A proxy from a release before 2026-10-17 still names the shared project
// "_user" on the wire. The daemon maps it on every request, because
// `memstated upgrade` replaces only the daemon.
func TestLegacySharedProjectOnTheWire(t *testing.T) {
	ts := newTestServer(t)
	code, body := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": legacySharedProject, "keypath": "preferences.legacy", "content": "v",
	})
	if code != 200 {
		t.Fatalf("store via %s: %d %+v", legacySharedProject, code, body)
	}
	stored, _ := body["stored"].(map[string]any)
	if stored["project_id"] != sharedProject {
		t.Fatalf("stored project: got %v want %s", stored["project_id"], sharedProject)
	}
	code, body = getJSON(t, ts.URL+"/api/v1/tree?project_id="+legacySharedProject)
	if code != 200 || body["project_id"] != sharedProject {
		t.Fatalf("tree via %s: %d %+v", legacySharedProject, code, body)
	}
	code, body = postJSON(t, ts.URL+"/api/v1/memories/search", map[string]any{
		"project_id": legacySharedProject, "query": "v", "mode": "fts",
	})
	if code != 200 || body["total_found"] != float64(1) {
		t.Fatalf("search via %s: %d %+v", legacySharedProject, code, body)
	}
	code, body = postJSON(t, ts.URL+"/api/v1/memories/delete", map[string]any{
		"project_id": legacySharedProject, "keypath": "preferences.legacy",
	})
	if code != 200 {
		t.Fatalf("delete via %s: %d %+v", legacySharedProject, code, body)
	}
}
