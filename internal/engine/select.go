package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	where, orderColumn, descending, limit, err := parseSelectTail(rest)
	if err != nil {
		return Result{}, err
	}
	if isAggregate(selectExpr) {
		return aggregateResult(ctx, t, selectExpr, where)
	}
	tick := &rowTicker{}
	// rowCap bounds materialized rows: when a MaxRows policy applies, emit
	// at most maxRows+1 so exceeding the cap is detected (and aborted)
	// without buffering an unbounded result. The explicit LIMIT still
	// wins when it is smaller.
	rowCap := limit
	if maxRows > 0 && (rowCap < 0 || rowCap > maxRows) {
		rowCap = maxRows + 1
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
			return r, nil
		}
		if row, exists := t.Rows[simpleValue]; exists {
			r.Rows = append(r.Rows, selectedValues(row, names))
		}
		if limit == 0 {
			r.Rows = nil
		}
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
	keys := make([]string, 0, len(t.Order))
	indexed := false
	if simple && t.Indexes[simpleColumn] != nil {
		indexed = true
	}
	if len(t.fast) > 0 && orderColumn == "" && !indexed {
		r := Result{Columns: names, Rows: make([][]any, 0, len(t.fast))}
		if limit == 0 {
			return r, nil
		}
		// Bare `column = value` predicates skip the compiled-condition
		// machinery: the column index is resolved once and rows are
		// compared with no per-row map lookups.
		if fastTermOK {
			emit := func(row fastRow) {
				r.Rows = append(r.Rows, selectedFastValues(row, names, t.columns))
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
			r.Rows = append(r.Rows, selectedFastValues(row, names, t.columns))
			if rowCap >= 0 && len(r.Rows) >= rowCap {
				break
			}
		}
		return r, capResultRows(r, maxRows)
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
					if matchFastTerm(t.fast[fi].values, term) {
						keys = append(keys, key)
					}
					continue
				}
				if row, exists := t.Rows[key]; exists && conditionValueEqual(row[simpleColumn], term.expected) {
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
				if row, exists := t.Rows[key]; exists && compiled.matchesMap(row) {
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
		sort.SliceStable(keys, func(i, j int) bool {
			leftValue, rightValue := t.Rows[keys[i]][orderColumn], t.Rows[keys[j]][orderColumn]
			if descending {
				return valueLess(rightValue, leftValue)
			}
			return valueLess(leftValue, rightValue)
		})
	}
	if rowCap >= 0 && len(keys) > rowCap {
		keys = keys[:rowCap]
	}
	r := Result{Columns: names, Rows: make([][]any, 0, len(keys))}
	if len(t.fast) > 0 {
		for _, key := range keys {
			if fi, ok := t.fastPos[key]; ok && t.fast[fi].key != "" {
				r.Rows = append(r.Rows, selectedFastValues(t.fast[fi], names, t.columns))
				continue
			}
			r.Rows = append(r.Rows, selectedValues(t.Rows[key], names))
		}
	} else {
		for _, key := range keys {
			r.Rows = append(r.Rows, selectedValues(t.Rows[key], names))
		}
	}
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
	return strings.ToLower(strings.TrimSpace(parts[0])), strings.Trim(strings.TrimSpace(parts[1]), "'"), true
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
	raw := strings.Trim(strings.TrimSpace(parts[1]), "'")
	expected := any(raw)
	if value, err := parseVal(parts[1], colType); err == nil {
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

func parseSelectTail(rest string) (where, orderColumn string, descending bool, limit int, err error) {
	limit = -1
	whereStart := indexKeyword(rest, "WHERE")
	orderStart := indexKeyword(rest, "ORDER BY")
	limitStart := indexKeyword(rest, "LIMIT")
	if whereStart >= 0 && (orderStart >= 0 && orderStart < whereStart || limitStart >= 0 && limitStart < whereStart) ||
		orderStart >= 0 && limitStart >= 0 && limitStart < orderStart {
		return "", "", false, -1, errors.New("invalid SELECT clause order")
	}
	end := len(rest)
	if orderStart >= 0 && orderStart < end {
		end = orderStart
	}
	if limitStart >= 0 && limitStart < end {
		end = limitStart
	}
	if whereStart >= 0 {
		where = strings.TrimSpace(rest[whereStart+5 : end])
		if where == "" {
			return "", "", false, -1, errors.New("invalid WHERE expression")
		}
	}
	if orderStart >= 0 {
		orderEnd := len(rest)
		if limitStart > orderStart {
			orderEnd = limitStart
		}
		order := strings.Fields(strings.TrimSpace(rest[orderStart+8 : orderEnd]))
		if len(order) == 0 {
			return "", "", false, -1, errors.New("ORDER BY requires a column")
		}
		if len(order) > 2 {
			return "", "", false, -1, errors.New("invalid ORDER BY expression")
		}
		orderColumn = strings.ToLower(order[0])
		if len(order) > 1 {
			switch strings.ToUpper(order[1]) {
			case "ASC":
			case "DESC":
				descending = true
			default:
				return "", "", false, -1, errors.New("ORDER BY expects ASC or DESC")
			}
		}
	}
	if limitStart >= 0 {
		limit, err = strconv.Atoi(strings.TrimSpace(rest[limitStart+5:]))
		if err != nil || limit < 0 {
			return "", "", false, -1, errors.New("LIMIT must be a non-negative integer")
		}
	}
	return where, orderColumn, descending, limit, nil
}

func isAggregate(expression string) bool {
	trimmed := strings.TrimSpace(expression)
	return hasPrefixFold(trimmed, "COUNT(") || hasPrefixFold(trimmed, "SUM(") || hasPrefixFold(trimmed, "MIN(") || hasPrefixFold(trimmed, "MAX(")
}

func aggregateResult(ctx context.Context, t *Table, expression, where string) (Result, error) {
	trimmed := strings.TrimSpace(expression)
	isSum := hasPrefixFold(trimmed, "SUM(")
	isMin := hasPrefixFold(trimmed, "MIN(")
	isMax := hasPrefixFold(trimmed, "MAX(")
	open, close := strings.Index(expression, "("), strings.LastIndex(expression, ")")
	if open < 0 || close <= open {
		return Result{}, errors.New("invalid aggregate")
	}
	column := strings.ToLower(strings.TrimSpace(expression[open+1 : close]))
	isCountStar := hasPrefixFold(trimmed, "COUNT(") && column == "*"
	compiled, validCondition := compileCondition(where, t.Columns)
	if !validCondition {
		return Result{}, errors.New("invalid WHERE expression")
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
			case isSum:
				v, ok := numeric(value)
				if !ok {
					return Result{}, errors.New("SUM requires numeric values")
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
			case isSum:
				v, ok := numeric(value)
				if !ok {
					return Result{}, errors.New("SUM requires numeric values")
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
	case isMin:
		value = minValue
	case isMax:
		value = maxValue
	case !hasPrefixFold(trimmed, "COUNT("):
		return Result{}, errors.New("unsupported aggregate")
	}
	return Result{Columns: []string{strings.ToLower(strings.TrimSpace(expression))}, Rows: [][]any{{value}}}, nil
}

// valueLess shares ordering between ORDER BY and MIN/MAX. Compare integers
// directly so adjacent int64 values above 2^53 never collapse to one float.
func valueLess(left, right any) bool {
	if l, ok := left.(int64); ok {
		if r, ok := right.(int64); ok {
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
