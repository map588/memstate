package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Purge erases a keypath: each version, the search row and the vectors.
// It is the opposite of the rest of the store, which only appends, and it
// is for a note that must not be part of the collection at all. A delete
// (tombstone) stays the normal way to retire a fact.
//
// Purge is a human action in `memstate ui`, direct on the SQLite file like
// export, import and `embed prune`. There is no HTTP route and no MCP tool
// for it: a program that can reach the daemon must not be able to erase
// history.
//
// Each purged keypath gets a row in the purged table. Import reads it
// (mergeKeypath), so a file from a machine that still has the note does
// not bring the note back.

// PurgedKeypath is one keypath of a purge, with its number of versions.
type PurgedKeypath struct {
	Keypath  string
	Versions int
}

// purgeWhere is the row filter of a purge, for the tables that have the
// columns project_id and keypath. recursive adds each keypath below.
//
// The match is exact. A purge has no undo, so it must not take a keypath
// that only looks like the target; keypathScope gives the reason.
func purgeWhere(projectID, keypath string, recursive bool) (string, []any) {
	if !recursive {
		return `project_id = ? AND keypath = ?`, []any{projectID, keypath}
	}
	scope, args := keypathScope("keypath", keypath)
	return `project_id = ? AND ` + scope, append([]any{projectID}, args...)
}

type rowQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func purgeList(q rowQuerier, projectID, keypath string, recursive bool) ([]PurgedKeypath, error) {
	where, args := purgeWhere(projectID, keypath, recursive)
	rows, err := q.Query(`SELECT keypath, COUNT(*) FROM memories WHERE `+where+` GROUP BY keypath ORDER BY keypath`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PurgedKeypath
	for rows.Next() {
		var p PurgedKeypath
		if err := rows.Scan(&p.Keypath, &p.Versions); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PurgePreview lists what Purge erases with the same arguments: live
// keypaths and tombstoned ones. It changes nothing.
//
// Preview and Purge take the keypath as it is stored and do not normalize
// it. An import can leave a tombstoned chain with a name that a write
// changes ("x/y"); its purge must not erase the keypath "x.y".
func (s *Store) PurgePreview(projectID, keypath string, recursive bool) ([]PurgedKeypath, error) {
	if keypath == "" {
		return nil, errors.New("purge needs a keypath")
	}
	return purgeList(s.db, projectID, keypath, recursive)
}

// Purge erases each version at keypath, and with recursive at each keypath
// below it, in one transaction. It returns what it erased. Nothing to
// erase is an error.
func (s *Store) Purge(projectID, keypath string, recursive bool) ([]PurgedKeypath, error) {
	if keypath == "" {
		return nil, errors.New("purge needs a keypath")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	items, err := purgeList(tx, projectID, keypath, recursive)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("nothing to purge in project %q at %q", projectID, keypath)
	}
	where, args := purgeWhere(projectID, keypath, recursive)
	// One DELETE takes a whole version chain, so the parent_id references
	// inside the chain hold at the end of the statement.
	for _, stmt := range []string{
		`DELETE FROM memories_fts WHERE rowid IN (SELECT id FROM memories WHERE ` + where + `)`,
		`DELETE FROM keypath_embeddings WHERE ` + where,
		`DELETE FROM memories WHERE ` + where,
	} {
		if _, err := tx.Exec(stmt, args...); err != nil {
			return nil, err
		}
	}
	now := time.Now().Unix()
	for _, it := range items {
		if _, err := tx.Exec(
			`INSERT INTO purged(project_id, keypath, purged_at) VALUES(?, ?, ?)
			 ON CONFLICT(project_id, keypath) DO UPDATE SET purged_at = excluded.purged_at`,
			projectID, it.Keypath, now,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

// purgedAt returns the time of the purge of a keypath. ok is false when
// the keypath was not purged.
func purgedAt(q dbExec, projectID, keypath string) (at int64, ok bool, err error) {
	err = q.QueryRow(`SELECT purged_at FROM purged WHERE project_id = ? AND keypath = ?`,
		projectID, keypath).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return at, err == nil, err
}
