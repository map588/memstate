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
	v1, prev, err := s.Write("proj", "auth.provider", "jwt", WriteMeta{Source: "agent"}, false)
	if err != nil || v1.Version != 1 || prev != nil {
		t.Fatalf("v1 write: v=%+v prev=%+v err=%v", v1, prev, err)
	}
	v2, prev, err := s.Write("proj", "auth.provider", "sessions", WriteMeta{Source: "agent"}, false)
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
	_, _, _ = s.Write("p", "db.engine", "postgres with pgvector", WriteMeta{}, false)
	_, _, _ = s.Write("p", "cache", "redis cluster", WriteMeta{}, false)
	_, _, _ = s.Write("p", "tombstoned", "secret phrase xyzzy", WriteMeta{}, false)
	_, _, _ = s.Delete("p", "tombstoned")

	hits, err := s.Search("p", "redis", SearchFilter{}, 10)
	if err != nil || len(hits) != 1 || hits[0].Keypath != "cache" {
		t.Fatalf("redis search: %+v err=%v", hits, err)
	}
	hits, err = s.Search("p", "xyzzy", SearchFilter{}, 10)
	if err != nil {
		t.Fatalf("xyzzy search err: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected tombstoned hit excluded, got %+v", hits)
	}
}

func TestStoreDeleteSubtree(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("p", "auth.provider", "jwt", WriteMeta{}, false)
	_, _, _ = s.Write("p", "auth.session.ttl", "15m", WriteMeta{}, false)
	_, _, _ = s.Write("p", "db.engine", "pg", WriteMeta{}, false)

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
	_, _, _ = s.Write("p", "auth.provider", "jwt", WriteMeta{}, false)
	_, _, _ = s.Write("p", "auth.session.ttl", "15m", WriteMeta{}, false)
	_, _, _ = s.Write("p", "db.engine", "pg", WriteMeta{}, false)

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
	_, _, _ = s.Write("a", "x", "1", WriteMeta{}, false)
	_, _, _ = s.Write("b", "x", "2", WriteMeta{}, false)
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
	m, _, _ := s.Write("p", "k", "v", WriteMeta{}, false)
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
	_, _, _ = s.Write("p", "auth", "v1", WriteMeta{Source: "agent"}, false)

	secs := []Section{
		{Keypath: "auth", Content: "v2"},
		{Keypath: "db.engine", Content: "pg"},
		{Keypath: "cache", Content: "redis"},
	}
	items, err := s.WriteBatch("p", secs, WriteMeta{Source: "agent"})
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
	items, err := s.WriteBatch("p", secs, WriteMeta{})
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
	_, _, _ = s.Write("p", "k", "v", WriteMeta{}, false)
	if d, _ := s.ProjectDeleted("p"); d {
		t.Fatal("fresh project flagged deleted")
	}
	_ = s.DeleteProject("p")
	if d, _ := s.ProjectDeleted("p"); !d {
		t.Fatal("soft-deleted project not flagged")
	}
}

func TestStoreListEscapesLikeWildcards(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("p", "task_1.detail", "a", WriteMeta{}, false)
	_, _, _ = s.Write("p", "taskx1.detail", "b", WriteMeta{}, false)

	list, err := s.List("p", "task_1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Keypath != "task_1.detail" {
		t.Fatalf("underscore must not act as LIKE wildcard: %+v", list)
	}
}

func TestStoreSearchToleratesPunctuation(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("p", "k", "the user's config for foo-bar", WriteMeta{}, false)

	for _, q := range []string{`user's config`, `foo-bar`, `"unbalanced`} {
		if _, err := s.Search("p", q, SearchFilter{}, 10); err != nil {
			t.Fatalf("query %q must not error: %v", q, err)
		}
	}
	hits, err := s.Search("p", "user's", SearchFilter{}, 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("apostrophe query should match: %+v err=%v", hits, err)
	}
}

