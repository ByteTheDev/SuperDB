package engine

import (
	"errors"
	"fmt"
	"strings"
)

func (d *Database) selectRows(s string) (Result, error) {
	u := strings.ToUpper(s)
	fi := strings.Index(u, "FROM")
	if fi < 0 {
		return Result{}, errors.New("SELECT requires FROM")
	}
	rest := strings.TrimSpace(s[fi+4:])
	parts := strings.Fields(rest)
	if len(parts) < 1 {
		return Result{}, errors.New("table required")
	}
	t, e := d.table(parts[0])
	if e != nil {
		return Result{}, e
	}
	names := []string{}
	sel := strings.TrimSpace(s[6:fi])
	if sel == "*" {
		for _, c := range t.Columns {
			names = append(names, c.Name)
		}
	} else {
		for _, x := range strings.Split(sel, ",") {
			names = append(names, strings.ToLower(strings.TrimSpace(x)))
		}
	}
	whereCol, whereVal := filter(rest)
	t.mu.RLock()
	defer t.mu.RUnlock()
	r := Result{Columns: names}
	if whereCol != "" && whereCol == t.primaryColumn() {
		if row, ok := t.Rows[whereVal]; ok {
			r.Rows = append(r.Rows, selectedValues(row, names))
		}
		return r, nil
	}
	for _, k := range t.Order {
		row := t.Rows[k]
		if whereCol != "" && fmt.Sprint(row[whereCol]) != whereVal {
			continue
		}
		r.Rows = append(r.Rows, selectedValues(row, names))
	}
	return r, nil
}

func selectedValues(row map[string]any, names []string) []any {
	line := make([]any, 0, len(names))
	for _, n := range names {
		line = append(line, row[n])
	}
	return line
}
