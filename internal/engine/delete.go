package engine

import (
	"context"
	"errors"
	"strings"
)

func (d *Database) delete(ctx context.Context, s string) (Result, error) {
	rest := strings.TrimSpace(s[len("DELETE FROM"):])
	tableName, ok := firstField(rest)
	if !ok {
		return Result{}, errors.New("invalid DELETE")
	}
	t, e := d.table(tableName)
	if e != nil {
		return Result{}, e
	}
	c, v, hasWhere, err := parseMutationWhere(s)
	if err != nil {
		return Result{}, err
	}
	_ = hasWhere
	t.mu.Lock()
	defer t.mu.Unlock()
	if c != "" && c == t.primaryColumn() {
		if _, ok := t.Rows[v]; ok {
			for column := range t.Indexes {
				t.removeIndexValue(column, valueKey(t.Rows[v][column]), v)
			}
			delete(t.Rows, v)
			t.removeFromOrderLocked(v)
			t.removeFastRowLocked(v)
			return Result{Affected: 1}, nil
		}
		return Result{}, nil
	}
	n := 0
	remaining := t.Order[:0]
	tick := &rowTicker{}
	for _, k := range t.Order {
		if err := tick.tick(ctx); err != nil {
			return Result{}, err
		}
		row, ok := t.Rows[k]
		if !ok {
			continue
		}
		if c == "" || matchWhereValue(row[c], v) {
			for column := range t.Indexes {
				t.removeIndexValue(column, valueKey(row[column]), k)
			}
			delete(t.Rows, k)
			t.removeFastRowLocked(k)
			n++
			continue
		}
		remaining = append(remaining, k)
	}
	t.Order = remaining
	t.dead = 0
	if len(t.fast) > 0 && t.fastDead*4 >= len(t.fast)+t.fastDead {
		t.rebuildFastPathLocked()
	}
	return Result{Affected: n}, nil
}
