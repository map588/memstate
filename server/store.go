package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
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
  category   TEXT,
  topics     TEXT,
  version    INTEGER NOT NULL,
  parent_id  INTEGER REFERENCES memories(id),
  tombstone  INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_mem_pkv
  ON memories(project_id, keypath, version);

-- idx_mem_pk was a prefix of the unique index above; dropped as redundant.
DROP INDEX IF EXISTS idx_mem_pk;

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  content, keypath, tokenize = 'porter unicode61'
);

-- Semantic search embeds the CONTENT of the current version at each keypath;
-- one row per (project, keypath, model), upserted whenever content changes.
CREATE TABLE IF NOT EXISTS keypath_embeddings (
  project_id TEXT    NOT NULL,
  keypath    TEXT    NOT NULL,
  model      TEXT    NOT NULL,
  dim        INTEGER NOT NULL,
  vector     BLOB    NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, keypath, model)
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

// embedSource names what the stored vectors were computed from. Bumping this
// value wipes keypath_embeddings on startup so stale vectors (e.g. from the
// earlier keypath-string scheme) never mix with the current one; vectors
// rebuild lazily via the heal path in maybeEmbedContent.
const embedSource = "content"

// Memory is one row in the version chain for a (project_id, keypath).
type Memory struct {
	ID        int64    `json:"id"`
	ProjectID string   `json:"project_id"`
	Keypath   string   `json:"keypath"`
	Content   string   `json:"content"`
	Source    string   `json:"source,omitempty"`
	Category  string   `json:"category,omitempty"`
	Topics    []string `json:"topics,omitempty"`
	Version   int      `json:"version"`
	ParentID  *int64   `json:"parent_id,omitempty"`
	Tombstone bool     `json:"tombstone,omitempty"`
	CreatedAt int64    `json:"created_at"`
}

// WriteMeta carries the per-version metadata of a write. The zero value is
// valid (no source, no category, no topics).
type WriteMeta struct {
	Source   string
	Category string
	Topics   []string
}

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	// Apply PRAGMAs per connection via DSN so every pooled connection
	// inherits the same behaviour. SQLite PRAGMAs set on one connection do
	// not propagate to siblings, so the DSN is the only reliable place.
	//
	// - busy_timeout=5000: writers wait up to 5s when another writer holds
	//   the DB lock. Belt-and-suspenders with MaxOpenConns=1 below.
	// - journal_mode=WAL: concurrent readers + single writer.
	// - synchronous=NORMAL: safe under WAL, faster than FULL.
	// - cache_size=-64000: 64MB page cache (negative = KB).
	// - mmap_size=268435456: 256MB memory-mapped reads. Safe because we
	//   use a single pooled connection (see SetMaxOpenConns below), so the
	//   cross-connection stale-read race that can surface on macOS does
	//   not apply here.
	// - temp_store=MEMORY: keep temp tables / sort scratch in RAM.
	// - foreign_keys=1: enforce FK constraints.
	pragmas := []string{
		"busy_timeout(5000)",
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"cache_size(-64000)",
		"mmap_size(268435456)",
		"temp_store(MEMORY)",
		"foreign_keys(1)",
	}
	var dsn strings.Builder
	dsn.WriteString(path)
	for i, p := range pragmas {
		if i == 0 {
			dsn.WriteByte('?')
		} else {
			dsn.WriteByte('&')
		}
		dsn.WriteString("_pragma=")
		dsn.WriteString(p)
	}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	// Single connection serializes all DB access. At this scale (small
	// writes, small reads, no long-running queries) the contention savings
	// outweigh the parallelism loss, and it sidesteps modernc.org/sqlite's
	// per-connection PRAGMA quirks under concurrent fire-and-forget embeds.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate converges DBs created under older schemas. CREATE TABLE IF NOT
