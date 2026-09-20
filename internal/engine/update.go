package engine

import (
	"errors"
	"strings"
)

func (d *Database) update(s string) (Result, error) {
	si := indexFold(s, "SET")
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
	wi := indexFold(tail, "WHERE")
	assign := strings.TrimSpace(tail)
	if wi >= 0 {
		assign = strings.TrimSpace(tail[:wi])
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
	hasWhere := wi >= 0
	if hasWhere {
		var err error
		wc, wv, _, err = parseMutationWhere(s)
		if err != nil {
			return Result{}, err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	pk := t.primaryColumn()
	isPKUpdate := col == pk && pk != ""
	if wc != "" && wc == pk {
		if row, ok := t.Rows[wv]; ok {
			if !isPKUpdate {
				old := valueKey(row[col])
				row[col] = v
				t.removeIndexValue(col, old, wv)
				t.addIndexValue(col, valueKey(v), wv)
				t.updateFastRowLocked(wv, col, v)
				return Result{Affected: 1}, nil
			}
			return t.rekeyPrimaryLocked(wv, v)
		}
		return Result{}, nil
	}
	if isPKUpdate {
		return t.updatePrimaryMultiLocked(wc, wv, v)
	}
	n := 0
	for key, row := range t.Rows {
		if wc != "" && !matchWhereValue(row[wc], wv) {
			continue
		}
		t.removeIndexValue(col, valueKey(row[col]), key)
		t.addIndexValue(col, valueKey(v), key)
		row[col] = v
		t.updateFastRowLocked(key, col, v)
		n++
	}
	return Result{Affected: n}, nil
}

// rekeyPrimaryLocked moves a single row to a new primary key, keeping Rows,
// Order, secondary indexes, and the fast path consistent. Caller holds t.mu.
func (t *Table) rekeyPrimaryLocked(oldKey string, newVal any) (Result, error) {
	newKey := valueKey(newVal)
	row := t.Rows[oldKey]
	if newKey == oldKey {
		pk := t.primaryColumn()
		old := valueKey(row[pk])
		row[pk] = newVal
		t.removeIndexValue(pk, old, oldKey)
		t.addIndexValue(pk, newKey, oldKey)
		t.updateFastRowLocked(oldKey, pk, newVal)
		return Result{Affected: 1}, nil
	}
	if _, dup := t.Rows[newKey]; dup {
		return Result{}, errors.New("duplicate primary key")
	}
	for column := range t.Indexes {
		t.removeIndexValue(column, valueKey(row[column]), oldKey)
	}
	delete(t.Rows, oldKey)
	row[t.primaryColumn()] = newVal
	t.Rows[newKey] = row
	for i, k := range t.Order {
		if k == oldKey {
			t.Order[i] = newKey
			break
		}
	}
	for column := range t.Indexes {
		t.addIndexValue(column, valueKey(row[column]), newKey)
	}
	t.rekeyFastRowLocked(oldKey, newKey, newVal)
	return Result{Affected: 1}, nil
}

// updatePrimaryMultiLocked handles SET on the primary column for predicates
// that are not a single primary-key equality (full-table or secondary
// scans). It collects matches first so map mutation is safe, rejects
// duplicates (against untouched rows and within the matched set), then
// rekeys each row through rekeyPrimaryLocked.
func (t *Table) updatePrimaryMultiLocked(wc, wv string, v any) (Result, error) {
	keys := make([]string, 0, 1)
	for key, row := range t.Rows {
		if wc != "" && !matchWhereValue(row[wc], wv) {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return Result{Affected: 0}, nil
	}
	newKey := valueKey(v)
	if len(keys) > 1 {
		return Result{}, errors.New("duplicate primary key")
	}
	if _, dup := t.Rows[newKey]; dup && newKey != keys[0] {
		return Result{}, errors.New("duplicate primary key")
	}
	return t.rekeyPrimaryLocked(keys[0], v)
}
