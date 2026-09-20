package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// BindParams substitutes "?" placeholders in sql with safely-escaped
// literals derived from args. Placeholders inside single-quoted string
// literals are ignored. Returns an error when the placeholder count does
// not match len(args).
//
// This is the single shared binding implementation used by the hosted
// network server: remote clients send SQL + params separately, and the
// server binds here before calling Exec. It is NOT naive string
// concatenation: strings are quoted with ” escaping, and every other
// type is rendered from its typed value.
func BindParams(sql string, args []any) (string, error) {
	if len(args) == 0 {
		if hasPlaceholder(sql) {
			return "", fmt.Errorf("missing params: query has placeholders but no args")
		}
		return sql, nil
	}
	var out strings.Builder
	out.Grow(len(sql) + len(args)*8)
	arg := 0
	inQuote := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if c == '\'' {
			// '' inside a literal is an escaped quote, not a toggle.
			if inQuote && i+1 < len(sql) && sql[i+1] == '\'' {
				out.WriteString("''")
				i++
				continue
			}
			inQuote = !inQuote
			out.WriteByte(c)
			continue
		}
		if c == '?' && !inQuote {
			if arg >= len(args) {
				return "", fmt.Errorf("too few params: placeholder %d has no value", arg+1)
			}
			out.WriteString(formatParam(args[arg]))
			arg++
			continue
		}
		out.WriteByte(c)
	}
	if arg != len(args) {
		return "", fmt.Errorf("too many params: have %d args for %d placeholders", len(args), arg)
	}
	return out.String(), nil
}

func hasPlaceholder(sql string) bool {
	inQuote := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if c == '\'' {
			if inQuote && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inQuote = !inQuote
			continue
		}
		if c == '?' && !inQuote {
			return true
		}
	}
	return false
}

// PlaceholderCount reports the number of bindable "?" markers outside
// string literals.
func PlaceholderCount(sql string) int {
	n := 0
	inQuote := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if c == '\'' {
			if inQuote && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inQuote = !inQuote
			continue
		}
		if c == '?' && !inQuote {
			n++
		}
	}
	return n
}

func formatParam(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.FormatInt(int64(t), 10)
	case int8:
		return strconv.FormatInt(int64(t), 10)
	case int16:
		return strconv.FormatInt(int64(t), 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint:
		return strconv.FormatUint(uint64(t), 10)
	case uint8:
		return strconv.FormatUint(uint64(t), 10)
	case uint16:
		return strconv.FormatUint(uint64(t), 10)
	case uint32:
		return strconv.FormatUint(uint64(t), 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 64)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []byte:
		return "'" + strings.ReplaceAll(string(t), "'", "''") + "'"
	default:
		s := fmt.Sprint(v)
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
}