// EXISTS never adds columns to an existing table, so column additions live
// here; the embed_source check wipes vectors computed under a different
// embedding scheme (they rebuild lazily on the next write per keypath).
func migrate(db *sql.DB) error {
	for _, col := range []string{"category", "topics"} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('memories') WHERE name=?`, col,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE memories ADD COLUMN ` + col + ` TEXT`); err != nil {
				return err
			}
		}
	}
	var src string
	err := db.QueryRow(`SELECT value FROM meta WHERE key='embed_source'`).Scan(&src)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if src != embedSource {
		if _, err := db.Exec(`DELETE FROM keypath_embeddings`); err != nil {
			return err
		}
		if _, err := db.Exec(
			`INSERT INTO meta(key, value) VALUES('embed_source', ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, embedSource,
		); err != nil {
			return err
		}
	}
	return nil
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

// ensureProject creates the project row, or revives it if soft-deleted.
// Any write to a project therefore un-deletes it.
func ensureProject(exec dbExec, id string) error {
	_, err := exec.Exec(
		`INSERT INTO projects(id, created_at) VALUES(?, ?)
		 ON CONFLICT(id) DO UPDATE SET deleted_at = NULL`,
		id, time.Now().Unix(),
	)
	return err
}

func scanMemory(scan func(dest ...any) error) (*Memory, error) {
	m := &Memory{}
	var src, cat, topics sql.NullString
	var parent sql.NullInt64
	var tomb int
	if err := scan(&m.ID, &m.ProjectID, &m.Keypath, &m.Content, &src, &cat, &topics,
		&m.Version, &parent, &tomb, &m.CreatedAt); err != nil {
		return nil, err
	}
	if src.Valid {
		m.Source = src.String
	}
	if cat.Valid {
		m.Category = cat.String
	}
	if topics.Valid {
		if err := json.Unmarshal([]byte(topics.String), &m.Topics); err != nil {
			return nil, fmt.Errorf("decode topics for memory %d: %w", m.ID, err)
		}
	}
	if parent.Valid {
		m.ParentID = &parent.Int64
	}
	m.Tombstone = tomb != 0
	return m, nil
}

// memoryCols is always qualified so queries can safely join FTS5 (which also
// has a keypath column) without provoking "ambiguous column" errors.
const memoryCols = `m.id, m.project_id, m.keypath, m.content, m.source, m.category, m.topics, m.version, m.parent_id, m.tombstone, m.created_at`

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
// superseded previous (nil if none). The whole read-latest + insert runs in
// one transaction so concurrent writers to the same keypath cannot collide
// on the (project_id, keypath, version) unique index.
func (s *Store) Write(projectID, keypath, content string, meta WriteMeta, tombstone bool) (*Memory, *Memory, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := ensureProject(tx, projectID); err != nil {
		return nil, nil, err
	}
	stored, prev, err := writeExec(tx, projectID, keypath, content, meta, tombstone)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	committed = true
	return stored, prev, nil
}

// writeExec is the tx-capable core of Write. Caller is responsible for
// ensureProject; within a batch tx we hoist that call outside the loop.
//
// Idempotency: if the prior live version has identical content and metadata
// (and this call is not a tombstone), no new row is written. Both return
// values point at the same prior memory — callers detect "unchanged" via
// pointer or ID equality.
func writeExec(exec dbExec, projectID, keypath, content string, meta WriteMeta, tombstone bool) (*Memory, *Memory, error) {
	prev, err := getLatestExec(exec, projectID, keypath)
	if err != nil {
		return nil, nil, err
	}
	if !tombstone && prev != nil && !prev.Tombstone &&
		prev.Content == content && prev.Category == meta.Category &&
		slices.Equal(prev.Topics, meta.Topics) {
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
	var srcArg, catArg, topicsArg any
	if meta.Source != "" {
		srcArg = meta.Source
	}
	if meta.Category != "" {
		catArg = meta.Category
	}
	if len(meta.Topics) > 0 {
		b, err := json.Marshal(meta.Topics)
		if err != nil {
			return nil, nil, err
		}
		topicsArg = string(b)
	}
	res, err := exec.Exec(
		`INSERT INTO memories(project_id, keypath, content, source, category, topics, version, parent_id, tombstone, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, keypath, content, srcArg, catArg, topicsArg, nextVer, parent, tombVal, now,
	)
	if err != nil {
		return nil, nil, err
	}
	id, _ := res.LastInsertId()
	// Only the current version is searchable: drop the superseded version's
	// FTS row so the index doesn't grow with (and rank against) dead text.
	if prev != nil && !prev.Tombstone {
		if _, err := exec.Exec(
			`DELETE FROM memories_fts WHERE rowid=?`, prev.ID,
		); err != nil {
			return nil, nil, err
		}
	}
	if tombstone {
		// A dead keypath must also stop participating in semantic search.
		if _, err := exec.Exec(
			`DELETE FROM keypath_embeddings WHERE project_id=? AND keypath=?`,
			projectID, keypath,
		); err != nil {
			return nil, nil, err
		}
	} else {
		if _, err := exec.Exec(
			`INSERT INTO memories_fts(rowid, content, keypath) VALUES(?, ?, ?)`,
			id, content, keypath,
		); err != nil {
			return nil, nil, err
		}
	}
	m := &Memory{
		ID: id, ProjectID: projectID, Keypath: keypath, Content: content,
		Source: meta.Source, Category: meta.Category, Topics: meta.Topics,
		Version: nextVer, Tombstone: tombstone, CreatedAt: now,
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

// MissingEmbedding is a current, non-tombstoned memory in a live project
// that has no embedding row for the given model. Input to the startup
// backfill.
type MissingEmbedding struct {
	ProjectID string
	Keypath   string
	Content   string
}

// ListMissingEmbeddings returns every current, non-tombstoned keypath in a
// live project whose (project, keypath, model) embedding row is absent.
func (s *Store) ListMissingEmbeddings(model string) ([]MissingEmbedding, error) {
	rows, err := s.db.Query(`
		SELECT m.project_id, m.keypath, m.content FROM memories m
		JOIN projects p ON p.id = m.project_id AND p.deleted_at IS NULL
		WHERE m.tombstone = 0
		  AND m.version = (
		    SELECT MAX(version) FROM memories m2
		    WHERE m2.project_id = m.project_id AND m2.keypath = m.keypath
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM keypath_embeddings ke
		    WHERE ke.project_id = m.project_id
		      AND ke.keypath = m.keypath
		      AND ke.model = ?
		  )
		ORDER BY m.project_id, m.keypath`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingEmbedding
	for rows.Next() {
		var me MissingEmbedding
		if err := rows.Scan(&me.ProjectID, &me.Keypath, &me.Content); err != nil {
			return nil, err
		}
		out = append(out, me)
	}
	return out, rows.Err()
}

// KeypathEmbedding is a row from keypath_embeddings, exposed to the search
// path after the vector is unpacked.
type KeypathEmbedding struct {
	ProjectID string
	Keypath   string
	Vector    []float32
}

// ListKeypathEmbeddings returns every (project, keypath, vector) row for a
// given model, optionally scoped to a single project. Rows from soft-deleted
// projects are excluded. Vectors are unpacked from BLOB so the caller can
// compute cosine in memory.
func (s *Store) ListKeypathEmbeddings(projectID, model string) ([]*KeypathEmbedding, error) {
	q := `SELECT ke.project_id, ke.keypath, ke.vector FROM keypath_embeddings ke
	      JOIN projects p ON p.id = ke.project_id AND p.deleted_at IS NULL
	      WHERE ke.model=?`
	args := []any{model}
	if projectID != "" {
		q += ` AND ke.project_id=?`
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

// SemanticSearch ranks keypaths in the given project (or all live projects
// if projectID is empty) by cosine similarity to query. Returns at most
// limit hits whose similarity is >= threshold and whose current memory
// passes the filter, paired with that memory.
//
// Callers that want to skip the embedding lookup (e.g. because they already
// have a vector) can pass the pre-embedded query directly.
func (s *Store) SemanticSearch(projectID string, query []float32, model string, threshold float32, filter SearchFilter, limit int) ([]*SemanticHit, error) {
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
	sort.Slice(picks, func(i, j int) bool { return picks[i].score > picks[j].score })
	// Truncate to limit AFTER the tombstone/filter checks below, so filtered
	// rows don't consume result slots.
	out := make([]*SemanticHit, 0, min(len(picks), limit))
	for _, p := range picks {
		if len(out) == limit {
			break
		}
		m, err := s.GetLatest(p.pid, p.kp)
		if err != nil {
			return nil, err
		}
		if m == nil || m.Tombstone || !filter.Matches(m) {
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
// Meta applies to every row. Sections are processed in order, so a later
// section can observe a prior section's write via parent_id chain.
func (s *Store) WriteBatch(projectID string, sections []Section, meta WriteMeta) ([]BatchItem, error) {
	if len(sections) == 0 {
		return nil, nil
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
	if err := ensureProject(tx, projectID); err != nil {
		return nil, err
	}
	out := make([]BatchItem, 0, len(sections))
	for _, sec := range sections {
		kp := NormalizeKeypath(sec.Keypath)
		stored, prev, err := writeExec(tx, projectID, kp, sec.Content, meta, false)
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

// escapeLike backslash-escapes LIKE wildcards. Snake_case keypaths contain
// `_` (the single-char wildcard), so unescaped prefixes silently over-match.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
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
		q += ` AND (m.keypath = ? OR m.keypath LIKE ? ESCAPE '\')`
		args = append(args, keypath, escapeLike(keypath)+".%")
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

// SearchFilter narrows search results by per-version metadata and keypath
// location. Zero value means no filtering. Topics is match-any: a memory
// qualifies if it carries at least one of the listed topics. KeypathPrefix
// restricts hits to that exact keypath or its descendants (dot-boundary,
// e.g. "branches.feature_x" matches "branches.feature_x.todo" but not
// "branches.feature_x2").
type SearchFilter struct {
	Category      string
	Topics        []string
	KeypathPrefix string
}

// Matches reports whether a memory passes the filter. Used by the semantic
// path, which filters in Go; the FTS path expresses the same predicate in SQL.
func (f SearchFilter) Matches(m *Memory) bool {
	if f.Category != "" && m.Category != f.Category {
		return false
	}
	if len(f.Topics) > 0 && !slices.ContainsFunc(f.Topics, func(t string) bool {
		return slices.Contains(m.Topics, t)
	}) {
		return false
	}
	if f.KeypathPrefix != "" && m.Keypath != f.KeypathPrefix &&
		!strings.HasPrefix(m.Keypath, f.KeypathPrefix+".") {
		return false
	}
	return true
}

// ftsQuote turns free text into a safe FTS5 query: each whitespace-separated
// token becomes a quoted string (implicit AND). Punctuation like apostrophes
// or hyphens would otherwise be parsed as FTS5 operator syntax and error out.
func ftsQuote(q string) string {
	fields := strings.Fields(q)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " ")
}

// Search does an FTS5 match on content+keypath, restricted to current
// non-tombstoned versions in live (non-deleted) projects.
func (s *Store) Search(projectID, query string, filter SearchFilter, limit int) ([]*Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `
		SELECT ` + memoryCols + ` FROM memories m
		JOIN memories_fts fts ON fts.rowid = m.id
		JOIN projects p ON p.id = m.project_id AND p.deleted_at IS NULL
		WHERE memories_fts MATCH ?
		  AND m.tombstone = 0
		  AND m.version = (
		    SELECT MAX(version) FROM memories m2
		    WHERE m2.project_id = m.project_id AND m2.keypath = m.keypath
		  )
	`
	args := []any{ftsQuote(query)}
	if projectID != "" {
		q += ` AND m.project_id = ?`
		args = append(args, projectID)
	}
	if filter.Category != "" {
		q += ` AND m.category = ?`
		args = append(args, filter.Category)
	}
	if filter.KeypathPrefix != "" {
		q += ` AND (m.keypath = ? OR m.keypath LIKE ? ESCAPE '\')`
		args = append(args, filter.KeypathPrefix, escapeLike(filter.KeypathPrefix)+".%")
	}
	if len(filter.Topics) > 0 {
		q += ` AND m.topics IS NOT NULL AND EXISTS (
			SELECT 1 FROM json_each(m.topics)
			WHERE json_each.value IN (?` + strings.Repeat(",?", len(filter.Topics)-1) + `))`
		for _, t := range filter.Topics {
			args = append(args, t)
		}
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
	return s.Write(projectID, keypath, "", WriteMeta{}, true)
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
		if _, _, err := s.Write(projectID, m.Keypath, "", WriteMeta{}, true); err != nil {
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
