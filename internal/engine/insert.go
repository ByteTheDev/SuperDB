package engine

import (
	"errors"
	"strings"
)

func (d *Database) insert(s string) (Result, error) {
	u := strings.ToUpper(s)
	vi := strings.Index(u, "VALUES")
	if vi < 0 {
		return Result{}, errors.New("INSERT requires VALUES")
	}
	h := strings.Fields(s[:vi])
	if len(h) < 3 {
		return Result{}, errors.New("invalid INSERT")
	}
	t, e := d.table(h[2])
	if e != nil {
		return Result{}, e
	}
	rows, err := parseInsertRows(strings.TrimSpace(s[vi+6:]), t.Columns)
	if err != nil {
		return Result{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, item := range rows {
		if _, ok := t.Rows[item.key]; ok {
			return Result{}, errors.New("duplicate primary key")
		}
	}
	for _, item := range rows {
		t.Rows[item.key] = item.row
		t.Order = append(t.Order, item.key)
		for column := range t.Indexes {
			t.addIndexValue(column, valueKey(item.row[column]), item.key)
		}
		t.appendFastRowLocked(item.key, item.row)
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
		if key == "" {
			return nil, errors.New("primary key required")
		}
		for _, previous := range rows {
			if previous.key == key {
				return nil, errors.New("duplicate primary key")
			}
		}
		rows = append(rows, insertRow{key: key, row: row})
	}
	return rows, nil
}

func splitRows(s string) []string {
	var out []string
	start, depth := -1, 0
	quote := false
	for i, r := range s {
		if r == '\'' {
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
