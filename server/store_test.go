package main

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
	v1, prev, err := s.Write("proj", "auth.provider", "jwt", "agent", false)
	if err != nil || v1.Version != 1 || prev != nil {
		t.Fatalf("v1 write: v=%+v prev=%+v err=%v", v1, prev, err)
	}
	v2, prev, err := s.Write("proj", "auth.provider", "sessions", "agent", false)
	if err != nil || v2.Version != 2 || prev == nil || prev.Version != 1 {
		t.Fatalf("v2 write: v=%+v prev=%+v err=%v", v2, prev, err)
	}
	latest, err := s.GetLatest("proj", "auth.provider")
	if err != nil || latest.Version != 2 || latest.Content != "sessions" {
		t.Fatalf("latest: %+v err=%v", latest, err)
	}
	hist, err := s.History("proj", "auth.provider")
	if err != nil || len(hist) != 2 || hist[0].Version != 2 || hist[1].Version != 1 {
		t.Fatalf("history: %+v err=%v", hist, err)
	}
	tomb, prev, err := s.Delete("proj", "auth.provider")
	if err != nil || !tomb.Tombstone || prev.Version != 2 {
		t.Fatalf("delete: tomb=%+v prev=%+v err=%v", tomb, prev, err)
	}
	latest, err = s.GetLatest("proj", "auth.provider")
	if err != nil || !latest.Tombstone {
		t.Fatalf("latest after delete: %+v err=%v", latest, err)
	}
	list, err := s.List("proj", "")
	if err != nil || len(list) != 0 {
		t.Fatalf("list after delete: len=%d err=%v", len(list), err)
	}
}

func TestStoreSearch(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("p", "db.engine", "postgres with pgvector", "", false)
	_, _, _ = s.Write("p", "cache", "redis cluster", "", false)
	_, _, _ = s.Write("p", "tombstoned", "secret phrase xyzzy", "", false)
	_, _, _ = s.Delete("p", "tombstoned")

	hits, err := s.Search("p", "redis", 10)
	if err != nil || len(hits) != 1 || hits[0].Keypath != "cache" {
		t.Fatalf("redis search: %+v err=%v", hits, err)
	}
	hits, err = s.Search("p", "xyzzy", 10)
	if err != nil {
		t.Fatalf("xyzzy search err: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected tombstoned hit excluded, got %+v", hits)
	}
}

func TestStoreDeleteSubtree(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("p", "auth.provider", "jwt", "", false)
	_, _, _ = s.Write("p", "auth.session.ttl", "15m", "", false)
	_, _, _ = s.Write("p", "db.engine", "pg", "", false)

	killed, err := s.DeleteSubtree("p", "auth")
	if err != nil {
		t.Fatalf("subtree: %v", err)
	}
	if len(killed) != 2 {
		t.Fatalf("killed %d want 2: %+v", len(killed), killed)
	}
	list, _ := s.List("p", "")
	if len(list) != 1 || list[0].Keypath != "db.engine" {
		t.Fatalf("after subtree delete: %+v", list)
	}
}

func TestStoreTree(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("p", "auth.provider", "jwt", "", false)
	_, _, _ = s.Write("p", "auth.session.ttl", "15m", "", false)
	_, _, _ = s.Write("p", "db.engine", "pg", "", false)

	tree, err := s.Tree("p")
	if err != nil || tree == nil {
		t.Fatalf("tree: err=%v", err)
	}
	if len(tree.Children) != 2 {
		t.Fatalf("root children: %d want 2", len(tree.Children))
	}
	// Check auth has "provider" and "session".
	var auth *TreeNode
	for _, c := range tree.Children {
		if c.Name == "auth" {
			auth = c
		}
	}
	if auth == nil || len(auth.Children) != 2 {
		t.Fatalf("auth node missing or wrong shape: %+v", auth)
	}
}

func TestStoreListProjects(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("a", "x", "1", "", false)
	_, _, _ = s.Write("b", "x", "2", "", false)
	_ = s.DeleteProject("b")

	ps, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(ps) != 1 || ps[0].ID != "a" || ps[0].MemoryCount != 1 {
		t.Fatalf("projects: %+v", ps)
	}
}

func TestStoreGetByID(t *testing.T) {
	s := newTestStore(t)
	m, _, _ := s.Write("p", "k", "v", "", false)
	got, err := s.GetByID(m.ID)
	if err != nil || got == nil || got.Content != "v" {
		t.Fatalf("by id: %+v err=%v", got, err)
	}
	missing, err := s.GetByID(9999)
	if err != nil || missing != nil {
		t.Fatalf("missing: %+v err=%v", missing, err)
	}
}

func TestStoreWriteBatch(t *testing.T) {
	s := newTestStore(t)
	// Pre-existing row to force a superseded result for one section.
	_, _, _ = s.Write("p", "auth", "v1", "agent", false)

	secs := []Section{
		{Keypath: "auth", Content: "v2"},
		{Keypath: "db.engine", Content: "pg"},
		{Keypath: "cache", Content: "redis"},
	}
	items, err := s.WriteBatch("p", secs, "agent")
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}
	if items[0].Superseded == nil || items[0].Superseded.Version != 1 || items[0].Stored.Version != 2 {
		t.Fatalf("auth supersede wrong: %+v", items[0])
	}
	if items[1].Superseded != nil || items[1].Stored.Version != 1 {
		t.Fatalf("db.engine expected fresh v1: %+v", items[1])
	}
	if items[2].Superseded != nil || items[2].Stored.Content != "redis" {
		t.Fatalf("cache wrong: %+v", items[2])
	}
	// All rows visible post-commit.
	list, _ := s.List("p", "")
	if len(list) != 3 {
		t.Fatalf("want 3 live memories, got %d: %+v", len(list), list)
	}
}

func TestStoreWriteBatchSameKeypath(t *testing.T) {
	s := newTestStore(t)
	// Same keypath twice in one batch must chain: v1 then v2, both visible
	// via History (latest wins in List).
	secs := []Section{
		{Keypath: "same", Content: "first"},
		{Keypath: "same", Content: "second"},
	}
	items, err := s.WriteBatch("p", secs, "")
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if items[0].Stored.Version != 1 || items[0].Superseded != nil {
		t.Fatalf("first write wrong: %+v", items[0])
	}
	if items[1].Stored.Version != 2 || items[1].Superseded == nil || items[1].Superseded.Version != 1 {
		t.Fatalf("second write did not chain: %+v", items[1])
	}
	hist, _ := s.History("p", "same")
	if len(hist) != 2 {
		t.Fatalf("history: want 2 versions, got %d", len(hist))
	}
}

func TestProjectDeletedSemantics(t *testing.T) {
	s := newTestStore(t)
	deleted, err := s.ProjectDeleted("never-existed")
	if err != nil || deleted {
		t.Fatalf("missing should NOT be flagged deleted: %v err=%v", deleted, err)
	}
	_, _, _ = s.Write("p", "k", "v", "", false)
	if d, _ := s.ProjectDeleted("p"); d {
		t.Fatal("fresh project flagged deleted")
	}
	_ = s.DeleteProject("p")
	if d, _ := s.ProjectDeleted("p"); !d {
		t.Fatal("soft-deleted project not flagged")
	}
}
