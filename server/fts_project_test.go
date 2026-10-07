package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestFTSMatch(t *testing.T) {
	cases := []struct{ tokens, project, want string }{
		{`"jwt" AND "cookie"`, "", `{content keypath}: ("jwt" AND "cookie")`},
		{`"jwt"`, "my_app", `{content keypath}: ("jwt") AND project_id: "my_app"`},
		{`"x"`, `we"ird`, `{content keypath}: ("x") AND project_id: "we""ird"`},
	}
	for _, c := range cases {
		if got := ftsMatch(c.tokens, c.project); got != c.want {
			t.Errorf("ftsMatch(%q, %q) = %q, want %q", c.tokens, c.project, got, c.want)
		}
	}
}

// A project search finds only that project's rows, and a word that occurs
// in a project id never matches through the project_id column.
func TestFTSSearchScopedToProject(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, p := range []string{"alpha", "beta"} {
		if _, _, err := store.Write(p, "notes.k", "the cookie expires hourly", WriteMeta{}, false); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := store.Search("alpha", "cookie", SearchFilter{}, 10)
	if err != nil || len(hits) != 1 || hits[0].ProjectID != "alpha" {
		t.Fatalf("project search: hits=%+v err=%v", hits, err)
	}
	hits, err = store.Search("", "cookie", SearchFilter{}, 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("all-projects search: %d hits err=%v", len(hits), err)
	}
	// "alpha" is a project id, not content: no row contains it as text.
	hits, err = store.Search("", "alpha", SearchFilter{}, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("project id must not match as text: hits=%+v err=%v", hits, err)
	}
	if hits, err := store.Search("alpha", "   ", SearchFilter{}, 10); err != nil || len(hits) != 0 {
		t.Fatalf("no tokens must mean no rows: %+v %v", hits, err)
	}
}

// A database with the two-column FTS table from older releases is rebuilt
// on open, and search keeps working.
func TestMigrateFTSProjectColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := strings.Replace(schema,
		"content, keypath, project_id, tokenize = 'porter unicode61'",
		"content, keypath, tokenize = 'porter unicode61'", 1)
	if oldSchema == schema {
		t.Fatal("test setup: schema text changed, update the replacement")
	}
	for _, q := range []string{
		oldSchema,
		`INSERT INTO projects(id, created_at) VALUES('p', 1)`,
		`INSERT INTO memories(id, project_id, keypath, content, version, tombstone, created_at)
		 VALUES(1, 'p', 'notes.k', 'the token expires hourly', 1, 0, 1)`,
		`INSERT INTO memories_fts(rowid, content, keypath) VALUES(1, 'the token expires hourly', 'notes.k')`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("%s: %v", q[:30], err)
		}
	}
	old.Close()

	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('memories_fts') WHERE name='project_id'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("project_id column after migrate: n=%d err=%v", n, err)
	}
	hits, err := store.Search("p", "token", SearchFilter{}, 10)
	if err != nil || len(hits) != 1 || hits[0].Keypath != "notes.k" {
		t.Fatalf("search after migrate: hits=%+v err=%v", hits, err)
	}
	// A second open is a no-op.
	if _, err := OpenStore(path); err != nil {
		t.Fatal(err)
	}
}
