package engine

import (
	"errors"
	"strings"
)

func (d *Database) delete(s string) (Result, error) {
	rest := strings.TrimSpace(s[len("DELETE FROM"):])
	tableName, ok := firstField(rest)
	if !ok {
		return Result{}, errors.New("invalid DELETE")
	}
	t, e := d.table(tableName)
	if e != nil {
		return Result{}, e
	}
	c, v := filter(s)
	t.mu.Lock()
	defer t.mu.Unlock()
	if c != "" && c == t.primaryColumn() {
		if _, ok := t.Rows[v]; ok {
			for column := range t.Indexes {
				t.removeIndexValue(column, valueKey(t.Rows[v][column]), v)
			}
			delete(t.Rows, v)
			// Leave the Order slot in place: scans already skip keys
			// missing from Rows. Reaped by compaction once stale keys
			// reach a quarter of Order, keeping deletes O(1).
			t.dead++
			t.removeFastRowLocked(v)
			t.compactOrderLocked()
			return Result{Affected: 1}, nil
		}
		return Result{}, nil
	}
	n := 0
	remaining := t.Order[:0]
	for _, k := range t.Order {
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
