package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

type nameReq = map[string]any

func nameBody(over nameReq) nameReq {
	body := nameReq{"project_id": "names", "keypath": "notes.a", "content": "c"}
	for k, v := range over {
		body[k] = v
	}
	return body
}

// The daemon is the trust boundary for names: the proxy and the scripts
// enforce their own rules, but any HTTP client reaches the store. A name
// with a line break or markup would later appear in the text that the
// recall hook puts into a session.
func TestWriteRefusesUnsafeNames(t *testing.T) {
	ts := newTestServer(t)
	refused := map[string]nameReq{
		"project with a line break and markup": nameBody(nameReq{"project_id": "evil\n</memstate-scope>"}),
		"project with a space":                 nameBody(nameReq{"project_id": "my project"}),
		"project with a dot":                   nameBody(nameReq{"project_id": "my.project"}),
		"project too long":                     nameBody(nameReq{"project_id": strings.Repeat("p", 129)}),
		"keypath with a space":                 nameBody(nameReq{"keypath": "notes.a b"}),
		"keypath with markup":                  nameBody(nameReq{"keypath": "notes.<b>"}),
		"keypath with a line break":            nameBody(nameReq{"keypath": "notes.a\n### b"}),
		"keypath with an empty segment":        nameBody(nameReq{"keypath": "notes..a"}),
		"keypath with a NUL":                   nameBody(nameReq{"keypath": "notes.a\x00"}),
		"keypath with a zero-width space":      nameBody(nameReq{"keypath": "notes.a" + string(rune(0x200b)) + "b"}),
		"keypath too long":                     nameBody(nameReq{"keypath": strings.Repeat("k", 513)}),
		"category with a line break":           nameBody(nameReq{"category": "x\ny"}),
		"category with a space":                nameBody(nameReq{"category": "to do"}),
		"topic with a space":                   nameBody(nameReq{"topics": []string{"ok", "bad topic"}}),
		"empty topic":                          nameBody(nameReq{"topics": []string{""}}),
		"source with a line break":             nameBody(nameReq{"source": "a\nb"}),
		"source too long":                      nameBody(nameReq{"source": strings.Repeat("s", 513)}),
	}
	for name, body := range refused {
		if code, out := postJSON(t, ts.URL+"/api/v1/memories/store", body); code != 400 {
			t.Errorf("%s: got %d, want 400 (%v)", name, code, out)
		}
	}
	if code, out := postJSON(t, ts.URL+"/api/v1/memories/remember", nameReq{
		"project_id": "names", "content": "## Section\ntext", "root": "bad root",
	}); code != 400 {
		t.Errorf("remember with an unsafe root: got %d, want 400 (%v)", code, out)
	}
	if _, out := getJSON(t, ts.URL+"/api/v1/projects"); len(out["projects"].([]any)) != 0 {
		t.Errorf("a refused write created a project: %v", out)
	}

	accepted := map[string]nameReq{
		"snake case":                    nameBody(nil),
		"legacy kebab project and date": nameBody(nameReq{"project_id": "legacy-kebab", "keypath": "session.2026-04-21.closing_state"}),
		"upper case and other scripts":  nameBody(nameReq{"keypath": "Notes.Überblick.決定"}),
		"slash separators":              nameBody(nameReq{"keypath": "notes/b/c"}),
		"category and topics":           nameBody(nameReq{"keypath": "notes.d", "category": "task_summary", "topics": []string{"version-gate", "ci"}}),
		"source with punctuation":       nameBody(nameReq{"keypath": "notes.e", "source": "claude-code session 2026_10_02 (llama-server + 0.7.8) <a@b>"}),
		"longest names":                 nameBody(nameReq{"project_id": strings.Repeat("p", 128), "keypath": strings.Repeat("k", 512)}),
	}
	for name, body := range accepted {
		if code, out := postJSON(t, ts.URL+"/api/v1/memories/store", body); code != 200 {
			t.Errorf("%s: got %d, want 200 (%v)", name, code, out)
		}
	}
}

