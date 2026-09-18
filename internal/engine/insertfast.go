package engine

import (
	"errors"
	"strings"
)

// span is a substring range without allocating.
type span struct {
	start, end int
}

// parseInsertRowsFast parses the VALUES payload with the exact semantics of
// parseInsertRows (splitRows + splitVals + parseVal) but in a single pass
// over the payload bytes: no rune decoding, no intermediate substrings, and
// no per-value TrimSpace beyond what parseVal itself does.
//
// Error conditions and precedence match the original: empty payloads report
// "INSERT requires VALUES", wrong column counts report "column count
// mismatch" before any value parsing, missing keys report "primary key
// required", and duplicates report "duplicate primary key" at the same row.
func parseInsertRowsFast(raw string, columns []Column) ([]insertRow, error) {
	payload := strings.TrimSpace(raw)
	rows := []insertRow(nil)
	var seen map[string]struct{}
	var spans []span
	depth := 0
	rowStart := -1
	valStart := -1
	quote := false

	flushRow := func(end int) error {
		spans = append(spans, span{valStart, end})
		if len(spans) != len(columns) {
			return errors.New("column count mismatch")
		}
		row := make(map[string]any, len(columns))
		key := ""
		for i, c := range columns {
			v, err := parseVal(payload[spans[i].start:spans[i].end], c.Type)
			if err != nil {
				return err
			}
			row[c.Name] = v
			if c.Primary {
				key = valueKey(v)
			}
		}
		if key == "" {
			return errors.New("primary key required")
		}
		if len(rows) >= 1 {
			if seen == nil {
				seen = make(map[string]struct{}, 4)
				for _, previous := range rows {
					seen[previous.key] = struct{}{}
				}
			}
			if _, dup := seen[key]; dup {
				return errors.New("duplicate primary key")
			}
			seen[key] = struct{}{}
		}
		rows = append(rows, insertRow{key: key, row: row})
		return nil
	}

	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if c == '\'' {
			quote = !quote
			continue
		}
		if quote {
			continue
		}
		switch c {
		case '(':
			if depth == 0 {
				rowStart = i + 1
				valStart = i + 1
				spans = spans[:0]
			}
			depth++
		case ')':
			depth--
			if depth == 0 && rowStart >= 0 {
				if err := flushRow(i); err != nil {
					return nil, err
				}
				rowStart = -1
			}
		case ',':
			if rowStart >= 0 && depth >= 1 {
				spans = append(spans, span{valStart, i})
				valStart = i + 1
			}
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("INSERT requires VALUES")
	}
	return rows, nil
}
