package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func (d *Database) selectRows(ctx context.Context, s string, maxRows int) (Result, error) {
	fi := indexKeyword(s, "FROM")
	if fi < 0 {
		return Result{}, errors.New("SELECT requires FROM")
	}
	rest := strings.TrimSpace(s[fi+4:])
	tableName, ok := firstField(rest)
	if !ok {
		return Result{}, errors.New("table required")
	}
	t, err := d.table(tableName)
	if err != nil {
		return Result{}, err
	}
	selectExpr := strings.TrimSpace(s[6:fi])
	distinct := false
	if hasPrefixFold(selectExpr, "DISTINCT") && (len(selectExpr) == len("DISTINCT") || isBlankByte(selectExpr[len("DISTINCT")])) {
		distinct = true
		selectExpr = strings.TrimSpace(selectExpr[len("DISTINCT"):])
		if selectExpr == "" {
			return Result{}, errors.New("SELECT DISTINCT requires columns")
		}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	names := []string{}
	if selectExpr == "*" {
		for _, c := range t.Columns {
			names = append(names, c.Name)
		}
	} else {
		for _, x := range strings.Split(selectExpr, ",") {
			names = append(names, strings.ToLower(strings.TrimSpace(x)))
		}
	}
	tail, err := parseSelectTail(rest)
	if err != nil {
		return Result{}, err
	}
	// Only the table name may precede the first clause: previously a
	// trailing "JOIN ..." or alias was silently ignored while the single
	// table was scanned. Reject it instead of misreading the query.
	if fields := strings.Fields(rest[:tail.headEnd]); len(fields) != 1 {
		return Result{}, errors.New("invalid FROM clause")
	}
	where, orderColumn, descending, limit, offset := tail.where, tail.orderColumn, tail.descending, tail.limit, tail.offset
	if len(tail.groupBy) > 0 {
		return groupedResult(ctx, t, selectExpr, tail, distinct, maxRows)
	}
	if orderColumn != "" && !hasColumn(t.Columns, orderColumn) {
		return Result{}, fmt.Errorf("ORDER BY column not found: %s", orderColumn)
	}
	if isAggregate(selectExpr) {
		r, err := aggregateResult(ctx, t, selectExpr, where)
		if err == nil && (limit == 0 || offset > 0) {
			r.Rows = nil
		}
		return r, err
	}
	for _, n := range names {
		if !hasColumn(t.Columns, n) {
			return Result{}, fmt.Errorf("column not found: %s", n)
		}
	}
	tick := &rowTicker{}
	// rowCap bounds materialized rows: when a MaxRows policy applies, emit
	// at most maxRows+1 so exceeding the cap is detected (and aborted)
	// without buffering an unbounded result. The explicit LIMIT still
	// wins when it is smaller. OFFSET widens both caps by the skipped
	// count, since offset rows must be collected before they are dropped.
	rowCap := -1
	if limit >= 0 {
		rowCap = satAdd(limit, offset)
	}
	if maxRows > 0 {
		emitCap := satAdd(satAdd(maxRows, offset), 1)
		if rowCap < 0 || rowCap > emitCap {
			rowCap = emitCap
		}
	}
	// applyOffset drops the OFFSET head of a collected result.
	applyOffset := func(rows [][]any) [][]any {
		if offset <= 0 {
			return rows
		}
		if offset >= len(rows) {
			return rows[:0]
		}
		return rows[offset:]
	}
	// addLine deduplicates projected rows for SELECT DISTINCT. Keys are a
	// type-tagged, length-prefixed valueKey encoding, so ("a","b|c") can
	// never collide with ("a|b","c") and NULL never collides with "NULL".
	var seen map[string]struct{}
	addLine := func(rows [][]any, line []any) [][]any {
		if distinct {
			k := rowProjectionKey(line)
			if _, dup := seen[k]; dup {
				return rows
			}
			if seen == nil {
				seen = make(map[string]struct{})
			}
			seen[k] = struct{}{}
		}
		return append(rows, line)
	}
	// projectedKey resolves a stored row key to its DISTINCT dedup key,
	// preferring the fast array and falling back to the row map.
	projectedKey := func(key string) (string, bool) {
		if fi, ok := t.fastPos[key]; ok && fi >= 0 && fi < len(t.fast) && t.fast[fi].key != "" {
			return rowProjectionKey(selectedFastValues(t.fast[fi], names, t.columns)), true
		}
		if row, exists := t.Rows[key]; exists {
			return rowProjectionKey(selectedValues(row, names)), true
		}
		return "", false
	}
	simpleColumn, simpleValue, simple := simpleEquality(where)
	if simple && simpleColumn == t.primaryColumn() {
		r := Result{Columns: names}
		if rowIndex, fast := t.fastPos[simpleValue]; fast {
			if rowIndex >= 0 && rowIndex < len(t.fast) && t.fast[rowIndex].key != "" {
				r.Rows = [][]any{selectedFastValues(t.fast[rowIndex], names, t.columns)}
			}
			if limit == 0 {
				r.Rows = nil
			}
			r.Rows = applyOffset(r.Rows)
			return r, nil
		}
		if row, exists := t.Rows[simpleValue]; exists {
			r.Rows = append(r.Rows, selectedValues(row, names))
		}
		if limit == 0 {
			r.Rows = nil
		}
		r.Rows = applyOffset(r.Rows)
		return r, nil
	}
	var compiled compiledCondition
	var term fastTerm
	fastTermOK := false
	if len(t.fast) > 0 {
		term, fastTermOK = simpleFastTerm(where, t)
	}
	if where != "" && (!fastTermOK || orderColumn != "") {
		compiled, ok = compileCondition(where, t.Columns)
		if !ok {
			return Result{}, errors.New("invalid WHERE expression")
		}
	}
	// Preallocate by the cap when a LIMIT applies: LIMIT 10 over a
	// million-row table should not reserve a million slots.
	capHint := len(t.Order)
	if rowCap >= 0 && rowCap < capHint {
		capHint = rowCap
	}
	keys := make([]string, 0, capHint)
	indexed := false
	if simple && t.Indexes[simpleColumn] != nil {
		indexed = true
	}
	if len(t.fast) > 0 && orderColumn == "" && !indexed {
		scanRows := len(t.fast)
		if rowCap >= 0 && rowCap < scanRows {
			scanRows = rowCap
		}
		r := Result{Columns: names, Rows: make([][]any, 0, scanRows)}
		if limit == 0 {
			return r, nil
		}
		// Bare `column = value` predicates skip the compiled-condition
		// machinery: the column index is resolved once and rows are
		// compared with no per-row map lookups.
		if fastTermOK {
			emit := func(row fastRow) {
				r.Rows = addLine(r.Rows, selectedFastValues(row, names, t.columns))
			}
			switch term.kind {
			case fastInt:
				exp := term.expected.(int64)
				for _, row := range t.fast {
					if err := tick.tick(ctx); err != nil {
						return Result{}, err
					}
					if row.key == "" {
						continue
					}
					if v, isInt := row.values[term.index].(int64); isInt && v == exp {
						emit(row)
						if rowCap >= 0 && len(r.Rows) >= rowCap {
							break
						}
					}
				}
			case fastString:
				exp := term.expected.(string)
				for _, row := range t.fast {
					if err := tick.tick(ctx); err != nil {
						return Result{}, err
					}
					if row.key == "" {
						continue
					}
					if v, isString := row.values[term.index].(string); isString && v == exp {
						emit(row)
						if rowCap >= 0 && len(r.Rows) >= rowCap {
							break
						}
					}
				}
			case fastBool:
				exp := term.expected.(bool)
				for _, row := range t.fast {
					if err := tick.tick(ctx); err != nil {
						return Result{}, err
					}
					if row.key == "" {
						continue
					}
					if v, isBool := row.values[term.index].(bool); isBool && v == exp {
						emit(row)
						if rowCap >= 0 && len(r.Rows) >= rowCap {
							break
						}
					}
				}
			case fastFloat:
				exp := term.expected.(float64)
				for _, row := range t.fast {
					if err := tick.tick(ctx); err != nil {
						return Result{}, err
					}
					if row.key == "" {
						continue
					}
					if v, isFloat := row.values[term.index].(float64); isFloat && v == exp {
						emit(row)
						if rowCap >= 0 && len(r.Rows) >= rowCap {
							break
						}
					}
				}
			default:
				for _, row := range t.fast {
					if err := tick.tick(ctx); err != nil {
						return Result{}, err
					}
					if row.key == "" {
						continue
					}
					if where != "" && !conditionValueEqual(row.values[term.index], term.expected) {
						continue
					}
					emit(row)
					if rowCap >= 0 && len(r.Rows) >= rowCap {
						break
					}
				}
			}
			r.Rows = applyOffset(r.Rows)
			return r, capResultRows(r, maxRows)
		}
		for _, row := range t.fast {
			if err := tick.tick(ctx); err != nil {
				return Result{}, err
			}
			if row.key == "" {
				continue
			}
			if where != "" && !compiled.matchesFast(row.values, t.columns) {
				continue
			}
			r.Rows = addLine(r.Rows, selectedFastValues(row, names, t.columns))
			if rowCap >= 0 && len(r.Rows) >= rowCap {
				break
			}
		}
		r.Rows = applyOffset(r.Rows)
		return r, capResultRows(r, maxRows)
	}
	// dedupKey checks (and records) a candidate's projected row for SELECT
	// DISTINCT. It reports whether the candidate is a duplicate and can be
	// skipped during key collection, so early rowCap breaks still count
	// unique rows rather than raw matches.
	dedupKey := func(key string) bool {
		if !distinct {
			return false
		}
		pk, ok := projectedKey(key)
		if !ok {
			return false
		}
		if _, dup := seen[pk]; dup {
			return true
		}
		if seen == nil {
			seen = make(map[string]struct{})
		}
		seen[pk] = struct{}{}
		return false
	}
	if simple && t.Indexes[simpleColumn] != nil {
		// Verify candidates against the fast array when available: no
		// per-row map lookups, same single-term semantics via matchFastTerm.
		if fastTermOK {
			for key := range t.Indexes[simpleColumn][simpleValue] {
				if err := tick.tick(ctx); err != nil {
					return Result{}, err
				}
				if orderColumn == "" && rowCap >= 0 && len(keys) >= rowCap {
					break
				}
				if fi, present := t.fastPos[key]; present && t.fast[fi].key != "" {
					if matchFastTerm(t.fast[fi].values, term) && !dedupKey(key) {
						keys = append(keys, key)
					}
					continue
				}
				if row, exists := t.Rows[key]; exists && conditionValueEqual(row[simpleColumn], term.expected) && !dedupKey(key) {
					keys = append(keys, key)
				}
			}
		} else {
			for key := range t.Indexes[simpleColumn][simpleValue] {
				if err := tick.tick(ctx); err != nil {
					return Result{}, err
				}
				if orderColumn == "" && rowCap >= 0 && len(keys) >= rowCap {
					break
				}
				if row, exists := t.Rows[key]; exists && compiled.matchesMap(row) && !dedupKey(key) {
					keys = append(keys, key)
				}
			}
		}
	} else {
		for _, key := range t.Order {
			if err := tick.tick(ctx); err != nil {
				return Result{}, err
			}
			row, exists := t.Rows[key]
			if !exists {
				continue
			}
			if where != "" && !compiled.matchesMap(row) {
				continue
			}
			if dedupKey(key) {
				continue
			}
			keys = append(keys, key)
			if orderColumn == "" && rowCap >= 0 && len(keys) >= rowCap {
				break
			}
		}
	}
	if orderColumn != "" {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		// Order values are resolved once per row; when a LIMIT is smaller
		// than the candidate set a bounded top-K heap avoids the full sort.
		// DISTINCT sorts everything instead: truncating keys before dedup
		// could drop unique rows that a top-K cut cannot anticipate.
		orderCap := rowCap
		if distinct {
			orderCap = -1
		}
		keys = orderKeys(t, keys, orderColumn, descending, orderCap)
	}
	if !distinct && rowCap >= 0 && len(keys) > rowCap {
		keys = keys[:rowCap]
	}
	r := Result{Columns: names, Rows: make([][]any, 0, len(keys))}
	// Surviving keys are already DISTINCT-unique (dedupKey filtered them
	// during collection), so materialization appends directly.
	if len(t.fast) > 0 {
		for _, key := range keys {
			var line []any
			if fi, ok := t.fastPos[key]; ok && t.fast[fi].key != "" {
				line = selectedFastValues(t.fast[fi], names, t.columns)
			} else {
				line = selectedValues(t.Rows[key], names)
			}
			r.Rows = append(r.Rows, line)
			if rowCap >= 0 && len(r.Rows) >= rowCap {
				break
			}
		}
	} else {
		for _, key := range keys {
			r.Rows = append(r.Rows, selectedValues(t.Rows[key], names))
			if rowCap >= 0 && len(r.Rows) >= rowCap {
				break
			}
		}
	}
	r.Rows = applyOffset(r.Rows)
	return r, capResultRows(r, maxRows)
}

// capResultRows enforces the MaxRows policy: scans stop at maxRows+1
// emitted rows, so len(rows) > maxRows proves the cap was exceeded.
func capResultRows(r Result, maxRows int) error {
	if maxRows > 0 && len(r.Rows) > maxRows {
		return fmt.Errorf("%w: %d rows emitted (limit %d)", ErrResultTooLarge, len(r.Rows), maxRows)
	}
	return nil
}

func simpleEquality(expression string) (column, value string, ok bool) {
	if expression == "" || indexFold(expression, " AND ") >= 0 || indexFold(expression, " OR ") >= 0 {
		return "", "", false
	}
	parts := strings.SplitN(expression, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(parts[0])), unquoteLiteral(parts[1]), true
}

// fastTerm is a pre-resolved bare `column = value` predicate: the column
// index is looked up once per query instead of once per row, and kind
// selects a type-specialized comparison. kindFastGeneric preserves the exact
// conditionValueEqual semantics (including its fmt.Sprint fallback) for
// predicates where the expected value's type does not match the column.
type fastTerm struct {
	index    int
	expected any
	kind     uint8
}

const (
	kindFastGeneric uint8 = iota
	fastInt
	fastString
	fastBool
	fastFloat
)

// simpleFastTerm resolves a bare `column = value` predicate (no AND/OR) into
// a fastTerm with the exact single-term semantics of compileCondition.
// It reports false for anything else so callers fall back to the compiled
// path, including invalid expressions (which must still raise errors there).
func simpleFastTerm(expression string, t *Table) (fastTerm, bool) {
	if expression == "" || indexFold(expression, " AND ") >= 0 || indexFold(expression, " OR ") >= 0 {
		return fastTerm{}, false
	}
	parts := strings.SplitN(expression, "=", 2)
	if len(parts) != 2 {
		return fastTerm{}, false
	}
	column := strings.ToLower(strings.TrimSpace(parts[0]))
	if column == "" {
		return fastTerm{}, false
	}
	index, ok := t.columns[column]
	if !ok || index < 0 || index >= len(t.Columns) {
		return fastTerm{}, false
	}
	colType := t.Columns[index].Type
	raw := unquoteLiteral(parts[1])
	expected := any(raw)
	if value, err := parseVal(raw, colType); err == nil {
		expected = value
	}
	term := fastTerm{index: index, expected: expected}
	switch colType {
	case Int:
		if _, isInt := expected.(int64); isInt {
			term.kind = fastInt
		}
	case Text:
		if _, isString := expected.(string); isString {
			term.kind = fastString
		}
	case Bool:
		if _, isBool := expected.(bool); isBool {
			term.kind = fastBool
		}
	case Float:
		if _, isFloat := expected.(float64); isFloat {
			term.kind = fastFloat
		}
	}
	return term, true
}

// matchFastTerm evaluates a resolved term against one fast row.
func matchFastTerm(values []any, term fastTerm) bool {
	switch term.kind {
	case fastInt:
		v, ok := values[term.index].(int64)
		return ok && v == term.expected.(int64)
	case fastString:
		v, ok := values[term.index].(string)
		return ok && v == term.expected.(string)
	case fastBool:
		v, ok := values[term.index].(bool)
		return ok && v == term.expected.(bool)
	case fastFloat:
		v, ok := values[term.index].(float64)
		return ok && v == term.expected.(float64)
	default:
		return conditionValueEqual(values[term.index], term.expected)
	}
}

// indexOrderBy locates an "ORDER BY" clause outside quoted text, tolerating
// any whitespace between the keywords, and returns the clause start (the
// ORDER keyword) plus the offset just past BY. It reports -1 when absent;
// a bare ORDER (e.g. a column literally named "order") is not a clause.
func indexOrderBy(s string) (clauseStart, afterBy int) {
	return indexPairKeyword(s, "ORDER", "BY")
}

// indexGroupBy locates a "GROUP BY" clause with the same rules as
// indexOrderBy.
func indexGroupBy(s string) (clauseStart, afterBy int) {
	return indexPairKeyword(s, "GROUP", "BY")
}

// indexPairKeyword locates a two-word clause ("ORDER BY", "GROUP BY")
// outside quoted text, tolerating any whitespace between the words. It
// returns the clause start plus the offset just past the second word, or
// -1 when absent; a bare first word (e.g. a column named "order") is not
// a clause.
func indexPairKeyword(s, first, second string) (clauseStart, afterBy int) {
	start := 0
	for {
		i := indexKeyword(s[start:], first)
		if i < 0 {
			return -1, -1
		}
		i += start
		j := i + len(first)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
			j++
		}
		if foldEqualAt(s[j:], second) && (j+len(second) == len(s) || !identifierByte(s[j+len(second)])) {
			return i, j + len(second)
		}
		start = i + len(first)
	}
}

