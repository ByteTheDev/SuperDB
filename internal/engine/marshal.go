package engine

import (
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// MarshalJSON encodes a Result with output byte-identical to encoding/json's
// default struct encoding (field order Columns, Rows, Affected, Message with
// omitempty), but without reflection over rows. Row values produced by the
// engine are int64, float64, string, bool, or nil; anything else falls back
// to encoding/json so behavior stays exact.
func (r Result) MarshalJSON() ([]byte, error) {
	return AppendResultJSON(nil, r)
}

// AppendResultJSON appends the JSON encoding of r to buf, identical to
// MarshalJSON. It lets callers encode batches of results into one shared
// buffer instead of allocating per result.
func AppendResultJSON(buf []byte, r Result) ([]byte, error) {
	out := buf
	out = append(out, '{')
	needComma := false
	if len(r.Columns) > 0 {
		out = append(out, `"columns":`...)
		out = appendJSONStringArray(out, r.Columns)
		needComma = true
	}
	if len(r.Rows) > 0 {
		if needComma {
			out = append(out, ',')
		}
		needComma = true
		out = append(out, `"rows":`...)
		out = append(out, '[')
		for i, row := range r.Rows {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, '[')
			for j, v := range row {
				if j > 0 {
					out = append(out, ',')
				}
				var err error
				out, err = appendJSONValue(out, v)
				if err != nil {
					return nil, err
				}
			}
			out = append(out, ']')
		}
		out = append(out, ']')
	}
	if r.Affected != 0 {
		if needComma {
			out = append(out, ',')
		}
		needComma = true
		out = append(out, `"affected":`...)
		out = strconv.AppendInt(out, int64(r.Affected), 10)
	}
	if r.Message != "" {
		if needComma {
			out = append(out, ',')
		}
		out = append(out, `"message":`...)
		out = appendJSONString(out, r.Message)
	}
	out = append(out, '}')
	return out, nil
}

func appendJSONStringArray(out []byte, values []string) []byte {
	out = append(out, '[')
	for i, s := range values {
		if i > 0 {
			out = append(out, ',')
		}
		out = appendJSONString(out, s)
	}
	return append(out, ']')
}

func appendJSONValue(out []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return append(out, "null"...), nil
	case string:
		return appendJSONString(out, t), nil
	case bool:
		if t {
			return append(out, "true"...), nil
		}
		return append(out, "false"...), nil
	case int64:
		return strconv.AppendInt(out, t, 10), nil
	case int:
		return strconv.AppendInt(out, int64(t), 10), nil
	case int32:
		return strconv.AppendInt(out, int64(t), 10), nil
	case float64:
		return appendJSONFloat(out, t)
	default:
		// Rare or foreign types (including float32): defer to the standard
		// library so output and errors stay identical.
		b, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		return append(out, b...), nil
	}
}

func appendJSONFloat(out []byte, f float64) ([]byte, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	return append(out, b...), nil
}

const lowerHex = "0123456789abcdef"

// appendJSONString appends s quoted with the exact escaping of
// encoding/json (including HTML escaping of <, >, and &).
func appendJSONString(out []byte, s string) []byte {
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			out = append(out, `\"`...)
		case c == '\\':
			out = append(out, `\\`...)
		case c == '\n':
			out = append(out, `\n`...)
		case c == '\r':
			out = append(out, `\r`...)
		case c == '\t':
			out = append(out, `\t`...)
		case c == '<':
			out = append(out, `\u003c`...)
		case c == '>':
			out = append(out, `\u003e`...)
		case c == '&':
			out = append(out, `\u0026`...)
		case c < 0x20:
			out = append(out, `\u00`...)
			out = append(out, lowerHex[c>>4], lowerHex[c&0xf])
		case c < 0x80:
			out = append(out, c)
		default:
			// Multibyte UTF-8 passes through; invalid bytes become U+FFFD
			// exactly like encoding/json.
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size <= 1 {
				out = append(out, "\uFFFD"...)
				continue
			}
			out = append(out, s[i:i+size]...)
			i += size - 1
		}
	}
	return append(out, '"')
}
