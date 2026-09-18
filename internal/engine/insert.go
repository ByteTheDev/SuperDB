package engine

import (
	"errors"
	"fmt"
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
	raw := strings.TrimSpace(s[vi+6:])
	raw = strings.Trim(raw, "() ")
	vals := splitVals(raw)
	if len(vals) != len(t.Columns) {
		return Result{}, errors.New("column count mismatch")
	}
	row := map[string]any{}
	key := ""
	for i, c := range t.Columns {
		v, e := parseVal(vals[i], c.Type)
		if e != nil {
			return Result{}, e
		}
		row[c.Name] = v
		if c.Primary {
			key = fmt.Sprint(v)
		}
	}
	if key == "" {
		return Result{}, errors.New("primary key required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.Rows[key]; ok {
		return Result{}, errors.New("duplicate primary key")
	}
	t.Rows[key] = row
	t.Order = append(t.Order, key)
	return Result{Affected: 1}, nil
}
