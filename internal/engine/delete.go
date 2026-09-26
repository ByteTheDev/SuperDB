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
	c, v, op, _, err := parseMutationWhere(s)
	if err != nil {
		return Result{}, err
	}
	expected, err := mutationExpected(t, c, v, op)
	if err != nil {
		return Result{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if c != "" && c == t.primaryColumn() && op == opEq {
		if _, ok := t.Rows[v]; ok {
			for column := range t.Indexes {
				t.removeIndexValue(column, valueKey(t.Rows[v][column]), v)
			}
			delete(t.Rows, v)
			// Leave a tombstone in Order. Removing from the middle shifts every
			// later key, making batches of primary-key deletes quadratic.
			t.dead++
			t.removeFastRowLocked(v)
			t.compactOrderLocked()
			if len(t.fast) > 0 && t.fastDead*4 >= len(t.fast)+t.fastDead {
				t.rebuildFastPathLocked()
			}
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
		if c == "" || matchWhereOp(row[c], v, expected, op) {
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
	// Clear the tail so dropped keys do not pin row memory.
	for i := len(remaining); i < len(t.Order); i++ {
		t.Order[i] = ""
	}
	t.Order = remaining
	t.dead = 0
	if len(t.fast) > 0 && t.fastDead*4 >= len(t.fast)+t.fastDead {
		t.rebuildFastPathLocked()
	}
	return Result{Affected: n}, nil
}
