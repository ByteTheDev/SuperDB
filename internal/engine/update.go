package engine

import (
	"errors"
	"fmt"
	"strings"
)

func (d *Database) update(s string) (Result, error) {
	u := strings.ToUpper(s)
	si := strings.Index(u, "SET")
	if si < 0 {
		return Result{}, errors.New("UPDATE requires SET")
	}
	head := strings.Fields(s[:si])
	if len(head) < 2 {
		return Result{}, errors.New("table required")
	}
	t, e := d.table(head[1])
	if e != nil {
		return Result{}, e
	}
	tail := s[si+3:]
	wi := strings.Index(strings.ToUpper(tail), "WHERE")
	assign := strings.TrimSpace(tail)
	where := ""
	if wi >= 0 {
		assign = strings.TrimSpace(tail[:wi])
		where = strings.TrimSpace(tail[wi+5:])
	}
	ap := strings.SplitN(assign, "=", 2)
	if len(ap) != 2 {
		return Result{}, errors.New("invalid SET")
	}
	col := strings.ToLower(strings.TrimSpace(ap[0]))
	var typ Type
	for _, c := range t.Columns {
		if c.Name == col {
			typ = c.Type
		}
	}
	v, e := parseVal(ap[1], typ)
	if e != nil {
		return Result{}, e
	}
	wc, wv := "", ""
	if where != "" {
		p := strings.SplitN(where, "=", 2)
		if len(p) == 2 {
			wc = strings.ToLower(strings.TrimSpace(p[0]))
			wv = strings.Trim(strings.TrimSpace(p[1]), "'")
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if wc != "" && wc == t.primaryColumn() {
		if row, ok := t.Rows[wv]; ok {
			row[col] = v
			return Result{Affected: 1}, nil
		}
		return Result{}, nil
	}
	n := 0
	for _, row := range t.Rows {
		if wc != "" && fmt.Sprint(row[wc]) != wv {
			continue
		}
		row[col] = v
		n++
	}
	return Result{Affected: n}, nil
}
