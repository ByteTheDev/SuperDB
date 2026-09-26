package engine

import (
	"context"
	"errors"
	"strings"
)

func (d *Database) insert(ctx context.Context, s string) (Result, error) {
	vi := indexKeyword(s, "VALUES")
	if vi < 0 {
		return Result{}, errors.New("INSERT requires VALUES")
	}
	// Only the short table-name token is scanned; the VALUES payload can be
	// large and must not be split just to find the destination table.
	tableName, ok := firstField(s[len("INSERT INTO"):vi])
	if !ok {
		return Result{}, errors.New("invalid INSERT")
	}
	t, e := d.table(tableName)
	if e != nil {
		return Result{}, e
	}
	// The VALUES parser is the hottest part of bulk INSERT. Keep the
	// established parser available for parity tests and use the single-pass
	// implementation on the production path.
	t.mu.Lock()
	defer t.mu.Unlock()
	rows, err := parseInsertRowsFast(s[vi+6:], t.Columns)
	if err != nil {
		return Result{}, err
	}
	tick := &rowTicker{}
	for _, item := range rows {
		if err := tick.tick(ctx); err != nil {
			return Result{}, err
		}
		if item.key != "" {
			if _, ok := t.Rows[item.key]; ok {
				return Result{}, errors.New("duplicate primary key")
			}
		}
	}
	if t.dead > 0 {
		// Reclaim tombstones before appending so a reused primary key appears
		// only once in Order. Do this once per INSERT, not once per row.
		live := t.Order[:0]
		for _, key := range t.Order {
			if _, exists := t.Rows[key]; exists {
				live = append(live, key)
			}
		}
		for i := len(live); i < len(t.Order); i++ {
			t.Order[i] = ""
		}
		t.Order = live
		t.dead = 0
		if len(t.fast) > 0 {
			t.rebuildFastPathLocked()
		}
	}
	for i := range rows {
		if rows[i].key == "" {
			// Tables without a primary key use synthetic row keys; "#" can
			// never collide with a typed key produced by valueKey.
			rows[i].key = t.nextSyntheticKeyLocked()
		}
		t.Rows[rows[i].key] = rows[i].row
		t.Order = append(t.Order, rows[i].key)
		for column := range t.Indexes {
			t.addIndexValue(column, valueKey(rows[i].row[column]), rows[i].key)
		}
		t.appendFastRowLocked(rows[i].key, rows[i].row)
	}
	return Result{Affected: len(rows)}, nil
}

type insertRow struct {
	key string
	row map[string]any
}

func parseInsertRows(raw string, columns []Column) ([]insertRow, error) {
	groups := splitRows(strings.TrimSpace(raw))
	if len(groups) == 0 {
		return nil, errors.New("INSERT requires VALUES")
	}
	rows := make([]insertRow, 0, len(groups))
	seen := make(map[string]struct{}, len(groups))
	hasPrimary := hasPrimaryColumn(columns)
	for _, group := range groups {
		vals := splitVals(group)
		if len(vals) != len(columns) {
			return nil, errors.New("column count mismatch")
		}
		row := make(map[string]any, len(columns))
		key := ""
		for i, c := range columns {
			v, err := parseVal(vals[i], c.Type)
			if err != nil {
				return nil, err
			}
			row[c.Name] = v
			if c.Primary {
				key = valueKey(v)
			}
		}
		if key == "" && hasPrimary {
			return nil, errors.New("primary key required")
		}
		if key != "" {
			if _, dup := seen[key]; dup {
				return nil, errors.New("duplicate primary key")
			}
			seen[key] = struct{}{}
		}
		rows = append(rows, insertRow{key: key, row: row})
	}
	return rows, nil
}

func splitRows(s string) []string {
	var out []string
	start, depth := -1, 0
	quote := false
	for i := 0; i < len(s); i++ {
		r := s[i]
		if r == '\'' {
			if ni, skip := skipQuoted(s, i, quote); skip {
				i = ni
				continue
			}
			quote = !quote
			continue
		}
		if quote {
			continue
		}
		switch r {
		case '(':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case ')':
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
			}
		}
	}
	return out
}
