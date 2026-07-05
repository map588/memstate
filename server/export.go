package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Export/import is deliberately CLI-only (memstated export | import): a
// human workflow for moving memory between machines. There is no HTTP route
// and no MCP tool for it, and there should not be — the model has no
// business deciding when to export or import.

// Export file format. FormatVersion guards imports: a file written by a
// future schema declares a higher number and is rejected instead of being
// half-understood.
const (
	exportFormat        = "memstate-export"
	exportFormatVersion = 1
)

var ErrProjectNotFound = errors.New("project not found")

// ExportMemory is one version row in an export file. Database-local fields
// (id, parent_id) are omitted: they are meaningless in another database and
// are rebuilt from (keypath, version) order on import.
type ExportMemory struct {
	Keypath   string   `json:"keypath"`
	Content   string   `json:"content"`
	Source    string   `json:"source,omitempty"`
	Category  string   `json:"category,omitempty"`
	Topics    []string `json:"topics,omitempty"`
	Version   int      `json:"version"`
	Tombstone bool     `json:"tombstone,omitempty"`
	CreatedAt int64    `json:"created_at"`
}

// ProjectExport is one project's complete history: every version of every
// keypath, tombstones included.
type ProjectExport struct {
	ProjectID string         `json:"project_id"`
	Memories  []ExportMemory `json:"memories"`
}

// ExportData is the whole export file. Embeddings are NOT exported — they
// rebuild from content on the importing side (import runs a synchronous
// backfill; the daemon's startup backfill is the safety net).
type ExportData struct {
	Format        string          `json:"format"`
	FormatVersion int             `json:"format_version"`
	ExportedAt    int64           `json:"exported_at"`
	Projects      []ProjectExport `json:"projects"`
}