// selectTail is the parsed tail of a SELECT after the table name. headEnd
// marks where the table-name region ends (the first clause keyword, or the
// end of the string), so callers can reject junk like a JOIN remainder
// instead of silently reading only the first table.
type selectTail struct {
	where       string
	orderColumn string
	descending  bool
	limit       int
	offset      int
	groupBy     []string
	having      string
	headEnd     int
}

// parseSelectTail parses the clause sequence after `FROM table`. The
// canonical order is WHERE, GROUP BY, HAVING, ORDER BY, LIMIT, OFFSET;
// each present clause must strictly follow the previous one and carry a
// nonempty body.
func parseSelectTail(rest string) (selectTail, error) {
	tail := selectTail{limit: -1, headEnd: len(rest)}
	whereStart := indexKeyword(rest, "WHERE")
	groupStart, groupFields := indexGroupBy(rest)
	havingStart := indexKeyword(rest, "HAVING")
	orderStart, orderFields := indexOrderBy(rest)
	limitStart := indexKeyword(rest, "LIMIT")
	offsetStart := indexKeyword(rest, "OFFSET")
	// Clause order validation: positions must strictly increase.
	prev := -1
	for _, pos := range []int{whereStart, groupStart, havingStart, orderStart, limitStart, offsetStart} {
		if pos >= 0 {
			if pos <= prev {
				return tail, errors.New("invalid SELECT clause order")
			}
			prev = pos
		}
	}
	for _, pos := range []int{whereStart, groupStart, havingStart, orderStart, limitStart, offsetStart} {
		if pos >= 0 && pos < tail.headEnd {
			tail.headEnd = pos
		}
	}
	// clauseEnd returns the start of the first clause positioned after
	// `from`, bounding each clause body at the next clause keyword.
	clauseEnd := func(from int) int {
		end := len(rest)
		for _, pos := range []int{whereStart, groupStart, havingStart, orderStart, limitStart, offsetStart} {
			if pos > from && pos < end {
				end = pos
			}
		}
		return end
	}
	if whereStart >= 0 {
		tail.where = strings.TrimSpace(rest[whereStart+len("WHERE") : clauseEnd(whereStart)])
		if tail.where == "" {
			return tail, errors.New("invalid WHERE expression")
		}
	}
	if groupStart >= 0 {
		grp := strings.TrimSpace(rest[groupFields:clauseEnd(groupStart)])
		if grp == "" {
			return tail, errors.New("GROUP BY requires a column")
		}
		for _, g := range splitVals(grp) {
			g = strings.ToLower(strings.TrimSpace(g))
			if g == "" {
				return tail, errors.New("invalid GROUP BY expression")
			}
			tail.groupBy = append(tail.groupBy, g)
		}
	}
	if havingStart >= 0 {
		tail.having = strings.TrimSpace(rest[havingStart+len("HAVING") : clauseEnd(havingStart)])
		if tail.having == "" {
			return tail, errors.New("invalid HAVING expression")
		}
	}
	if tail.having != "" && len(tail.groupBy) == 0 {
		return tail, errors.New("HAVING requires GROUP BY")
	}
	if orderStart >= 0 {
		order := strings.Fields(strings.TrimSpace(rest[orderFields:clauseEnd(orderStart)]))
		if len(order) == 0 {
			return tail, errors.New("ORDER BY requires a column")
		}
		if len(order) > 2 {
			return tail, errors.New("invalid ORDER BY expression")
		}
		tail.orderColumn = strings.ToLower(order[0])
		if len(order) > 1 {
			switch strings.ToUpper(order[1]) {
			case "ASC":
			case "DESC":
				tail.descending = true
			default:
				return tail, errors.New("ORDER BY expects ASC or DESC")
			}
		}
	}
	if limitStart >= 0 {
		limit, err := strconv.Atoi(strings.TrimSpace(rest[limitStart+len("LIMIT") : clauseEnd(limitStart)]))
		if err != nil || limit < 0 {
			return tail, errors.New("LIMIT must be a non-negative integer")
		}
		tail.limit = limit
	}
	if offsetStart >= 0 {
		offset, err := strconv.Atoi(strings.TrimSpace(rest[offsetStart+len("OFFSET") : clauseEnd(offsetStart)]))
		if err != nil || offset < 0 {
			return tail, errors.New("OFFSET must be a non-negative integer")
		}
		tail.offset = offset
	}
	return tail, nil
}

