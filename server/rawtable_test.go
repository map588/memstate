package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// The raw table reader must show each row one time. A page boundary must
// not drop a row and must not repeat a row, for each table and each page
// size.
func TestRawRowsPagingProperty(t *testing.T) {
	dir := t.TempDir()
	n := 0
	rapid.Check(t, func(rt *rapid.T) {
		n++
		s, err := OpenStore(filepath.Join(dir, fmt.Sprintf("raw%d.db", n)))
		if err != nil {
			rt.Fatal(err)
		}
		defer s.Close()

		projects := []string{"alpha", "beta", sharedProject}
		keys := []string{"a", "a.b", "notes.x", "todo"}
		writes := rapid.IntRange(0, 25).Draw(rt, "writes")
		for i := 0; i < writes; i++ {
			pid := rapid.SampledFrom(projects).Draw(rt, "project")
			kp := rapid.SampledFrom(keys).Draw(rt, "keypath")
			if pid == sharedProject {
				kp = "preferences." + strings.ReplaceAll(kp, ".", "_")
			}
			tomb := rapid.IntRange(0, 4).Draw(rt, "tombstone") == 0
			content := rapid.StringMatching(`[a-z ]{1,20}`).Draw(rt, "content")
			if tomb {
				content = ""
			}
			if _, _, err := s.Write(pid, kp, content, WriteMeta{}, tomb); err != nil {
				rt.Fatalf("write %s/%s: %v", pid, kp, err)
			}
		}
		if rapid.Bool().Draw(rt, "embedding") {
			if err := s.UpsertKeypathEmbedding("alpha", "a", "m1", 2, []byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
				rt.Fatal(err)
			}
		}

		counts, err := s.RawTableCounts()
		if err != nil {
			rt.Fatal(err)
		}
		if len(counts) != len(rawTables) {
			rt.Fatalf("counts for %d tables, want %d", len(counts), len(rawTables))
		}
		for _, c := range counts {
			cols, all, err := s.RawRows(c.Name, 0, c.Count+10)
			if err != nil {
				rt.Fatalf("%s: %v", c.Name, err)
			}
			if len(all) != c.Count {
				rt.Fatalf("%s: %d rows, count(*) is %d", c.Name, len(all), c.Count)
			}
			size := rapid.IntRange(1, 7).Draw(rt, "page_size")
			var paged [][]string
			for off := 0; ; off += size {
				_, page, err := s.RawRows(c.Name, off, size)
				if err != nil {
					rt.Fatalf("%s offset %d: %v", c.Name, off, err)
				}
				for _, row := range page {
					if len(row) != len(cols) {
						rt.Fatalf("%s: row has %d cells, %d columns", c.Name, len(row), len(cols))
					}
				}
				paged = append(paged, page...)
				if len(page) < size {
					break
				}
			}
			if !slices.EqualFunc(paged, all, func(a, b []string) bool { return slices.Equal(a, b) }) {
				rt.Fatalf("%s: pages of %d differ from one read\npaged=%v\nall=%v", c.Name, size, paged, all)
			}
		}
	})
}

func TestRawRowsShape(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Write("proj", "a.b", "hello", WriteMeta{Category: "note"}, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Write("proj", "a.b", "", WriteMeta{}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertKeypathEmbedding("proj", "a.b", "m1", 2, []byte{0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}

	// A name that is not in the fixed list is refused. The name never
	// reaches SQL text.
	if _, _, err := s.RawRows("memories; DROP TABLE memories", 0, 10); err == nil {
		t.Fatal("unknown table name must be an error")
	}

	// The memories table shows each version, the tombstone too.
	cols, rows, err := s.RawRows("memories", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("memories rows = %d, want 2 (one version, one tombstone)", len(rows))
	}
	tomb := slices.Index(cols, "tombstone")
	if tomb < 0 || rows[0][tomb] != "0" || rows[1][tomb] != "1" {
		t.Fatalf("tombstone column: cols=%v rows=%v", cols, rows)
	}
	parent := slices.Index(cols, "parent_id")
	if rows[0][parent] != rawNull {
		t.Fatalf("parent_id of version 1 = %q, want %q", rows[0][parent], rawNull)
	}

	// The vector is a size, not bytes.
	cols, rows, err = s.RawRows("keypath_embeddings", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	vec := slices.Index(cols, "vector")
	if len(rows) != 1 || rows[0][vec] != "<8 bytes>" {
		t.Fatalf("vector cell: cols=%v rows=%v", cols, rows)
	}
}
