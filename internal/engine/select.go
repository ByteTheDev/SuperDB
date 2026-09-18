package engine

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	t, err := d.table(parts[0])
	if err != nil {
		return Result{}, err
	}
	selectExpr := strings.TrimSpace(s[6:fi])
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
		return aggregateResult(t, selectExpr, where)
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	compiled, validCondition := compileCondition(where, t.Columns)
	if !validCondition {
		return Result{}, errors.New("invalid WHERE expression")
	}
	if column, value, ok := simpleEquality(where); ok && column == t.primaryColumn() {
		r := Result{Columns: names}
		if rowIndex, fast := t.fastPos[value]; fast {
			if rowIndex >= 0 && rowIndex < len(t.fast) && t.fast[rowIndex].key != "" {
				r.Rows = [][]any{selectedFastValues(t.fast[rowIndex], names, t.columns)}
			}
			if limit == 0 {
				r.Rows = nil
			}
			return r, nil
		}
		if row, exists := t.Rows[value]; exists {
			r.Rows = append(r.Rows, selectedValues(row, names))
		}
		if limit == 0 {
			r.Rows = nil
		}
		return r, nil
	}
	keys := make([]string, 0, len(t.Order))
	indexed := false
	if column, _, ok := simpleEquality(where); ok && t.Indexes[column] != nil {
		indexed = true
	}
	if len(t.fast) > 0 && orderColumn == "" && !indexed {
		r := Result{Columns: names, Rows: make([][]any, 0, len(t.fast))}
		if limit == 0 {
			return r, nil
		}
		for _, row := range t.fast {
			if row.key == "" {
				continue
			}
			if where != "" && !compiled.matchesFast(row.values, t.columns) {
				continue
			}
			r.Rows = append(r.Rows, selectedFastValues(row, names, t.columns))
			if limit >= 0 && len(r.Rows) >= limit {
				break
			}
		}
		return r, nil
	}
	if column, value, ok := simpleEquality(where); ok && t.Indexes[column] != nil {
		for key := range t.Indexes[column][value] {
			if row, exists := t.Rows[key]; exists && compiled.matchesMap(row) {
				keys = append(keys, key)
			}
		}
	} else {
		for _, key := range t.Order {
			row := t.Rows[key]
			if where != "" && !compiled.matchesMap(row) {
				continue
			}
			keys = append(keys, key)
		}
	}
	if orderColumn != "" {
		sort.SliceStable(keys, func(i, j int) bool {
			leftValue, rightValue := t.Rows[keys[i]][orderColumn], t.Rows[keys[j]][orderColumn]
			left, right := fmt.Sprint(leftValue), fmt.Sprint(rightValue)
			if leftNumber, leftOK := numeric(leftValue); leftOK {
				if rightNumber, rightOK := numeric(rightValue); rightOK {
					if descending {
						return leftNumber > rightNumber
					}
					return leftNumber < rightNumber
				}
			}
			if descending {
				return left > right
			}
			return left < right
		})
	}
	if limit >= 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	r := Result{Columns: names, Rows: make([][]any, 0, len(keys))}
	for _, key := range keys {
		r.Rows = append(r.Rows, selectedValues(t.Rows[key], names))
	}
	return r, nil
}

func simpleEquality(expression string) (column, value string, ok bool) {
	if expression == "" || strings.Contains(strings.ToUpper(expression), " AND ") || strings.Contains(strings.ToUpper(expression), " OR ") {
		return "", "", false
	}
	parts := strings.SplitN(expression, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(parts[0])), strings.Trim(strings.TrimSpace(parts[1]), "'"), true
}

func parseSelectTail(rest string) (where, orderColumn string, descending bool, limit int, err error) {
	limit = -1
	upper := strings.ToUpper(rest)
	whereStart := strings.Index(upper, "WHERE")
	orderStart := strings.Index(upper, "ORDER BY")
	limitStart := strings.Index(upper, "LIMIT")
	end := len(rest)
	if orderStart >= 0 && orderStart < end {
		end = orderStart
	}
	if limitStart >= 0 && limitStart < end {
		end = limitStart
	}
	if whereStart >= 0 {
		where = strings.TrimSpace(rest[whereStart+5 : end])
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
	upper := strings.ToUpper(strings.TrimSpace(expression))
	return strings.HasPrefix(upper, "COUNT(") || strings.HasPrefix(upper, "SUM(") || strings.HasPrefix(upper, "MIN(") || strings.HasPrefix(upper, "MAX(")
}

func aggregateResult(t *Table, expression, where string) (Result, error) {
	upper := strings.ToUpper(strings.TrimSpace(expression))
	open, close := strings.Index(expression, "("), strings.LastIndex(expression, ")")
	if open < 0 || close <= open {
		return Result{}, errors.New("invalid aggregate")
	}
	column := strings.ToLower(strings.TrimSpace(expression[open+1 : close]))
	t.mu.RLock()
	defer t.mu.RUnlock()
	compiled, validCondition := compileCondition(where, t.Columns)
	if !validCondition {
		return Result{}, errors.New("invalid WHERE expression")
	}
	count := 0
	var total float64
	var minValue, maxValue any
	if len(t.fast) > 0 {
		columnIndex, hasColumn := t.columns[column]
		if column != "*" && !hasColumn {
			return Result{}, errors.New("column not found")
		}
		for _, row := range t.fast {
			if row.key == "" || (where != "" && !compiled.matchesFast(row.values, t.columns)) {
				continue
			}
			count++
			if upper == "COUNT(*)" {
				continue
			}
			value := row.values[columnIndex]
			if value == nil {
				continue
			}
			switch {
			case strings.HasPrefix(upper, "SUM("):
				v, ok := numeric(value)
				if !ok {
					return Result{}, errors.New("SUM requires numeric values")
				}
				total += v
			case strings.HasPrefix(upper, "MIN("):
				if minValue == nil || fmt.Sprint(value) < fmt.Sprint(minValue) {
					minValue = value
				}
			case strings.HasPrefix(upper, "MAX("):
				if maxValue == nil || fmt.Sprint(value) > fmt.Sprint(maxValue) {
					maxValue = value
				}
			}
		}
	} else {
		for _, key := range t.Order {
			row := t.Rows[key]
			if where != "" && !compiled.matchesMap(row) {
				continue
			}
			count++
			if upper == "COUNT(*)" {
				continue
			}
			value := row[column]
			if value == nil {
				continue
			}
			switch {
			case strings.HasPrefix(upper, "SUM("):
				v, ok := numeric(value)
				if !ok {
					return Result{}, errors.New("SUM requires numeric values")
				}
				total += v
			case strings.HasPrefix(upper, "MIN("):
				if minValue == nil || fmt.Sprint(value) < fmt.Sprint(minValue) {
					minValue = value
				}
			case strings.HasPrefix(upper, "MAX("):
				if maxValue == nil || fmt.Sprint(value) > fmt.Sprint(maxValue) {
					maxValue = value
				}
			}
		}
	}
	var value any = count
	switch {
	case strings.HasPrefix(upper, "SUM("):
		value = total
	case strings.HasPrefix(upper, "MIN("):
		value = minValue
	case strings.HasPrefix(upper, "MAX("):
		value = maxValue
	case !strings.HasPrefix(upper, "COUNT("):
		return Result{}, errors.New("unsupported aggregate")
	}
	return Result{Columns: []string{strings.ToLower(strings.TrimSpace(expression))}, Rows: [][]any{{value}}}, nil
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