func isAggregate(expression string) bool {
	trimmed := strings.TrimSpace(expression)
	return hasPrefixFold(trimmed, "COUNT(") || hasPrefixFold(trimmed, "SUM(") ||
		hasPrefixFold(trimmed, "MIN(") || hasPrefixFold(trimmed, "MAX(") || hasPrefixFold(trimmed, "AVG(")
}

func aggregateResult(ctx context.Context, t *Table, expression, where string) (Result, error) {
	trimmed := strings.TrimSpace(expression)
	isSum := hasPrefixFold(trimmed, "SUM(")
	isAvg := hasPrefixFold(trimmed, "AVG(")
	isMin := hasPrefixFold(trimmed, "MIN(")
	isMax := hasPrefixFold(trimmed, "MAX(")
	aggName := strings.ToLower(strings.TrimSpace(expression))
	open, close := strings.Index(expression, "("), strings.LastIndex(expression, ")")
	if open < 0 || close <= open {
		return Result{}, errors.New("invalid aggregate")
	}
	column := strings.ToLower(strings.TrimSpace(expression[open+1 : close]))
	isCountStar := hasPrefixFold(trimmed, "COUNT(") && column == "*"
	if column == "*" && !isCountStar {
		return Result{}, errors.New("invalid aggregate: only COUNT(*) supports *")
	}
	// Column validation is unconditional so aggregates on a missing column
	// error identically on small tables and the fast path.
	if column != "*" && !hasColumn(t.Columns, column) {
		return Result{}, fmt.Errorf("column not found: %s", column)
	}
	compiled, validCondition := compileCondition(where, t.Columns)
	if !validCondition {
		return Result{}, errors.New("invalid WHERE expression")
	}
	// COUNT(*) shortcuts: no WHERE is exactly the live row count (Order may
	// hold tombstones, Rows cannot), and a bare `column = value` on a
	// primary key or secondary index is a direct lookup verified with the
	// same semantics the SELECT paths use.
	if isCountStar {
		if where == "" {
			return Result{Columns: []string{aggName}, Rows: [][]any{{len(t.Rows)}}}, nil
		}
		if simpleColumn, simpleValue, simple := simpleEquality(where); simple {
			if simpleColumn == t.primaryColumn() {
				count := 0
				if _, exists := t.Rows[simpleValue]; exists {
					count = 1
				}
				return Result{Columns: []string{aggName}, Rows: [][]any{{count}}}, nil
			}
			if t.Indexes[simpleColumn] != nil {
				term, termOK := simpleFastTerm(where, t)
				count, err := t.countIndexEquality(ctx, simpleColumn, simpleValue, term, termOK, compiled)
				if err != nil {
					return Result{}, err
				}
				return Result{Columns: []string{aggName}, Rows: [][]any{{count}}}, nil
			}
		}
	}
	count := 0
	var total float64
	var minValue, maxValue any
	tick := &rowTicker{}
	if len(t.fast) > 0 {
		columnIndex, hasColumn := t.columns[column]
		if column != "*" && !hasColumn {
			return Result{}, errors.New("column not found")
		}
		matchAll := where == ""
		term, simple := simpleFastTerm(where, t)
		for _, row := range t.fast {
			if err := tick.tick(ctx); err != nil {
				return Result{}, err
			}
			if row.key == "" {
				continue
			}
			if !matchAll {
				if simple {
					if !matchFastTerm(row.values, term) {
						continue
					}
				} else if !compiled.matchesFast(row.values, t.columns) {
					continue
				}
			}
			if isCountStar {
				count++
				continue
			}
			value := row.values[columnIndex]
			if value == nil {
				continue
			}
			count++
			switch {
			case isSum || isAvg:
				v, ok := numeric(value)
				if !ok {
					return Result{}, errors.New(aggName + " requires numeric values")
				}
				total += v
			case isMin:
				if minValue == nil || valueLess(value, minValue) {
					minValue = value
				}
			case isMax:
				if maxValue == nil || valueLess(maxValue, value) {
					maxValue = value
				}
			}
		}
	} else {
		for _, key := range t.Order {
			if err := tick.tick(ctx); err != nil {
				return Result{}, err
			}
			row, exists := t.Rows[key]
			if !exists {
				continue
			}
			if where != "" && !compiled.matchesMap(row) {
				continue
			}
			if isCountStar {
				count++
				continue
			}
			value := row[column]
			if value == nil {
				continue
			}
			count++
			switch {
			case isSum || isAvg:
				v, ok := numeric(value)
				if !ok {
					return Result{}, errors.New(aggName + " requires numeric values")
				}
				total += v
			case isMin:
				if minValue == nil || valueLess(value, minValue) {
					minValue = value
				}
			case isMax:
				if maxValue == nil || valueLess(maxValue, value) {
					maxValue = value
				}
			}
		}
	}
	var value any = count
	switch {
	case isSum:
		value = total
	case isAvg:
		if count == 0 {
			value = nil
		} else {
			value = total / float64(count)
		}
	case isMin:
		value = minValue
	case isMax:
		value = maxValue
	case !hasPrefixFold(trimmed, "COUNT("):
		return Result{}, errors.New("unsupported aggregate")
	}
	return Result{Columns: []string{aggName}, Rows: [][]any{{value}}}, nil
}

