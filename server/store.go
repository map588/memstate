package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS projects (
  id         TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  deleted_at INTEGER
);

CREATE TABLE IF NOT EXISTS memories (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT    NOT NULL REFERENCES projects(id),
  keypath    TEXT    NOT NULL,
  content    TEXT    NOT NULL,
  source     TEXT,
  version    INTEGER NOT NULL,
  parent_id  INTEGER REFERENCES memories(id),
  tombstone  INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_mem_pkv
  ON memories(project_id, keypath, version);

CREATE INDEX IF NOT EXISTS idx_mem_pk
  ON memories(project_id, keypath);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  content, keypath, tokenize = 'porter unicode61'
);

-- Stub kept for historical compatibility; current semantic-search code uses
-- keypath_embeddings (one row per keypath, not per memory version).
CREATE TABLE IF NOT EXISTS embeddings (
  memory_id INTEGER PRIMARY KEY REFERENCES memories(id),
  model     TEXT    NOT NULL,
  dim       INTEGER NOT NULL,
  vector    BLOB    NOT NULL
);

-- Semantic search embeds the keypath string (not content). Keypaths are
-- stable across versions, so one row per (project, keypath, model) is both
-- cheaper and avoids re-embedding on every remember-at-same-keypath.
CREATE TABLE IF NOT EXISTS keypath_embeddings (
  project_id TEXT    NOT NULL,
  keypath    TEXT    NOT NULL,
  model      TEXT    NOT NULL,
  dim        INTEGER NOT NULL,
  vector     BLOB    NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, keypath, model)
);
`

// Memory is one row in the version chain for a (project_id, keypath).
type Memory struct {
	ID        int64  `json:"id"`
	ProjectID string `json:"project_id"`
	Keypath   string `json:"keypath"`
	Content   string `json:"content"`
	Source    string `json:"source,omitempty"`
	Version   int    `json:"version"`
	ParentID  *int64 `json:"parent_id,omitempty"`
	Tombstone bool   `json:"tombstone,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	// Apply PRAGMAs per connection via DSN so every pooled connection
	// shares busy-wait behaviour and WAL mode. SQLite PRAGMAs set on one
	// connection do not propagate to siblings.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// NormalizeKeypath accepts dot- or slash-separated paths and returns dot form.
func NormalizeKeypath(k string) string {
	k = strings.TrimSpace(k)
	k = strings.Trim(k, "./")
	k = strings.ReplaceAll(k, "/", ".")
	return k
}

// ProjectDeleted reports whether a project row exists AND is soft-deleted.
// A non-existent project is NOT deleted (writes will create it).
func (s *Store) ProjectDeleted(id string) (bool, error) {
	var deletedAt sql.NullInt64
	err := s.db.QueryRow(`SELECT deleted_at FROM projects WHERE id=?`, id).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return deletedAt.Valid, nil
}

func (s *Store) ensureProject(id string) error {
	_, err := s.db.Exec(
		`INSERT INTO projects(id, created_at) VALUES(?, ?)
		 ON CONFLICT(id) DO UPDATE SET deleted_at = NULL`,
		id, time.Now().Unix(),
	)
	return err
}

func scanMemory(scan func(dest ...any) error) (*Memory, error) {
	m := &Memory{}
	var src sql.NullString
	var parent sql.NullInt64
	var tomb int
	if err := scan(&m.ID, &m.ProjectID, &m.Keypath, &m.Content, &src,
		&m.Version, &parent, &tomb, &m.CreatedAt); err != nil {
		return nil, err
	}
	if src.Valid {
		m.Source = src.String
	}
	if parent.Valid {
		m.ParentID = &parent.Int64
	}
	m.Tombstone = tomb != 0
	return m, nil
}

// memoryCols is always qualified so queries can safely join FTS5 (which also
// has a keypath column) without provoking "ambiguous column" errors.
const memoryCols = `m.id, m.project_id, m.keypath, m.content, m.source, m.version, m.parent_id, m.tombstone, m.created_at`

// dbExec abstracts the subset of *sql.DB / *sql.Tx used by write helpers,
// so the same code path can run inside a batch transaction or stand-alone.
type dbExec interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// GetLatest returns the current (highest version) memory at keypath, or nil if absent.
// A tombstoned latest is returned so callers can distinguish deleted from never-set.
func (s *Store) GetLatest(projectID, keypath string) (*Memory, error) {
	return getLatestExec(s.db, projectID, keypath)
}

