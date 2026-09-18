package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func splitVals(s string) []string {
	var out []string
	start := 0
	quote := false
	for i, r := range s {
		if r == '\'' {
			quote = !quote
		}
		if r == ',' && !quote {
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

func parseVal(raw string, typ Type) (any, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "NULL") {
		return nil, nil
	}
	if typ == Text {
		return strings.Trim(raw, "'"), nil
	}
	switch typ {
	case Int:
		return strconv.ParseInt(raw, 10, 64)
	case Float:
		return strconv.ParseFloat(raw, 64)
	case Bool:
		return strconv.ParseBool(raw)
	}
	return nil, errors.New("unknown type")
}

func filter(s string) (string, string) {
	u := strings.ToUpper(s)
	i := strings.Index(u, "WHERE")
	if i < 0 {
		return "", ""
	}
	p := strings.SplitN(strings.TrimSpace(s[i+5:]), "=", 2)
	if len(p) != 2 {
		return "", ""
	}
	return strings.ToLower(strings.TrimSpace(p[0])), strings.Trim(strings.TrimSpace(p[1]), "'")
}

func splitLogical(s, operator string) []string {
	upper := strings.ToUpper(s)
	needle := " " + operator + " "
	var out []string
	start := 0
	quote := false
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i] == '\'' {
			quote = !quote
		}
		if !quote && upper[i:i+len(needle)] == needle {
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + len(needle)
			i += len(needle) - 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

func condition(row map[string]any, expression string) bool {
	compiled, ok := compileCondition(expression, nil)
	return ok && compiled.matchesMap(row)
}

type conditionTerm struct {
	column   string
	expected any
}

// compiledCondition parses a WHERE expression once so row evaluation does not
// repeatedly allocate strings or split the same expression.
type compiledCondition [][]conditionTerm

func compileCondition(expression string, columns []Column) (compiledCondition, bool) {
	if strings.TrimSpace(expression) == "" {
		return nil, true
	}
	types := make(map[string]Type, len(columns))
	for _, column := range columns {
		types[column.Name] = column.Type
	}
	compiled := make(compiledCondition, 0, 1)
	for _, disjunction := range splitLogical(expression, "OR") {
		group := make([]conditionTerm, 0, 1)
		for _, conjunction := range splitLogical(disjunction, "AND") {
			parts := strings.SplitN(conjunction, "=", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
				return nil, false
			}
			column := strings.ToLower(strings.TrimSpace(parts[0]))
			raw := strings.Trim(strings.TrimSpace(parts[1]), "'")
			expected := any(raw)
			if typ, exists := types[column]; exists {
				if value, err := parseVal(parts[1], typ); err == nil {
					expected = value
				}
			}
			group = append(group, conditionTerm{column: column, expected: expected})
		}
		compiled = append(compiled, group)
	}
	return compiled, true
}

func (c compiledCondition) matchesMap(row map[string]any) bool {
	for _, group := range c {
		matches := true
		for _, term := range group {
			if !conditionValueEqual(row[term.column], term.expected) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func (c compiledCondition) matchesFast(values []any, columns map[string]int) bool {
	for _, group := range c {
		matches := true
		for _, term := range group {
			index, ok := columns[term.column]
			if !ok || !conditionValueEqual(values[index], term.expected) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func conditionValueEqual(actual, expected any) bool {
	switch value := expected.(type) {
	case nil:
		return actual == nil
	case string:
		actualValue, ok := actual.(string)
		return ok && actualValue == value
	case int64:
		actualValue, ok := actual.(int64)
		return ok && actualValue == value
	case float64:
		actualValue, ok := actual.(float64)
		return ok && actualValue == value
	case bool:
		actualValue, ok := actual.(bool)
		return ok && actualValue == value
	default:
		return fmt.Sprint(actual) == fmt.Sprint(expected)
	}
}