// countIndexEquality counts rows matching a bare `column = value`
// predicate through the secondary index. Candidates are verified with the
// same logic the SELECT indexed path uses — fast-term match on the fast
// array, conditionValueEqual on the row map, or the compiled condition —
// so a COUNT returns exactly the number of rows SELECT would emit.
func (t *Table) countIndexEquality(ctx context.Context, column, value string, term fastTerm, termOK bool, compiled compiledCondition) (int, error) {
	count := 0
	tick := &rowTicker{}
	for key := range t.Indexes[column][value] {
		if err := tick.tick(ctx); err != nil {
			return 0, err
		}
		if termOK {
			if fi, present := t.fastPos[key]; present && fi >= 0 && fi < len(t.fast) && t.fast[fi].key != "" {
				if matchFastTerm(t.fast[fi].values, term) {
					count++
				}
				continue
			}
			if row, exists := t.Rows[key]; exists && conditionValueEqual(row[column], term.expected) {
				count++
			}
			continue
		}
		if row, exists := t.Rows[key]; exists && compiled.matchesMap(row) {
			count++
		}
	}
	return count, nil
}

// valueLess shares ordering between ORDER BY and MIN/MAX. Compare integers
// directly so adjacent int64 values above 2^53 never collapse to one float.
// Same-type string/bool fast paths avoid the fmt.Sprint fallback, which
// allocated once per row comparison on TEXT and BOOL sorts.
func valueLess(left, right any) bool {
	switch l := left.(type) {
	case int64:
		if r, ok := right.(int64); ok {
			return l < r
		}
	case string:
		if r, ok := right.(string); ok {
			return l < r
		}
	case bool:
		// Matches the legacy "false" < "true" rendered-string ordering.
		if r, ok := right.(bool); ok {
			return !l && r
		}
	case float64:
		if r, ok := right.(float64); ok {
			return l < r
		}
	}
	if l, ok := numeric(left); ok {
		if r, ok := numeric(right); ok {
			return l < r
		}
	}
	return fmt.Sprint(left) < fmt.Sprint(right)
}