func TestStoreSearchExcludesDeletedProjects(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("live", "k", "shared token word", WriteMeta{}, false)
	_, _, _ = s.Write("dead", "k", "shared token word", WriteMeta{}, false)
	_ = s.DeleteProject("dead")

	// Global search (no project scope) must skip soft-deleted projects.
	hits, err := s.Search("", "token", SearchFilter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ProjectID != "live" {
		t.Fatalf("deleted project leaked into global search: %+v", hits)
	}
}

func TestStoreCategoryTopics(t *testing.T) {
	s := newTestStore(t)
	meta := WriteMeta{Category: "decision", Topics: []string{"auth", "security"}}
	_, _, err := s.Write("p", "k", "use jwt", meta, false)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := s.GetLatest("p", "k")
	if err != nil || got.Category != "decision" ||
		len(got.Topics) != 2 || got.Topics[0] != "auth" {
		t.Fatalf("metadata roundtrip: %+v err=%v", got, err)
	}

	// Identical content AND metadata → unchanged (no new version).
	again, prev, err := s.Write("p", "k", "use jwt", meta, false)
	if err != nil || again.ID != prev.ID {
		t.Fatalf("identical write should be unchanged: %+v %+v err=%v", again, prev, err)
	}

	// Same content, different metadata → new version.
	v2, prev, err := s.Write("p", "k", "use jwt",
		WriteMeta{Category: "decision", Topics: []string{"auth"}}, false)
	if err != nil || v2.Version != 2 || prev.Version != 1 {
		t.Fatalf("metadata change must version: %+v prev=%+v err=%v", v2, prev, err)
	}

	// Category filter.
	hits, err := s.Search("p", "jwt", SearchFilter{Category: "decision"}, 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("category filter: %+v err=%v", hits, err)
	}
	hits, err = s.Search("p", "jwt", SearchFilter{Category: "other"}, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("wrong category must not match: %+v err=%v", hits, err)
	}

	// Topics filter is match-any.
	hits, err = s.Search("p", "jwt", SearchFilter{Topics: []string{"auth", "unrelated"}}, 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("topics match-any: %+v err=%v", hits, err)
	}
	hits, err = s.Search("p", "jwt", SearchFilter{Topics: []string{"unrelated"}}, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("unmatched topic must not hit: %+v err=%v", hits, err)
	}
}

func TestStoreSupersededVersionLeavesFTS(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("p", "k", "ancient sphinx riddle", WriteMeta{}, false)
	_, _, _ = s.Write("p", "k", "modern replacement text", WriteMeta{}, false)

	// The old version's FTS row must be gone: unique old-content token
	// finds nothing even though History still holds the text.
	hits, err := s.Search("p", "sphinx", SearchFilter{}, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("superseded FTS row should be dropped: %+v err=%v", hits, err)
	}
	hist, _ := s.History("p", "k")
	if len(hist) != 2 || hist[1].Content != "ancient sphinx riddle" {
		t.Fatalf("history must keep old content: %+v", hist)
	}
}

func TestStoreSearchKeypathPrefix(t *testing.T) {
	s := newTestStore(t)
	_, _, _ = s.Write("p", "branches.feature_x.todo", "shared token alpha", WriteMeta{}, false)
	_, _, _ = s.Write("p", "branches.feature_x2.todo", "shared token alpha", WriteMeta{}, false)
	_, _, _ = s.Write("p", "decisions.auth", "shared token alpha", WriteMeta{}, false)

	// Prefix must stop at the dot boundary: feature_x, not feature_x2.
	hits, err := s.Search("p", "alpha", SearchFilter{KeypathPrefix: "branches.feature_x"}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Keypath != "branches.feature_x.todo" {
		t.Fatalf("prefix must respect dot boundary: %+v", hits)
	}

	// Exact keypath also matches its own prefix.
	hits, err = s.Search("p", "alpha", SearchFilter{KeypathPrefix: "decisions.auth"}, 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("exact-keypath prefix: %+v err=%v", hits, err)
	}

	// No filter still returns all three.
	hits, err = s.Search("p", "alpha", SearchFilter{}, 10)
	if err != nil || len(hits) != 3 {
		t.Fatalf("unfiltered: %+v err=%v", hits, err)
	}
}

func TestSearchFilterMatchesKeypathPrefix(t *testing.T) {
	f := SearchFilter{KeypathPrefix: "branches.feature_x"}
	cases := []struct {
		keypath string
		want    bool
	}{
		{"branches.feature_x", true},
		{"branches.feature_x.todo", true},
		{"branches.feature_x2", false},
		{"branches.feature_x2.todo", false},
		{"decisions.auth", false},
	}
	for _, c := range cases {
		if got := f.Matches(&Memory{Keypath: c.keypath}); got != c.want {
			t.Errorf("Matches(%q) = %v, want %v", c.keypath, got, c.want)
		}
	}
}
