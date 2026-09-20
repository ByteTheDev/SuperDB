package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// matchWhereValue compares a stored row value against the raw (unquoted)
// WHERE value string. It replicates the existing fmt.Sprint comparison
// exactly while fast-pathing strings and bools without formatting.
func matchWhereValue(rowValue any, wv string) bool {
	switch v := rowValue.(type) {
	case string:
		return v == wv
	case bool:
		return (v && wv == "true") || (!v && wv == "false")
	default:
		return fmt.Sprint(rowValue) == wv
	}
}

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
	c, v, _, err := parseMutationWhere(s)
	if err != nil {
		return "", ""
	}
	return c, v
}

// parseMutationWhere parses the WHERE clause of a DELETE/UPDATE statement.
// It returns hasWhere=false when no WHERE keyword is present (caller applies
// the mutation to all rows). When WHERE is present the remainder must be a
// single `column = value` equality with both sides non-empty; anything else
// (missing "=", empty column/value such as "WHERE id > 1" or bare "WHERE")
// returns an invalid-WHERE error so callers never fall back to match-all.
func parseMutationWhere(s string) (column, value string, hasWhere bool, err error) {
	i := indexKeyword(s, "WHERE")
	if i < 0 {
		return "", "", false, nil
	}
	rest := strings.TrimSpace(s[i+5:])
	if rest == "" {
		return "", "", true, errors.New("invalid WHERE expression")
	}
	// Only single-term equality is supported for mutations.
	if indexKeyword(rest, "AND") >= 0 || indexKeyword(rest, "OR") >= 0 {
		return "", "", true, errors.New("invalid WHERE expression")
	}
	p := strings.SplitN(rest, "=", 2)
	if len(p) != 2 {
		return "", "", true, errors.New("invalid WHERE expression")
	}
	column = strings.ToLower(strings.TrimSpace(p[0]))
	raw := strings.TrimSpace(p[1])
	if column == "" || raw == "" {
		return "", "", true, errors.New("invalid WHERE expression")
	}
	return column, strings.Trim(raw, "'"), true, nil
}

func splitLogical(s, operator string) []string {
	needle := " " + operator + " "
	var out []string
	start := 0
	quote := false
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i] == '\'' {
			quote = !quote
		}
		if !quote && foldEqualAt(s[i:], needle) {
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