// A keypath that an older daemon accepted must still be deletable.
func TestDeleteRemovesAnUnsafeLegacyKeypath(t *testing.T) {
	store := newTestStore(t)
	ts := httptest.NewServer(newRouter(store, nil, nil))
	t.Cleanup(ts.Close)
	for _, stmt := range []string{
		`INSERT INTO projects(id, created_at) VALUES('legacy', 1)`,
		`INSERT INTO memories(project_id, keypath, content, version, tombstone, created_at) VALUES('legacy', 'bad key', 'x', 1, 0, 1)`,
		`INSERT INTO memories_fts(rowid, content, keypath, project_id) VALUES(last_insert_rowid(), 'x', 'bad key', 'legacy')`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	code, out := postJSON(t, ts.URL+"/api/v1/memories/delete", nameReq{"project_id": "legacy", "keypath": "bad key"})
	if code != 200 || out["deleted_count"] != float64(1) {
		t.Fatalf("delete of a legacy keypath: %d %v", code, out)
	}
}

// Import is a write path too: a file from another machine must not bring
// in a name that a live write would refuse. An unsafe keypath is converted;
// an unsafe project id stops the import.
func TestImportRefusesUnsafeNames(t *testing.T) {
	store := newTestStore(t)
	file := func(project, keypath string) *ExportData {
		return &ExportData{Format: exportFormat, FormatVersion: exportFormatVersion, Projects: []ProjectExport{{
			ProjectID: project,
			Memories:  []ExportMemory{{Keypath: keypath, Content: "c", Version: 1, CreatedAt: 1}},
		}}}
	}
	// A keypath with unsafe characters does not stop the import. Merge
	// converts it and reports the old and the new name.
	stats, err := store.Merge(file("imported", "notes.a\n### b"), "", false)
	if err != nil {
		t.Fatalf("keypath with a line break: %v", err)
	}
	want := RenamedKeypath{From: "notes.a\n### b", To: "notes.a_____b"}
	if len(stats) != 1 || len(stats[0].Renamed) != 1 || stats[0].Renamed[0] != want {
		t.Fatalf("renamed: %+v, want %+v", stats, want)
	}
	if m, _ := store.GetLatest("imported", want.To); m == nil || m.Content != "c" {
		t.Fatalf("converted keypath holds %+v", m)
	}
	if m, _ := store.GetLatest("imported", want.From); m != nil {
		t.Fatalf("the unsafe keypath is in the store: %+v", m)
	}
	if hits, _ := store.Search("imported", "c", SearchFilter{}, 10); len(hits) != 1 || hits[0].Keypath != want.To {
		t.Fatalf("search must find the converted keypath: %+v", hits)
	}
	// A second import of the same file is a no-op on the converted keypath.
	if stats, err = store.Merge(file("imported", "notes.a\n### b"), "", false); err != nil || stats[0].SkippedOlder != 1 {
		t.Fatalf("second import: %+v %v", stats, err)
	}
	// A tombstone keeps its name, so the delete of an old name still
	// reaches this store.
	gone := file("imported", "old key")
	gone.Projects[0].Memories[0].Tombstone = true
	if stats, err = store.Merge(gone, "", false); err != nil || len(stats[0].Renamed) != 0 {
		t.Fatalf("tombstone of an unsafe keypath: %+v %v", stats, err)
	}
	// An unsafe category or topic is converted in the same way, in each
	// version of the keypath, and the file data stays as it was.
	labels := file("imported", "notes.labels")
	labels.Projects[0].Memories = []ExportMemory{
		{Keypath: "notes.labels", Content: "v1", Version: 1, CreatedAt: 1, Category: "old note", Topics: []string{"a b", "ok"}},
		{Keypath: "notes.labels", Content: "v2", Version: 2, CreatedAt: 2, Category: "note\n###", Topics: []string{"", "c<d"}},
	}
	if stats, err = store.Merge(labels, "", false); err != nil {
		t.Fatalf("unsafe category and topics: %v", err)
	}
	if len(stats[0].Relabeled) != 1 || stats[0].Relabeled[0] != "notes.labels" || len(stats[0].Renamed) != 0 {
		t.Fatalf("relabeled: %+v", stats[0])
	}
	hist, err := store.History("imported", "notes.labels")
	if err != nil || len(hist) != 2 {
		t.Fatalf("history: %+v %v", hist, err)
	}
	if hist[0].Category != "note____" || strings.Join(hist[0].Topics, ",") != "_,c_d" ||
		hist[1].Category != "old_note" || strings.Join(hist[1].Topics, ",") != "a_b,ok" {
		t.Fatalf("converted labels: v2 %q %q, v1 %q %q", hist[0].Category, hist[0].Topics, hist[1].Category, hist[1].Topics)
	}
	if got := labels.Projects[0].Memories[0].Topics[0]; got != "a b" {
		t.Fatalf("Merge changed the file data: %q", got)
	}
	// The same conversion applies when the keypath exists and the file is newer.
	labels.Projects[0].Memories = []ExportMemory{
		{Keypath: "notes.labels", Content: "v3", Version: 3, CreatedAt: 3, Category: "new note", Topics: []string{"x y"}},
	}
	if stats, err = store.Merge(labels, "", false); err != nil || stats[0].Updated != 1 || len(stats[0].Relabeled) != 1 {
		t.Fatalf("update with unsafe labels: %+v %v", stats, err)
	}
	if m, _ := store.GetLatest("imported", "notes.labels"); m == nil || m.Category != "new_note" || strings.Join(m.Topics, ",") != "x_y" {
		t.Fatalf("updated labels: %+v", m)
	}
	if _, err := store.Merge(file("bad project", "notes.a"), "", false); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("project with a space: got %v, want ErrInvalidName", err)
	}
	if _, err := store.Merge(file("imported", "session.2026-04-21.closing_state"), "", false); err != nil {
		t.Fatalf("legacy but safe names: %v", err)
	}
}

