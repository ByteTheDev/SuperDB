package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Comparison operators supported in WHERE terms.
const (
	opEq uint8 = iota // =
	opNe              // != or <>
	opLt              // <
	opLe              // <=
	opGt              // >
	opGe              // >=
)

// matchWhereValue compares a stored row value against the raw (unquoted)
// WHERE value string under opEq semantics. It replicates the historical
// fmt.Sprint comparison exactly while fast-pathing strings and bools
// without formatting.
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

// matchWhereOp is the mutation-path equivalent of conditionValueCompare.
// Equality keeps matchWhereValue's raw-string semantics; ordered operators
// compare typed values like ORDER BY does, so an INT column compares
// numerically rather than lexicographically.
func matchWhereOp(rowValue any, wv string, expected any, op uint8) bool {
	switch op {
	case opEq:
		return matchWhereValue(rowValue, wv)
	case opNe:
		return !matchWhereValue(rowValue, wv)
	}
	return orderedCompare(rowValue, expected, op)
}

// orderedCompare evaluates < <= > >= against the same ordering rules used
// by ORDER BY and MIN/MAX (valueLess): integers compare as int64, mixed
// numerics as float64, and anything else by rendered string.
func orderedCompare(actual, expected any, op uint8) bool {
	lt := valueLess(actual, expected)
	switch op {
	case opLt:
		return lt
	case opLe:
		return !valueLess(expected, actual)
	case opGt:
		return valueLess(expected, actual)
	case opGe:
		return !lt
	default:
		return false
	}
}

// unquoteLiteral reduces a raw SQL literal to its text value: surrounding
// single quotes are removed and ” inside a quoted literal collapses to '.
// Unbalanced or unquoted input keeps the historical strings.Trim behavior.
func unquoteLiteral(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return strings.Trim(s, "'")
}

// skipQuoted advances i when s[i] opens a ” escape inside a literal so
// quote-tracking scanners treat ” as one escaped quote, matching
// indexKeyword and BindParams. It reports whether the caller should skip
// the usual quote toggle.
func skipQuoted(s string, i int, quoted bool) (int, bool) {
	if quoted && i+1 < len(s) && s[i+1] == '\'' {
		return i + 1, true
	}
	return i, false
}

func splitVals(s string) []string {
	var out []string
	start := 0
	quote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			if ni, skip := skipQuoted(s, i, quote); skip {
				i = ni
				continue
			}
			quote = !quote
		}
		if c == ',' && !quote {
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
		return unquoteLiteral(raw), nil
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

// parseMutationWhere parses the WHERE clause of a DELETE/UPDATE statement.
// It returns hasWhere=false when no WHERE keyword is present (caller applies
// the mutation to all rows). When WHERE is present the remainder must be a
// single `column <op> value` predicate with both sides non-empty; anything
// else (missing operator, empty column/value such as "WHERE id >" or bare
// "WHERE") returns an invalid-WHERE error so callers never fall back to
// match-all.
func parseMutationWhere(s string) (column, value string, op uint8, hasWhere bool, err error) {
	i := indexKeyword(s, "WHERE")
	if i < 0 {
		return "", "", opEq, false, nil
	}
	rest := strings.TrimSpace(s[i+5:])
	if rest == "" {
		return "", "", opEq, true, errors.New("invalid WHERE expression")
	}
	// Only single-term predicates are supported for mutations.
	if indexKeyword(rest, "AND") >= 0 || indexKeyword(rest, "OR") >= 0 {
		return "", "", opEq, true, errors.New("invalid WHERE expression")
	}
	lhs, op, rhs, ok := splitConditionTerm(rest)
	if !ok {
		return "", "", opEq, true, errors.New("invalid WHERE expression")
	}
	column = strings.ToLower(strings.TrimSpace(lhs))
	raw := strings.TrimSpace(rhs)
	if column == "" || raw == "" {
		return "", "", opEq, true, errors.New("invalid WHERE expression")
	}
	return column, unquoteLiteral(raw), op, true, nil
}

func splitLogical(s, operator string) []string {
	needle := " " + operator + " "
	var out []string
	start := 0
	quote := false
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i] == '\'' {
			if ni, skip := skipQuoted(s, i, quote); skip {
				i = ni
				continue
			}
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

// splitConditionTerm divides one predicate into column, operator, and raw
// value. The operator is the first comparison token found outside quoted
// text: =, !=, <>, <, <=, >, or >=. Anything without an operator reports
// ok=false so callers reject instead of silently mismatching.
func splitConditionTerm(s string) (lhs string, op uint8, rhs string, ok bool) {
	quote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
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
		switch c {
		case '!', '<':
			if i+1 < len(s) && s[i+1] == '=' {
				return s[:i], opNeOrLe(c), s[i+2:], true
			}
			if c == '<' {
				if i+1 < len(s) && s[i+1] == '>' {
					return s[:i], opNe, s[i+2:], true
				}
				return s[:i], opLt, s[i+1:], true
			}
			// A bare '!' is malformed.
			return "", 0, "", false
		case '>':
			if i+1 < len(s) && s[i+1] == '=' {
				return s[:i], opGe, s[i+2:], true
			}
			return s[:i], opGt, s[i+1:], true
		case '=':
			return s[:i], opEq, s[i+1:], true
		}
	}
	return "", 0, "", false
}

func opNeOrLe(c byte) uint8 {
	if c == '!' {
		return opNe
	}
	return opLe
}

type conditionTerm struct {
	column   string
	expected any
	op       uint8
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
			lhs, op, rhs, ok := splitConditionTerm(conjunction)
			if !ok {
				return nil, false
			}
			column := strings.ToLower(strings.TrimSpace(lhs))
			if column == "" {
				return nil, false
			}
			typ, exists := types[column]
			if !exists {
				// Comparisons on a missing column can never be satisfied
				// reliably: equality silently mismatches and != silently
				// matches every row. Reject rather than guess.
				return nil, false
			}
			raw := strings.TrimSpace(rhs)
			if raw == "" {
				return nil, false
			}
			expected := any(unquoteLiteral(raw))
			// parseVal on the unquoted text keeps `id >= '5'` numeric and
			// consistent with the mutation path; when the literal does not
			// convert the raw string stays, preserving historical
			// type-mismatch no-match semantics.
			if value, err := parseVal(unquoteLiteral(raw), typ); err == nil {
				expected = value
			}
			group = append(group, conditionTerm{column: column, expected: expected, op: op})
		}
		compiled = append(compiled, group)
	}
	return compiled, true
}

func (c compiledCondition) matchesMap(row map[string]any) bool {
	for _, group := range c {
		matches := true
		for _, term := range group {
			if !conditionValueCompare(row[term.column], term.expected, term.op) {
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
			if !ok || !conditionValueCompare(values[index], term.expected, term.op) {
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

func conditionValueCompare(actual, expected any, op uint8) bool {
	switch op {
	case opEq:
		return conditionValueEqual(actual, expected)
	case opNe:
		return !conditionValueEqual(actual, expected)
	default:
		return orderedCompare(actual, expected, op)
	}
}
