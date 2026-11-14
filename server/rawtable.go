package main

import (
	"database/sql"
	"fmt"
)

// Raw table reader for `memstate ui`. It shows the SQLite rows as they are:
// each version, each tombstone, each deleted project. It only reads.

// rawNull is the text of a NULL cell.
const rawNull = "NULL"

// rawTable is one table that the reader can show. The table name and the
// SQL text come only from rawTables, never from input.
type rawTable struct {
	name    string
	cols    []string // column titles, one per expression in sel
	sel     string   // SELECT list
	orderBy string   // a unique key, so that pages do not overlap
}

var rawTables = []rawTable{
	{
		name:    "projects",
		cols:    []string{"id", "created_at", "deleted_at"},
		sel:     "id, created_at, deleted_at",
		orderBy: "id",
	},
	{
		name: "memories",
		cols: []string{"id", "project_id", "keypath", "content", "source", "category",
			"topics", "version", "parent_id", "tombstone", "created_at"},
		sel: "id, project_id, keypath, content, source, category, " +
			"topics, version, parent_id, tombstone, created_at",
		orderBy: "id",
	},
	{
		name:    "memories_fts",
		cols:    []string{"rowid", "project_id", "keypath", "content"},
		sel:     "rowid, project_id, keypath, content",
		orderBy: "rowid",
	},
	{
		// A vector is thousands of bytes of floats. The cell shows its size.
		name:    "keypath_embeddings",
		cols:    []string{"project_id", "keypath", "model", "dim", "vector", "created_at"},
		sel:     "project_id, keypath, model, dim, '<' || length(vector) || ' bytes>', created_at",
		orderBy: "project_id, keypath, model",
	},
	{
		name:    "meta",
		cols:    []string{"key", "value"},
		sel:     "key, value",
		orderBy: "key",
	},
	{
		name:    "purged",
		cols:    []string{"project_id", "keypath", "purged_at"},
		sel:     "project_id, keypath, purged_at",
		orderBy: "project_id, keypath",
	},
}

func findRawTable(name string) (rawTable, bool) {
	for _, t := range rawTables {
		if t.name == name {
			return t, true
		}
	}
	return rawTable{}, false
}

// RawTableCount is a table name with its number of rows.
type RawTableCount struct {
	Name  string
	Count int
}

// RawTableCounts returns the row count of each table in rawTables, in that
// order.
func (s *Store) RawTableCounts() ([]RawTableCount, error) {
	out := make([]RawTableCount, 0, len(rawTables))
	for _, t := range rawTables {
		c := RawTableCount{Name: t.name}
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + t.name).Scan(&c.Count); err != nil {
			return nil, fmt.Errorf("count %s: %w", t.name, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// RawRows returns one page of a table as text cells, in key order. A NULL
// cell is rawNull.
func (s *Store) RawRows(table string, offset, limit int) ([]string, [][]string, error) {
	t, ok := findRawTable(table)
	if !ok {
		return nil, nil, fmt.Errorf("unknown table %q", table)
	}
	rows, err := s.db.Query(
		`SELECT `+t.sel+` FROM `+t.name+` ORDER BY `+t.orderBy+` LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		cells := make([]sql.NullString, len(t.cols))
		dest := make([]any, len(cells))
		for i := range cells {
			dest[i] = &cells[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(cells))
		for i, c := range cells {
			if c.Valid {
				row[i] = c.String
			} else {
				row[i] = rawNull
			}
		}
		out = append(out, row)
	}
	return t.cols, out, rows.Err()
}