func getLatestExec(exec dbExec, projectID, keypath string) (*Memory, error) {
	row := exec.QueryRow(
		`SELECT `+memoryCols+` FROM memories m
		 WHERE m.project_id=? AND m.keypath=?
		 ORDER BY m.version DESC LIMIT 1`,
		projectID, keypath,
	)
	m, err := scanMemory(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// Write appends a new version at keypath. Returns the new memory and the
// superseded previous (nil if none).
func (s *Store) Write(projectID, keypath, content, source string, tombstone bool) (*Memory, *Memory, error) {
	if err := s.ensureProject(projectID); err != nil {
		return nil, nil, err
	}
	return writeExec(s.db, projectID, keypath, content, source, tombstone)
}

// writeExec is the tx-capable core of Write. Caller is responsible for
// ensureProject; within a batch tx we hoist that call outside the loop.
//
// Idempotency: if the prior live version has identical content (and this
// call is not a tombstone), no new row is written. Both return values
// point at the same prior memory — callers detect "unchanged" via pointer
// or ID equality.
func writeExec(exec dbExec, projectID, keypath, content, source string, tombstone bool) (*Memory, *Memory, error) {
	prev, err := getLatestExec(exec, projectID, keypath)
	if err != nil {
		return nil, nil, err
	}
	if !tombstone && prev != nil && !prev.Tombstone && prev.Content == content {
		return prev, prev, nil
	}
	nextVer := 1
	var parent sql.NullInt64
	if prev != nil {
		nextVer = prev.Version + 1
		parent = sql.NullInt64{Int64: prev.ID, Valid: true}
	}
	now := time.Now().Unix()
	tombVal := 0
	if tombstone {
		tombVal = 1
	}
	var srcArg any
	if source != "" {
		srcArg = source
	}
	res, err := exec.Exec(
		`INSERT INTO memories(project_id, keypath, content, source, version, parent_id, tombstone, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, keypath, content, srcArg, nextVer, parent, tombVal, now,
	)
	if err != nil {
		return nil, nil, err
	}
	id, _ := res.LastInsertId()
	if !tombstone {
		if _, err := exec.Exec(
			`INSERT INTO memories_fts(rowid, content, keypath) VALUES(?, ?, ?)`,
			id, content, keypath,
		); err != nil {
			return nil, nil, err
		}
	}
	m := &Memory{
		ID: id, ProjectID: projectID, Keypath: keypath, Content: content,
		Source: source, Version: nextVer, Tombstone: tombstone, CreatedAt: now,
	}
	if parent.Valid {
		pid := parent.Int64
		m.ParentID = &pid
	}
	return m, prev, nil
}

// HasKeypathEmbedding reports whether an embedding row exists for
// (project, keypath, model). Used to skip redundant Ollama calls when a
// keypath has already been embedded under the current model.
func (s *Store) HasKeypathEmbedding(projectID, keypath, model string) (bool, error) {
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM keypath_embeddings
		 WHERE project_id=? AND keypath=? AND model=?`,
		projectID, keypath, model,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// UpsertKeypathEmbedding inserts or replaces the vector for (project, keypath, model).
func (s *Store) UpsertKeypathEmbedding(projectID, keypath, model string, dim int, vector []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO keypath_embeddings(project_id, keypath, model, dim, vector, created_at)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, keypath, model) DO UPDATE SET
		   dim=excluded.dim, vector=excluded.vector, created_at=excluded.created_at`,
		projectID, keypath, model, dim, vector, time.Now().Unix(),
	)
	return err
}

// KeypathEmbedding is a row from keypath_embeddings, exposed to the search
// path after the vector is unpacked.
type KeypathEmbedding struct {
	ProjectID string
	Keypath   string
	Vector    []float32
}

// ListKeypathEmbeddings returns every (project, keypath, vector) row for a
// given model, optionally scoped to a single project. Vectors are unpacked
// from BLOB so the caller can compute cosine in memory.
func (s *Store) ListKeypathEmbeddings(projectID, model string) ([]*KeypathEmbedding, error) {
	q := `SELECT project_id, keypath, vector FROM keypath_embeddings WHERE model=?`
	args := []any{model}
	if projectID != "" {
		q += ` AND project_id=?`
		args = append(args, projectID)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*KeypathEmbedding
	for rows.Next() {
		var pid, kp string
		var blob []byte
		if err := rows.Scan(&pid, &kp, &blob); err != nil {
			return nil, err
		}
		vec, err := unpackVector(blob)
		if err != nil {
			return nil, fmt.Errorf("decode vector for %s/%s: %w", pid, kp, err)
		}
		out = append(out, &KeypathEmbedding{ProjectID: pid, Keypath: kp, Vector: vec})
	}
	return out, rows.Err()
}

// SemanticHit pairs a current-version memory with its cosine similarity to
// the query vector. Sorted by Score descending by the caller.
type SemanticHit struct {
	*Memory
	Score float32 `json:"score"`
}

// SemanticSearch ranks keypaths in the given project (or all projects if
// projectID is empty) by cosine similarity to query. Returns at most limit
// hits whose similarity is >= threshold, paired with the current non-
// tombstoned memory at that keypath.
//
// Callers that want to skip the embedding lookup (e.g. because they already
// have a vector) can pass the pre-embedded query directly.
func (s *Store) SemanticSearch(projectID string, query []float32, model string, threshold float32, limit int) ([]*SemanticHit, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.ListKeypathEmbeddings(projectID, model)
	if err != nil {
		return nil, err
	}
	type ranked struct {
		pid, kp string
		score   float32
	}
	picks := make([]ranked, 0, len(rows))
	for _, r := range rows {
		sc := cosine(query, r.Vector)
		if sc >= threshold {
			picks = append(picks, ranked{r.ProjectID, r.Keypath, sc})
		}
	}
	// Sort descending by score (simple insertion sort, N is small).
	for i := 1; i < len(picks); i++ {
		for j := i; j > 0 && picks[j].score > picks[j-1].score; j-- {
			picks[j], picks[j-1] = picks[j-1], picks[j]
		}
	}
	if len(picks) > limit {
		picks = picks[:limit]
	}
	out := make([]*SemanticHit, 0, len(picks))
	for _, p := range picks {
		m, err := s.GetLatest(p.pid, p.kp)
		if err != nil {
			return nil, err
		}
		if m == nil || m.Tombstone {
			continue
		}
		out = append(out, &SemanticHit{Memory: m, Score: p.score})
	}
	return out, nil
}

// BatchItem reports the outcome of one section in a WriteBatch call.
type BatchItem struct {
	Keypath    string
	Stored     *Memory
	Superseded *Memory
}

// WriteBatch writes all sections under a single transaction. On any per-row
// error the whole batch is rolled back — callers see no partial state.
// Source applies to every row. Sections are processed in order, so a later
// section can observe a prior section's write via parent_id chain.
func (s *Store) WriteBatch(projectID string, sections []Section, source string) ([]BatchItem, error) {
	if len(sections) == 0 {
		return nil, nil
	}
	if err := s.ensureProject(projectID); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	out := make([]BatchItem, 0, len(sections))
	for _, sec := range sections {
		kp := NormalizeKeypath(sec.Keypath)
		stored, prev, err := writeExec(tx, projectID, kp, sec.Content, source, false)
		if err != nil {
			return nil, err
		}
		out = append(out, BatchItem{Keypath: kp, Stored: stored, Superseded: prev})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return out, nil
}

// List returns current-version, non-tombstoned memories matching keypath.
// If keypath is empty, returns everything under the project.
// Otherwise matches exact keypath OR children (prefix + ".").
func (s *Store) List(projectID, keypath string) ([]*Memory, error) {
	q := `
		SELECT ` + memoryCols + ` FROM memories m
		WHERE m.project_id = ?
		  AND m.tombstone = 0
		  AND m.version = (
		    SELECT MAX(version) FROM memories m2
		    WHERE m2.project_id = m.project_id AND m2.keypath = m.keypath
		  )
	`
	args := []any{projectID}
	if keypath != "" {
		q += ` AND (m.keypath = ? OR m.keypath LIKE ?)`
		args = append(args, keypath, keypath+".%")
	}
	q += ` ORDER BY m.keypath`
	return s.queryMemories(q, args...)
}

// History returns every version at keypath (including tombstones), newest first.
func (s *Store) History(projectID, keypath string) ([]*Memory, error) {
	return s.queryMemories(
		`SELECT `+memoryCols+` FROM memories m
		 WHERE m.project_id=? AND m.keypath=?
		 ORDER BY m.version DESC`,
		projectID, keypath,
	)
}

// Search does an FTS5 match on content+keypath, restricted to current non-tombstoned versions.
func (s *Store) Search(projectID, query string, limit int) ([]*Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `
		SELECT ` + memoryCols + ` FROM memories m
		JOIN memories_fts fts ON fts.rowid = m.id
		WHERE memories_fts MATCH ?
		  AND m.tombstone = 0
		  AND m.version = (
		    SELECT MAX(version) FROM memories m2
		    WHERE m2.project_id = m.project_id AND m2.keypath = m.keypath
		  )
	`
	args := []any{query}
	if projectID != "" {
		q += ` AND m.project_id = ?`
		args = append(args, projectID)
	}
	q += ` ORDER BY fts.rank LIMIT ?`
	args = append(args, limit)
	return s.queryMemories(q, args...)
}

func (s *Store) queryMemories(q string, args ...any) ([]*Memory, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Memory
	for rows.Next() {
		m, err := scanMemory(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Delete tombstones a keypath by appending a new tombstone version.
func (s *Store) Delete(projectID, keypath string) (*Memory, *Memory, error) {
	prev, err := s.GetLatest(projectID, keypath)
	if err != nil {
		return nil, nil, err
	}
	if prev == nil {
		return nil, nil, fmt.Errorf("keypath %q not found in project %q", keypath, projectID)
	}
	if prev.Tombstone {
		return prev, prev, nil
	}
	return s.Write(projectID, keypath, "", "", true)
}

// DeleteProject marks the project row deleted_at. Memories remain for history.
func (s *Store) DeleteProject(projectID string) error {
	res, err := s.db.Exec(`UPDATE projects SET deleted_at=? WHERE id=? AND deleted_at IS NULL`,
		time.Now().Unix(), projectID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %q not found or already deleted", projectID)
	}
	return nil
}

// DeleteSubtree tombstones every current, non-tombstoned keypath at prefix OR
// its descendants. Used for recursive deletes. Returns the keypaths that were
// tombstoned in this call.
func (s *Store) DeleteSubtree(projectID, prefix string) ([]string, error) {
	prefix = NormalizeKeypath(prefix)
	live, err := s.List(projectID, prefix)
	if err != nil {
		return nil, err
	}
	done := make([]string, 0, len(live))
	for _, m := range live {
		if _, _, err := s.Write(projectID, m.Keypath, "", "", true); err != nil {
			return done, err
		}
		done = append(done, m.Keypath)
	}
	return done, nil
}

// Project is a row from the projects table.
type Project struct {
	ID            string `json:"id"`
	CreatedAt     int64  `json:"created_at"`
	MemoryCount   int    `json:"memory_count"`
	LastUpdatedAt int64  `json:"last_updated_at,omitempty"`
}

// ListProjects returns every live (non-deleted) project plus a memory count
// from its current, non-tombstoned keypaths.
func (s *Store) ListProjects() ([]*Project, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.created_at,
		       COALESCE(SUM(CASE WHEN m.tombstone=0
		                          AND m.version=(SELECT MAX(version) FROM memories m2
		                                          WHERE m2.project_id=m.project_id
		                                            AND m2.keypath=m.keypath)
		                          THEN 1 ELSE 0 END), 0) AS mem_count,
		       COALESCE(MAX(m.created_at), 0) AS last_updated_at
		FROM projects p
		LEFT JOIN memories m ON m.project_id = p.id
		WHERE p.deleted_at IS NULL
		GROUP BY p.id, p.created_at
		ORDER BY p.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p := &Project{}
		if err := rows.Scan(&p.ID, &p.CreatedAt, &p.MemoryCount, &p.LastUpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetByID fetches a single memory row by its integer primary key.
func (s *Store) GetByID(id int64) (*Memory, error) {
	row := s.db.QueryRow(
		`SELECT `+memoryCols+` FROM memories m WHERE m.id=?`, id)
	m, err := scanMemory(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// TreeNode is a node in the hierarchical keypath tree returned by GET /tree.
type TreeNode struct {
	Name     string      `json:"name"`
	Keypath  string      `json:"keypath,omitempty"`
	Children []*TreeNode `json:"children,omitempty"`
	HasValue bool        `json:"has_value,omitempty"`
	Version  int         `json:"version,omitempty"`
}

// Tree builds a nested structure of current, non-tombstoned keypaths for a project.
func (s *Store) Tree(projectID string) (*TreeNode, error) {
	list, err := s.List(projectID, "")
	if err != nil {
		return nil, err
	}
	root := &TreeNode{Name: projectID}
	for _, m := range list {
		insertPath(root, strings.Split(m.Keypath, "."), m.Keypath, m.Version)
	}
	return root, nil
}

func insertPath(parent *TreeNode, parts []string, fullKeypath string, version int) {
	if len(parts) == 0 {
		parent.HasValue = true
		parent.Keypath = fullKeypath
		parent.Version = version
		return
	}
	name := parts[0]
	var child *TreeNode
	for _, c := range parent.Children {
		if c.Name == name {
			child = c
			break
		}
	}
	if child == nil {
		child = &TreeNode{Name: name}
		parent.Children = append(parent.Children, child)
	}
	insertPath(child, parts[1:], fullKeypath, version)
}
