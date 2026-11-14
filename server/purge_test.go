package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// rawSnapshot reads each raw table in full, as "table|cell|cell..." lines.
func rawSnapshot(t interface {
	Fatalf(format string, args ...any)
}, s *Store) []string {
	var out []string
	for _, tab := range rawTables {
		_, rows, err := s.RawRows(tab.name, 0, 1<<20)
		if err != nil {
			t.Fatalf("read %s: %v", tab.name, err)
		}
		for _, r := range rows {
			out = append(out, tab.name+"|"+strings.Join(r, "|"))
		}
	}
	return out
}

func TestPurge(t *testing.T) {
	s := newTestStore(t)
	must := func(kp, content string) {
		t.Helper()
		if _, _, err := s.Write("p", kp, content, WriteMeta{}, false); err != nil {
			t.Fatal(err)
		}
	}
	must("note", "secret one")
	must("note", "secret two")
	must("note", "secret three")
	must("note.child", "below the note")
	must("notes", "a sibling that shares the prefix")
	must("gone", "tombstoned before the purge")
	if _, _, err := s.Delete("p", "gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertKeypathEmbedding("p", "note", "m1", 2, []byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}

	// The preview lists what a purge erases, and erases nothing.
	before := rawSnapshot(t, s)
	prev, err := s.PurgePreview("p", "note", false)
	if err != nil || len(prev) != 1 || prev[0].Keypath != "note" || prev[0].Versions != 3 {
		t.Fatalf("preview: %+v err=%v", prev, err)
	}
	if !slices.Equal(before, rawSnapshot(t, s)) {
		t.Fatal("the preview changed the store")
	}

	// One keypath: each version, the search row and the vector go. The
	// child and the sibling stay.
	done, err := s.Purge("p", "note", false)
	if err != nil || len(done) != 1 || done[0].Versions != 3 {
		t.Fatalf("purge: %+v err=%v", done, err)
	}
	if hist, _ := s.History("p", "note"); len(hist) != 0 {
		t.Fatalf("history after purge: %+v", hist)
	}
	for _, line := range rawSnapshot(t, s) {
		if strings.Contains(line, "secret") {
			t.Fatalf("purged content is still in the store: %s", line)
		}
		if strings.HasPrefix(line, "keypath_embeddings|") {
			t.Fatalf("the vector is still in the store: %s", line)
		}
	}
	if hits, _ := s.SearchAny("p", "secret", SearchFilter{}, 10); len(hits) != 0 {
		t.Fatalf("search finds purged content: %+v", hits)
	}
	for _, kp := range []string{"note.child", "notes"} {
		if m, _ := s.GetLatest("p", kp); m == nil || m.Tombstone {
			t.Fatalf("%s did not survive: %+v", kp, m)
		}
	}

	// A write to the keypath after the purge starts a new chain.
	if m, _, err := s.Write("p", "note", "a new note", WriteMeta{}, false); err != nil || m.Version != 1 {
		t.Fatalf("write after purge: %+v err=%v", m, err)
	}

	// A subtree: the keypath, each keypath below, tombstoned chains too.
	// "notes" only shares the prefix.
	must("gone.below", "x")
	done, err = s.Purge("p", "gone", true)
	if err != nil || len(done) != 2 || done[0].Keypath != "gone" || done[0].Versions != 2 || done[1].Keypath != "gone.below" {
		t.Fatalf("subtree purge: %+v err=%v", done, err)
	}
	if m, _ := s.GetLatest("p", "notes"); m == nil {
		t.Fatal("the subtree purge took a sibling")
	}

	// Nothing to purge is an error, and an empty keypath is not "all".
	if _, err := s.Purge("p", "absent", true); err == nil {
		t.Fatal("purge of an absent keypath must be an error")
	}
	if _, err := s.Purge("p", "", true); err == nil {
		t.Fatal("purge of the empty keypath must be an error")
	}
	if m, _ := s.GetLatest("p", "notes"); m == nil {
		t.Fatal("a refused purge erased rows")
	}
}

// A purge removes each row of its keypaths from each table and changes no
// other row.
func TestPurgeProperty(t *testing.T) {
	dir := t.TempDir()
	run := 0
	rapid.Check(t, func(rt *rapid.T) {
		run++
		s, err := OpenStore(filepath.Join(dir, fmt.Sprintf("purge%d.db", run)))
		if err != nil {
			rt.Fatal(err)
		}
		defer s.Close()
		projects := []string{"alpha", "beta"}
		keys := []string{"a", "a.b", "a.b.c", "a_b", "ab", "n.x", "n"}
		for i, n := 0, rapid.IntRange(1, 25).Draw(rt, "writes"); i < n; i++ {
			pid := rapid.SampledFrom(projects).Draw(rt, "project")
			kp := rapid.SampledFrom(keys).Draw(rt, "keypath")
			if rapid.IntRange(0, 4).Draw(rt, "tombstone") == 0 {
				_, _, _ = s.Write(pid, kp, "", WriteMeta{}, true)
				continue
			}
			if _, _, err := s.Write(pid, kp, rapid.StringMatching(`[a-z ]{1,12}`).Draw(rt, "content"), WriteMeta{}, false); err != nil {
				rt.Fatal(err)
			}
			if rapid.Bool().Draw(rt, "vector") {
				if err := s.UpsertKeypathEmbedding(pid, kp, "m1", 2, []byte{0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
					rt.Fatal(err)
				}
			}
		}
		pid := rapid.SampledFrom(projects).Draw(rt, "purge_project")
		target := rapid.SampledFrom(keys).Draw(rt, "purge_keypath")
		recursive := rapid.Bool().Draw(rt, "recursive")
		hit := func(project, kp string) bool {
			return project == pid && (kp == target || (recursive && strings.HasPrefix(kp, target+".")))
		}
		// A raw row belongs to the purge when its project and keypath
		// cells match. The column order differs by table.
		rowHit := func(line string) bool {
			c := strings.Split(line, "|")
			switch c[0] {
			case "memories":
				return hit(c[2], c[3])
			case "memories_fts":
				return hit(c[2], c[3])
			case "keypath_embeddings":
				return hit(c[1], c[2])
			}
			return false
		}

		var want []string
		for _, line := range rawSnapshot(rt, s) {
			if !rowHit(line) {
				want = append(want, line)
			}
		}
		done, err := s.Purge(pid, target, recursive)
		got := rawSnapshot(rt, s)
		if err != nil {
			// Nothing matched: the store is as it was.
			if !slices.Equal(got, want) {
				rt.Fatalf("a refused purge changed the store")
			}
			return
		}
		var markers []string
		got = slices.DeleteFunc(got, func(line string) bool {
			if strings.HasPrefix(line, "purged|") {
				markers = append(markers, line)
				return true
			}
			return false
		})
		if !slices.Equal(got, want) {
			rt.Fatalf("purge %s/%s recursive=%v\n got: %v\nwant: %v", pid, target, recursive, got, want)
		}
		if len(markers) != len(done) {
			rt.Fatalf("%d markers for %d purged keypaths", len(markers), len(done))
		}
	})
}

// The scenario that the marker exists for: a note is purged here, and a
// file from a machine that still has the note is imported.
func TestMergeDoesNotRestorePurged(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []string{"unwanted v1", "unwanted v2"} {
		if _, _, err := s.Write("p", "note", c, WriteMeta{}, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Write("p", "keep", "wanted", WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	other, err := s.Export([]string{"p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Purge("p", "note", false); err != nil {
		t.Fatal(err)
	}

	for _, force := range []bool{false, true} {
		stats, err := s.Merge(other, "", force)
		if err != nil {
			t.Fatalf("merge force=%v: %v", force, err)
		}
		if hist, _ := s.History("p", "note"); len(hist) != 0 {
			t.Fatalf("force=%v: the import restored a purged keypath: %+v", force, hist)
		}
		if !slices.Equal(stats[0].SkippedPurged, []string{"note"}) || stats[0].Restored != 0 {
			t.Fatalf("force=%v: stats %+v", force, stats[0])
		}
	}

	// The other machine wrote the keypath again after the purge. Only the
	// versions after the purge come in.
	var at int64
	if err := s.db.QueryRow(`SELECT purged_at FROM purged WHERE project_id='p' AND keypath='note'`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	newer := exportOf("p",
		ExportMemory{Keypath: "note", Content: "unwanted v1", Version: 1, CreatedAt: at - 20},
		ExportMemory{Keypath: "note", Content: "unwanted v2", Version: 2, CreatedAt: at - 10},
		ExportMemory{Keypath: "note", Content: "a later, wanted note", Version: 3, CreatedAt: at + 10},
	)
	stats, err := s.Merge(newer, "", false)
	if err != nil {
		t.Fatal(err)
	}
	hist, _ := s.History("p", "note")
	if len(hist) != 1 || hist[0].Version != 3 || hist[0].Content != "a later, wanted note" || hist[0].ParentID != nil {
		t.Fatalf("history after the newer import: %+v (stats %+v)", hist, stats[0])
	}
	for _, line := range rawSnapshot(t, s) {
		if strings.Contains(line, "unwanted") {
			t.Fatalf("purged content came back: %s", line)
		}
	}
	// The same file again changes nothing.
	before := rawSnapshot(t, s)
	if _, err := s.Merge(newer, "", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, rawSnapshot(t, s)) {
		t.Fatal("a second import of the same file changed the store")
	}
}
