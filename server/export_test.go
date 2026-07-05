package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// exportOf builds a single-project export file in memory.
func exportOf(pid string, mems ...ExportMemory) *ExportData {
	return &ExportData{
		Format:        exportFormat,
		FormatVersion: exportFormatVersion,
		ExportedAt:    1,
		Projects:      []ProjectExport{{ProjectID: pid, Memories: mems}},
	}
}

// setCreatedAt rewrites a version's timestamp so tests can order writes
// across "machines" without sleeping through wall-clock seconds.
func setCreatedAt(t *testing.T, s *Store, pid, kp string, version int, ts int64) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE memories SET created_at=? WHERE project_id=? AND keypath=? AND version=?`,
		ts, pid, kp, version,
	); err != nil {
		t.Fatalf("set created_at: %v", err)
	}
}

func TestMergeFreshTargetRestoresHistory(t *testing.T) {
	src := newTestStore(t)
	mustWrite := func(kp, content string, meta WriteMeta) {
		t.Helper()
		if _, _, err := src.Write("p", kp, content, meta, false); err != nil {
			t.Fatalf("seed %s: %v", kp, err)
		}
	}
	mustWrite("auth.provider", "jwt", WriteMeta{Source: "agent"})
	mustWrite("auth.provider", "sessions", WriteMeta{Source: "agent"})
	mustWrite("db.engine", "postgres", WriteMeta{Category: "config", Topics: []string{"db", "infra"}})
	mustWrite("doomed", "to be deleted", WriteMeta{})
	if _, _, err := src.Delete("p", "doomed"); err != nil {
		t.Fatalf("seed delete: %v", err)
	}

	data, err := src.Export([]string{"p"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(data.Projects) != 1 || len(data.Projects[0].Memories) != 5 {
		t.Fatalf("export shape: %+v", data)
	}

	dst := newTestStore(t)
	stats, err := dst.Merge(data, "")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(stats) != 1 || stats[0].Restored != 3 || stats[0].Updated != 0 {
		t.Fatalf("stats: %+v", stats)
	}

	// Version chain intact, timestamps and metadata preserved.
	hist, err := dst.History("p", "auth.provider")
	if err != nil || len(hist) != 2 {
		t.Fatalf("history: %+v err=%v", hist, err)
	}
	if hist[0].Version != 2 || hist[0].Content != "sessions" ||
		hist[1].Version != 1 || hist[1].Content != "jwt" || hist[1].Source != "agent" {
		t.Fatalf("history rows: %+v %+v", hist[0], hist[1])
	}
	if hist[0].ParentID == nil || *hist[0].ParentID != hist[1].ID {
		t.Fatalf("parent chain broken: %+v -> %+v", hist[0], hist[1])
	}
	srcHist, _ := src.History("p", "auth.provider")
	if hist[0].CreatedAt != srcHist[0].CreatedAt {
		t.Fatalf("created_at not preserved: %d != %d", hist[0].CreatedAt, srcHist[0].CreatedAt)
	}
	if m, _ := dst.GetLatest("p", "db.engine"); m == nil || m.Category != "config" || len(m.Topics) != 2 {
		t.Fatalf("metadata: %+v", m)
	}
	if m, _ := dst.GetLatest("p", "doomed"); m == nil || !m.Tombstone {
		t.Fatalf("tombstone lost: %+v", m)
	}

	// FTS indexes only current live versions.
	if hits, _ := dst.Search("p", "postgres", SearchFilter{}, 10); len(hits) != 1 {
		t.Fatalf("fts current: %+v", hits)
	}
	if hits, _ := dst.Search("p", "jwt", SearchFilter{}, 10); len(hits) != 0 {
		t.Fatalf("fts indexed superseded version: %+v", hits)
	}
	if hits, _ := dst.Search("p", "deleted", SearchFilter{}, 10); len(hits) != 0 {
		t.Fatalf("fts indexed tombstoned keypath: %+v", hits)
	}

	// Re-importing the same file is a no-op (timestamps tie → local wins).
	stats, err = dst.Merge(data, "")
	if err != nil {
		t.Fatalf("re-merge: %v", err)
	}
	if stats[0].SkippedOlder != 3 || stats[0].Restored != 0 || stats[0].Updated != 0 {
		t.Fatalf("re-merge stats: %+v", stats)
	}

	// Imported project keeps working: a new write supersedes normally.
	v3, prev, err := dst.Write("p", "auth.provider", "oauth", WriteMeta{}, false)
	if err != nil || v3.Version != 3 || prev == nil || prev.Version != 2 {
		t.Fatalf("post-merge write: %+v prev=%+v err=%v", v3, prev, err)
	}
}

func TestMergeNewerWinsOlderSkipped(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Write("p", "k", "local", WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	setCreatedAt(t, s, "p", "k", 1, 1000)
	// Stale vector that must be dropped when the content changes.
	if err := s.UpsertKeypathEmbedding("p", "k", "m", 1, []byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}

	// Older source: local wins, nothing written.
	stats, err := s.Merge(exportOf("p",
		ExportMemory{Keypath: "k", Content: "older remote", Version: 1, CreatedAt: 500}), "")
	if err != nil || stats[0].SkippedOlder != 1 {
		t.Fatalf("older: %+v err=%v", stats, err)
	}
	// Tie: local wins.
	stats, err = s.Merge(exportOf("p",
		ExportMemory{Keypath: "k", Content: "tied remote", Version: 1, CreatedAt: 1000}), "")
	if err != nil || stats[0].SkippedOlder != 1 {
		t.Fatalf("tie: %+v err=%v", stats, err)
	}
	if m, _ := s.GetLatest("p", "k"); m.Content != "local" || m.Version != 1 {
		t.Fatalf("local clobbered: %+v", m)
	}

	// Newer source: replayed through the write path as a new version.
	stats, err = s.Merge(exportOf("p",
		ExportMemory{Keypath: "k", Content: "newer remote", Source: "machine_a", Version: 7, CreatedAt: 2000}), "")
	if err != nil || stats[0].Updated != 1 {
		t.Fatalf("newer: %+v err=%v", stats, err)
	}
	m, _ := s.GetLatest("p", "k")
	if m.Content != "newer remote" || m.Version != 2 || m.Source != "machine_a" {
		t.Fatalf("update: %+v", m)
	}
	if has, _ := s.HasKeypathEmbedding("p", "k", "m"); has {
		t.Fatal("stale embedding survived a content change")
	}
	if hits, _ := s.Search("p", "remote", SearchFilter{}, 10); len(hits) != 1 {
		t.Fatalf("fts after update: %+v", hits)
	}
}

func TestMergeIdenticalContentUnchanged(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Write("p", "k", "same", WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	setCreatedAt(t, s, "p", "k", 1, 1000)
	stats, err := s.Merge(exportOf("p",
		ExportMemory{Keypath: "k", Content: "same", Version: 1, CreatedAt: 2000}), "")
	if err != nil || stats[0].Unchanged != 1 {
		t.Fatalf("stats: %+v err=%v", stats, err)
	}
	if hist, _ := s.History("p", "k"); len(hist) != 1 {
		t.Fatalf("dedupe failed, history: %+v", hist)
	}
}

func TestMergeTombstonePropagates(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Write("p", "k", "alive", WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	setCreatedAt(t, s, "p", "k", 1, 1000)

	stats, err := s.Merge(exportOf("p",
		ExportMemory{Keypath: "k", Version: 2, Tombstone: true, CreatedAt: 2000}), "")
	if err != nil || stats[0].Deleted != 1 {
		t.Fatalf("delete: %+v err=%v", stats, err)
	}
	if m, _ := s.GetLatest("p", "k"); !m.Tombstone {
		t.Fatalf("not tombstoned: %+v", m)
	}
	// Already deleted + newer remote tombstone → unchanged.
	setCreatedAt(t, s, "p", "k", 2, 1500)
	stats, err = s.Merge(exportOf("p",
		ExportMemory{Keypath: "k", Version: 2, Tombstone: true, CreatedAt: 3000}), "")
	if err != nil || stats[0].Unchanged != 1 {
		t.Fatalf("double delete: %+v err=%v", stats, err)
	}
	// Newer remote LIVE value resurrects a locally tombstoned keypath.
	stats, err = s.Merge(exportOf("p",
		ExportMemory{Keypath: "k", Content: "back", Version: 9, CreatedAt: 4000}), "")
	if err != nil || stats[0].Updated != 1 {
		t.Fatalf("resurrect: %+v err=%v", stats, err)
	}
	if m, _ := s.GetLatest("p", "k"); m.Tombstone || m.Content != "back" || m.Version != 3 {
		t.Fatalf("resurrect state: %+v", m)
	}
}

func TestMergeMultiProjectAndRename(t *testing.T) {
	src := newTestStore(t)
	_, _, _ = src.Write("a", "k", "va", WriteMeta{}, false)
	_, _, _ = src.Write("b", "k", "vb", WriteMeta{}, false)
	data, err := src.Export([]string{"a", "b"})
	if err != nil || len(data.Projects) != 2 {
		t.Fatalf("export: %+v err=%v", data, err)
	}

	dst := newTestStore(t)
	stats, err := dst.Merge(data, "")
	if err != nil || len(stats) != 2 {
		t.Fatalf("multi merge: %+v err=%v", stats, err)
	}
	if m, _ := dst.GetLatest("b", "k"); m == nil || m.Content != "vb" {
		t.Fatalf("project b: %+v", m)
	}

	// Rename applies only to single-project files.
	if _, err := dst.Merge(data, "renamed"); err == nil {
		t.Fatal("rename of multi-project file should fail")
	}
	single, _ := src.Export([]string{"a"})
	if _, err := dst.Merge(single, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if m, _ := dst.GetLatest("renamed", "k"); m == nil || m.Content != "va" {
		t.Fatalf("renamed project: %+v", m)
	}
}

func TestMergeRejectsBadInput(t *testing.T) {
	s := newTestStore(t)
	good := exportOf("p", ExportMemory{Keypath: "k", Content: "v", Version: 1, CreatedAt: 1})

	bad := *good
	bad.Format = "something-else"
	if _, err := s.Merge(&bad, ""); err == nil {
		t.Fatal("wrong format accepted")
	}
	bad = *good
	bad.FormatVersion = 99
	if _, err := s.Merge(&bad, ""); err == nil {
		t.Fatal("wrong format_version accepted")
	}
	if _, err := s.Merge(exportOf("p",
		ExportMemory{Keypath: "", Content: "v", Version: 1, CreatedAt: 1}), ""); err == nil {
		t.Fatal("empty keypath accepted")
	}
	if _, err := s.Merge(exportOf("p",
		ExportMemory{Keypath: "k", Content: "v", Version: 0, CreatedAt: 1}), ""); err == nil {
		t.Fatal("version 0 accepted")
	}
	if _, err := s.Merge(exportOf("",
		ExportMemory{Keypath: "k", Content: "v", Version: 1, CreatedAt: 1}), ""); err == nil {
		t.Fatal("missing project_id accepted")
	}
}

func TestExportUnknownOrDeletedProject(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Export([]string{"nope"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	_, _, _ = s.Write("p", "k", "v", WriteMeta{}, false)
	if err := s.DeleteProject("p"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Export([]string{"p"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("soft-deleted project: %v", err)
	}
}

func TestCmdExportImport(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	out := filepath.Join(dir, "a.json")

	a, err := OpenStore(dbA)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = a.Write("p", "k", "v1", WriteMeta{}, false)
	_, _, _ = a.Write("p", "k", "v2", WriteMeta{}, false)
	a.Close()

	if code := cmdExport([]string{"--project", "p", "--db", dbA, "--out", out}); code != 0 {
		t.Fatalf("export exit %d", code)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("export file: %v", err)
	}
	if code := cmdExport([]string{"--project", "p", "--db", dbA, "--out", out}); code == 0 {
		t.Fatal("re-export without --overwrite should fail")
	}
	if code := cmdExport([]string{"--all", "--db", dbA, "--out", out, "--overwrite"}); code != 0 {
		t.Fatal("re-export with --overwrite failed")
	}
	if code := cmdExport([]string{"--db", dbA}); code == 0 {
		t.Fatal("export without --project/--all should fail")
	}

	if code := cmdImport([]string{"--db", dbB, out}); code != 0 {
		t.Fatal("import failed")
	}
	b, err := OpenStore(dbB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	hist, err := b.History("p", "k")
	if err != nil || len(hist) != 2 || hist[0].Content != "v2" {
		t.Fatalf("imported history: %+v err=%v", hist, err)
	}
}
