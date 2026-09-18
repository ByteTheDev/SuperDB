package engine

import (
	"errors"
	"fmt"
	"strings"
)

func (d *Database) delete(s string) (Result, error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return Result{}, errors.New("invalid DELETE")
	}
	t, e := d.table(f[2])
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
			t.removeOrderKey(v)
			t.removeFastRowLocked(v)
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
		if c == "" || fmt.Sprint(row[c]) == v {
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
	return Result{Affected: n}, nil
}

func (t *Table) removeOrderKey(key string) {
	for i, k := range t.Order {
		if k == key {
			t.Order = append(t.Order[:i], t.Order[i+1:]...)
			return
		}
	}
}