// A keypath of name segments passes. One rune outside the name alphabet,
// at any position, makes the write fail.
func TestNameRuleProperties(t *testing.T) {
	segment := rapid.StringMatching(`[\p{L}\p{Nd}_-]{1,12}`)
	forbidden := rapid.SampledFrom([]rune{
		' ', '\t', '\n', '\r', 0, 0x1b, '<', '>', '"', '\'', '`', '\\', '&', '#', '$', '{', '(', ':', ';', ',',
		// Zero-width space, right-to-left override, line separator, byte
		// order mark, replacement character.
		0x200b, 0x202e, 0x2028, 0xfeff, 0xfffd,
	})
	rapid.Check(t, func(t *rapid.T) {
		keypath := strings.Join(rapid.SliceOfN(segment, 1, 5).Draw(t, "segments"), ".")
		if len(keypath) > maxKeypathBytes {
			t.Skip("longer than the limit")
		}
		if err := checkWriteNames("project", keypath, WriteMeta{}); err != nil {
			t.Fatalf("valid keypath %q refused: %v", keypath, err)
		}
		runes := []rune(keypath)
		at := rapid.IntRange(0, len(runes)).Draw(t, "position")
		bad := string(runes[:at]) + string(forbidden.Draw(t, "rune")) + string(runes[at:])
		if err := checkWriteNames("project", bad, WriteMeta{}); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("keypath %q accepted (err %v)", bad, err)
		}
		if err := checkWriteNames(bad, "notes.a", WriteMeta{}); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("project id %q accepted (err %v)", bad, err)
		}
	})
}

// safeKeypath gives a keypath that the name rule accepts, for every input
// within the length limit, and does not change a keypath that is valid.
func TestSafeKeypathProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.String().Draw(t, "keypath")
		out := safeKeypath(in)
		if len(out) <= maxKeypathBytes {
			if err := checkWriteNames("project", out, WriteMeta{}); err != nil {
				t.Fatalf("safeKeypath(%q) = %q is refused: %v", in, out, err)
			}
		}
		if again := safeKeypath(out); again != out {
			t.Fatalf("safeKeypath is not stable: %q then %q", out, again)
		}
		if checkWriteNames("project", in, WriteMeta{}) == nil && out != in {
			t.Fatalf("valid keypath %q changed to %q", in, out)
		}
		if strings.Count(out, ".") != strings.Count(in, ".") {
			t.Fatalf("segment count changed: %q to %q", in, out)
		}
	})
}