// Export snapshots the full history of the named projects. Every id must
// name an existing, live (non-soft-deleted) project.
func (s *Store) Export(projectIDs []string) (*ExportData, error) {
	out := &ExportData{
		Format:        exportFormat,
		FormatVersion: exportFormatVersion,
		ExportedAt:    time.Now().Unix(),
	}
	for _, pid := range projectIDs {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM projects WHERE id=? AND deleted_at IS NULL`, pid,
		).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("project %q: %w", pid, ErrProjectNotFound)
		}
		all, err := s.queryMemories(
			`SELECT `+memoryCols+` FROM memories m
			 WHERE m.project_id=?
			 ORDER BY m.keypath, m.version`,
			pid,
		)
		if err != nil {
			return nil, err
		}
		pe := ProjectExport{ProjectID: pid, Memories: make([]ExportMemory, len(all))}
		for i, m := range all {
			pe.Memories[i] = ExportMemory{
				Keypath:   m.Keypath,
				Content:   m.Content,
				Source:    m.Source,
				Category:  m.Category,
				Topics:    m.Topics,
				Version:   m.Version,
				Tombstone: m.Tombstone,
				CreatedAt: m.CreatedAt,
			}
		}
		out.Projects = append(out.Projects, pe)
	}
	return out, nil
}

// MergeStats reports what Merge did to one project, in keypaths.
type MergeStats struct {
	ProjectID    string
	Restored     int // absent locally — full version chain copied from the file
	Updated      int // source latest was newer — written as a new local version
	Deleted      int // source latest was a newer tombstone — local keypath tombstoned
	Unchanged    int // source newer but identical state — nothing written
	SkippedOlder int // local latest is at least as new — local wins
}

// Merge folds an export file into the local store, project by project.
// overrideProjectID retargets the import and is only valid for a
// single-project file.
//
// Per keypath, against the local latest version:
//   - keypath absent locally → the file's full version chain is inserted
//     verbatim (versions, timestamps, and parent links preserved);
//   - the file's latest is strictly newer (created_at) → it is replayed
//     through the normal write path, so it supersedes, tombstones, or
//     dedupes to unchanged exactly as if it had been written by hand;
//   - otherwise the local version wins and the keypath is skipped.
//
// Ties keep local, so cross-machine clocks only need to be NTP-close.
// Re-importing the same file is a no-op, which makes A→B→A ping-pong safe.
// Each project entry is one transaction — a failure rolls back that project
// entirely. Any merge into a soft-deleted local project revives it, like
// every other write. Vectors for updated keypaths are dropped so the
// caller's embedding backfill recomputes them.
func (s *Store) Merge(data *ExportData, overrideProjectID string) ([]MergeStats, error) {
	if data.Format != exportFormat {
		return nil, fmt.Errorf("not a memstate export (format %q)", data.Format)
	}
	if data.FormatVersion != exportFormatVersion {
		return nil, fmt.Errorf("unsupported export format_version %d (this build reads %d)",
			data.FormatVersion, exportFormatVersion)
	}
	if overrideProjectID != "" && len(data.Projects) != 1 {
		return nil, fmt.Errorf("--project rename needs a single-project file; this one has %d projects",
			len(data.Projects))
	}
	var stats []MergeStats
	for _, pe := range data.Projects {
		target := pe.ProjectID
		if overrideProjectID != "" {
			target = overrideProjectID
		}
		if target == "" {
			return nil, errors.New("export entry has no project_id and none was given")
		}
		st, err := s.mergeProject(target, pe.Memories)
		if err != nil {
			return stats, fmt.Errorf("project %q: %w", target, err)
		}
		stats = append(stats, *st)
	}
	return stats, nil
}

func (s *Store) mergeProject(target string, memories []ExportMemory) (*MergeStats, error) {
	for i, m := range memories {
		if m.Keypath == "" {
			return nil, fmt.Errorf("memory %d: empty keypath", i)
		}
		if m.Version < 1 {
			return nil, fmt.Errorf("memory %d (%s): version %d < 1", i, m.Keypath, m.Version)
		}
	}
	mems := make([]ExportMemory, len(memories))
	copy(mems, memories)
	sort.Slice(mems, func(i, j int) bool {
		if mems[i].Keypath != mems[j].Keypath {
			return mems[i].Keypath < mems[j].Keypath
		}
		return mems[i].Version < mems[j].Version
	})

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
	if err := ensureProject(tx, target); err != nil {
		return nil, err
	}
	st := &MergeStats{ProjectID: target}
	for i := 0; i < len(mems); {
		j := i
		for j < len(mems) && mems[j].Keypath == mems[i].Keypath {
			j++
		}
		if err := mergeKeypath(tx, target, mems[i:j], st); err != nil {
			return nil, fmt.Errorf("keypath %s: %w", mems[i].Keypath, err)
		}
		i = j
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return st, nil
}

// mergeKeypath applies one keypath's exported version chain (sorted by
// version ascending) against the local store, inside the caller's tx.
func mergeKeypath(tx dbExec, target string, chain []ExportMemory, st *MergeStats) error {
	kp := chain[0].Keypath
	srcLatest := chain[len(chain)-1]
	local, err := getLatestExec(tx, target, kp)
	if err != nil {
		return err
	}

	if local == nil {
		// Fresh keypath: copy the full chain verbatim, parent links rebuilt.
		var prevID int64
		for i, m := range chain {
			var srcArg, catArg, topicsArg, parentArg any
			if m.Source != "" {
				srcArg = m.Source
			}
			if m.Category != "" {
				catArg = m.Category
			}
			if len(m.Topics) > 0 {
				b, err := json.Marshal(m.Topics)
				if err != nil {
					return err
				}
				topicsArg = string(b)
			}
			if i > 0 {
				parentArg = prevID
			}
			tombVal := 0
			if m.Tombstone {
				tombVal = 1
			}
			r, err := tx.Exec(
				`INSERT INTO memories(project_id, keypath, content, source, category, topics, version, parent_id, tombstone, created_at)
				 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				target, m.Keypath, m.Content, srcArg, catArg, topicsArg,
				m.Version, parentArg, tombVal, m.CreatedAt,
			)
			if err != nil {
				return fmt.Errorf("insert v%d: %w", m.Version, err)
			}
			if prevID, err = r.LastInsertId(); err != nil {
				return err
			}
		}
		if !srcLatest.Tombstone {
			if _, err := tx.Exec(
				`INSERT INTO memories_fts(rowid, content, keypath) VALUES(?, ?, ?)`,
				prevID, srcLatest.Content, kp,
			); err != nil {
				return err
			}
		}
		st.Restored++
		return nil
	}

	if srcLatest.CreatedAt <= local.CreatedAt {
		st.SkippedOlder++
		return nil
	}

	if srcLatest.Tombstone {
		if local.Tombstone {
			st.Unchanged++
			return nil
		}
		if _, _, err := writeExec(tx, target, kp, "", WriteMeta{}, true); err != nil {
			return err
		}
		st.Deleted++
		return nil
	}

	meta := WriteMeta{Source: srcLatest.Source, Category: srcLatest.Category, Topics: srcLatest.Topics}
	stored, prev, err := writeExec(tx, target, kp, srcLatest.Content, meta, false)
	if err != nil {
		return err
	}
	if prev != nil && stored.ID == prev.ID {
		st.Unchanged++
		return nil
	}
	// writeExec leaves live-write embeddings to the daemon's write path,
	// which isn't running here. Drop the stale vector so the post-import
	// backfill (which only fills missing rows) recomputes it.
	if _, err := tx.Exec(
		`DELETE FROM keypath_embeddings WHERE project_id=? AND keypath=?`,
		target, kp,
	); err != nil {
		return err
	}
	st.Updated++
	return nil
}

// safeFilename keeps a project id usable as a filename component. Ids are
// conventionally snake_case already; this is belt-and-suspenders for the
// ones that aren't.
func safeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
}
