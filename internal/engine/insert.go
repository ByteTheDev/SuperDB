package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func (d *Database) insert(ctx context.Context, s string, maxRows int) (Result, error) {
	// INSERT [OR REPLACE] INTO ...
	body := strings.TrimSpace(s[len("INSERT"):])
	replace := false
	if hasPrefixFold(body, "OR") && (len(body) == len("OR") || !identifierByte(body[len("OR")])) {
		body = strings.TrimSpace(body[len("OR"):])
		if !hasPrefixFold(body, "REPLACE") || (len(body) > len("REPLACE") && identifierByte(body[len("REPLACE")])) {
			return Result{}, errors.New("invalid INSERT")
		}
		replace = true
		body = strings.TrimSpace(body[len("REPLACE"):])
	}
	if !hasPrefixFold(body, "INTO") || (len(body) > len("INTO") && identifierByte(body[len("INTO")])) {
		return Result{}, errors.New("invalid INSERT")
	}
	vi := indexKeyword(s, "VALUES")
	si := indexKeyword(s, "SELECT")
	if vi < 0 && si < 0 {
		return Result{}, errors.New("INSERT requires VALUES or SELECT")
	}
	// Only the short table-name token is scanned; the VALUES/SELECT payload
	// can be large and must not be split just to find the destination table.
	// `off` is where `body` starts inside s, so s-offsets convert by -off.
	off := len(s) - len(body) + len("INTO")
	if si >= 0 && (vi < 0 || si < vi) {
		return d.insertSelect(ctx, s, off, si, replace, maxRows)
	}
	tableName, ok := firstField(s[off:vi])
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
	rows, err := parseInsertRowsMode(s[vi+6:], t.Columns, replace)
	if err != nil {
		return Result{}, err
	}
	return t.applyInsertRows(ctx, rows, replace)
}

// insertSelect implements INSERT INTO dst SELECT ..., including
// INSERT OR REPLACE. The source query runs to completion before the
// destination table is locked, so a self-insert (dst == src) cannot
// deadlock: rows are fully materialized by the SELECT first.
func (d *Database) insertSelect(ctx context.Context, s string, intoEnd, selectStart int, replace bool, maxRows int) (Result, error) {
	tableName, ok := firstField(s[intoEnd:selectStart])
	if !ok {
		return Result{}, errors.New("invalid INSERT")
	}
	t, e := d.table(tableName)
	if e != nil {
		return Result{}, e
	}
	// The source SELECT honors the same MaxRows policy any SELECT would:
	// copying a table that exceeds the configured cap aborts rather than
	// materializing an unbounded result.
	src, err := d.selectRows(ctx, strings.TrimSpace(s[selectStart:]), maxRows)
	if err != nil {
		return Result{}, err
	}
	if len(src.Columns) != len(t.Columns) {
		return Result{}, errors.New("column count mismatch")
	}
	rows := make([]insertRow, 0, len(src.Rows))
	for _, line := range src.Rows {
		row := make(map[string]any, len(t.Columns))
		key := ""
		for i, c := range t.Columns {
			var v any
			if i < len(line) {
				v = line[i]
			}
			cv, err := coerceValue(v, c.Type)
			if err != nil {
				return Result{}, err
			}
			row[c.Name] = cv
			if c.Primary {
				key = valueKey(cv)
			}
		}
		if key == "" && hasPrimaryColumn(t.Columns) {
			return Result{}, errors.New("primary key required")
		}
		rows = append(rows, insertRow{key: key, row: row})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.applyInsertRows(ctx, rows, replace)
}

// coerceValue adapts a value produced by SELECT to the destination column
// type for INSERT INTO ... SELECT. Numeric conversions follow the same
// leniency JSON decoding gets (float64 carrying an integral value fits an
// INT column); strings may parse into typed values like VALUES literals.
func coerceValue(v any, typ Type) (any, error) {
	if v == nil {
		return nil, nil
	}
	mismatch := errors.New("type mismatch")
	switch typ {
	case Int:
		switch x := v.(type) {
		case int64:
			return x, nil
		case float64:
			if x == math.Trunc(x) && x >= -9.223372036854776e18 && x <= 9.223372036854776e18 {
				return int64(x), nil
			}
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
				return n, nil
			}
		}
	case Float:
		switch x := v.(type) {
		case int64:
			return float64(x), nil
		case float64:
			return x, nil
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return f, nil
			}
		}
	case Text:
		// Any typed value renders to text, matching how a VALUES literal
		// like (1) unquotes into a TEXT column.
		return fmt.Sprint(v), nil
	case Bool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(x)); err == nil {
				return b, nil
			}
		}
	}
	return nil, mismatch
}

// applyInsertRows runs the shared tail of INSERT: duplicate-key policy,
// tombstone reclaim, row/index/fast-path application. With replace it
// upserts — existing keys keep their Order position while their row,
// index entries, and fast values are swapped. Caller holds t.mu.
func (t *Table) applyInsertRows(ctx context.Context, rows []insertRow, replace bool) (Result, error) {
	tick := &rowTicker{}
	if !replace {
		// Reject duplicates against existing rows and within the batch
		// itself. The VALUES parser already pre-checks batch duplicates,
		// but INSERT INTO ... SELECT rows reach here without that pass.
		batch := make(map[string]struct{}, len(rows))
		for _, item := range rows {
			if err := tick.tick(ctx); err != nil {
				return Result{}, err
			}
			if item.key != "" {
				if _, ok := t.Rows[item.key]; ok {
					return Result{}, errors.New("duplicate primary key")
				}
				if _, ok := batch[item.key]; ok {
					return Result{}, errors.New("duplicate primary key")
				}
				batch[item.key] = struct{}{}
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
		key := rows[i].key
		if replace {
			if old, exists := t.Rows[key]; exists {
				for column := range t.Indexes {
					t.removeIndexValue(column, valueKey(old[column]), key)
				}
				t.Rows[key] = rows[i].row
				for column := range t.Indexes {
					t.addIndexValue(column, valueKey(rows[i].row[column]), key)
				}
				t.replaceFastRowLocked(key, rows[i].row)
				continue
			}
		}
		t.Rows[key] = rows[i].row
		t.Order = append(t.Order, key)
		for column := range t.Indexes {
			t.addIndexValue(column, valueKey(rows[i].row[column]), key)
		}
		t.appendFastRowLocked(key, rows[i].row)
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
