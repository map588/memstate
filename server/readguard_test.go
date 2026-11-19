package main

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// readKeypaths posts one /keypaths read and returns the answer and its
// memories.
func readKeypaths(t *testing.T, url string, body map[string]any) (map[string]any, []map[string]any) {
	t.Helper()
	code, out := postJSON(t, url+"/api/v1/keypaths", body)
	if code != 200 {
		t.Fatalf("keypaths %v: %d %v", body, code, out)
	}
	var memories []map[string]any
	for _, m := range out["memories"].([]any) {
		memories = append(memories, m.(map[string]any))
	}
	return out, memories
}

// TestKeypathsReadGuardProperty holds the read guard for any subtree and any
// limit. A read is complete, or each memory carries a preview and the size of
// its content: never a part of the content. The limit applies only to a read
// of more than one memory whose content is above it, so one keypath always
// reads in full. The limit never changes which keypaths the read names.
func TestKeypathsReadGuardProperty(t *testing.T) {
	ts := newTestServer(t)
	word := rapid.StringMatching(`[a-z]{1,9}`)
	project := 0
	rapid.Check(t, func(rt *rapid.T) {
		project++
		pid := fmt.Sprintf("guard_%d", project)
		n := rapid.IntRange(1, 6).Draw(rt, "memories")
		contents := map[string]string{}
		total := 0
		for i := 0; i < n; i++ {
			keypath := fmt.Sprintf("area.k%d", i)
			content := strings.Join(rapid.SliceOfN(word, 1, 80).Draw(rt, "words"), " ")
			contents[keypath] = content
			total += len(content)
			if code, out := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
				"project_id": pid, "keypath": keypath, "content": content,
			}); code != 200 {
				t.Fatalf("store %s: %d %v", keypath, code, out)
			}
		}
		limit := rapid.IntRange(0, 900).Draw(rt, "limit")
		_, full := readKeypaths(t, ts.URL, map[string]any{
			"project_id": pid, "keypath": "area", "include_content": true,
		})
		out, got := readKeypaths(t, ts.URL, map[string]any{
			"project_id": pid, "keypath": "area", "include_content": true, "max_content_bytes": limit,
		})
		if len(full) != n || len(got) != n {
			rt.Fatalf("%d memories written, %d read in full, %d read with limit %d", n, len(full), len(got), limit)
		}
		cut := limit > 0 && n > 1 && total > limit
		if truncated, _ := out["truncated"].(bool); truncated != cut {
			rt.Fatalf("truncated = %v, want %v (limit %d, %d memories, %d bytes)", truncated, cut, limit, n, total)
		}
		if note, _ := out["note"].(string); (note != "") != cut {
			rt.Fatalf("note %q with truncated = %v", note, cut)
		}
		for i, m := range got {
			keypath, _ := m["keypath"].(string)
			if keypath != full[i]["keypath"] {
				rt.Fatalf("the limit changed the keypaths: %v, want %v", keypath, full[i]["keypath"])
			}
			want := contents[keypath]
			if !cut {
				if m["content"] != want {
					rt.Fatalf("%s: content changed below the limit", keypath)
				}
				continue
			}
			if _, has := m["content"]; has {
				rt.Fatalf("%s: content in a read above the limit", keypath)
			}
			if size, _ := m["content_bytes"].(float64); int(size) != len(want) {
				rt.Fatalf("%s: content_bytes = %v, want %d", keypath, m["content_bytes"], len(want))
			}
			if m["preview"] != previewWords(want, previewWordCount) {
				rt.Fatalf("%s: preview = %v", keypath, m["preview"])
			}
		}
		// One keypath reads in full with any limit.
		_, one := readKeypaths(t, ts.URL, map[string]any{
			"project_id": pid, "keypath": "area.k0", "include_content": true, "max_content_bytes": 1,
		})
		if len(one) != 1 || one[0]["content"] != contents["area.k0"] {
			rt.Fatalf("one keypath must read in full: %v", one)
		}
	})
}

func TestKeypathsReadGuardExamples(t *testing.T) {
	ts := newTestServer(t)
	// The input that showed the problem: a read of all "decisions" of a
	// project returned 29 KB (2026-11). Three decisions of 4 KB each.
	big := strings.TrimSpace(strings.Repeat("lorem ipsum dolor sit amet ", 150))
	for _, name := range []string{"auth", "naming", "storage"} {
		if code, out := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
			"project_id": "guard", "keypath": "decisions." + name, "content": name + " " + big,
		}); code != 200 {
			t.Fatalf("store: %d %v", code, out)
		}
	}
	out, memories := readKeypaths(t, ts.URL, map[string]any{
		"project_id": "guard", "keypath": "decisions", "include_content": true, "max_content_bytes": 8000,
	})
	if out["truncated"] != true || len(memories) != 3 {
		t.Fatalf("a subtree above the limit must answer with previews: %v", out)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "Read the keypaths that you need") || !strings.Contains(note, "recursive=false") {
		t.Fatalf("note = %q", note)
	}
	// Without a limit the read is complete: the CLI and older clients send none.
	out, memories = readKeypaths(t, ts.URL, map[string]any{
		"project_id": "guard", "keypath": "decisions", "include_content": true,
	})
	if _, has := out["truncated"]; has || memories[0]["content"] != "auth "+big {
		t.Fatalf("a read without a limit must be complete: %v", out["truncated"])
	}
	// A read without content has nothing to cut.
	out, _ = readKeypaths(t, ts.URL, map[string]any{
		"project_id": "guard", "keypath": "decisions", "max_content_bytes": 8000,
	})
	if _, has := out["truncated"]; has {
		t.Fatalf("a read without content must not be marked truncated")
	}
	if code, _ := postJSON(t, ts.URL+"/api/v1/keypaths", map[string]any{
		"project_id": "guard", "keypath": "decisions", "include_content": true, "max_content_bytes": -1,
	}); code != 400 {
		t.Fatalf("a negative limit must be refused, got %d", code)
	}
}