func numeric(value any) (float64, bool) {
	switch v := value.(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	default:
		return 0, false
	}
}

func selectedValues(row map[string]any, names []string) []any {
	line := make([]any, 0, len(names))
	for _, n := range names {
		line = append(line, row[n])
	}
	return line
}

func selectedFastValues(row fastRow, names []string, columns map[string]int) []any {
	line := make([]any, 0, len(names))
	for _, name := range names {
		index, ok := columns[name]
		if !ok || index >= len(row.values) {
			line = append(line, nil)
			continue
		}
		line = append(line, row.values[index])
	}
	return line
}

// satAdd adds non-negative ints, saturating at MaxInt so a LIMIT/OFFSET
// sum can never wrap to a negative (uncapped) value.
func satAdd(a, b int) int {
	if a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

func isBlankByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// rowProjectionKey renders one projected result row as a DISTINCT dedup
// key. Each element is written as a type tag plus a length-prefixed
// valueKey rendering, making the encoding unambiguous across types and
// separators ("1" vs 1, NULL vs "NULL", embedded delimiters).
func rowProjectionKey(row []any) string {
	var b strings.Builder
	for _, v := range row {
		var tag byte
		switch v.(type) {
		case int64:
			tag = 'i'
		case float64:
			tag = 'f'
		case string:
			tag = 's'
		case bool:
			tag = 'b'
		default:
			tag = 'n'
		}
		s := valueKey(v)
		b.WriteByte(tag)
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	return b.String()
}
