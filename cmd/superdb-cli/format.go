package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// cliResult is the decoded shape of one server response: either an engine
// Result or an {"error": "..."} object. Numbers stay as json.Number so
// values print exactly as stored.
type cliResult struct {
	Columns  []string `json:"columns"`
	Rows     [][]any  `json:"rows"`
	Affected int      `json:"affected"`
	Message  string   `json:"message"`
	Error    string   `json:"error"`
	Status   string   `json:"status"`
}

func decodeResult(raw []byte) (cliResult, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var r cliResult
	if err := dec.Decode(&r); err != nil {
		return cliResult{}, err
	}
	return r, nil
}

// printResult renders one response. json format prints the raw response
// unchanged (the historical CLI contract); table format renders a readable
// table for row results and short status lines otherwise. Query errors go
// to stderr in table mode so piped stdout stays clean.
func printResult(raw []byte, format string, timing string) {
	if format == "json" {
		fmt.Fprintln(stdout, string(raw))
		if timing != "" {
			fmt.Fprintf(stderr, "Time: %s\n", timing)
		}
		return
	}
	r, err := decodeResult(raw)
	if err != nil {
		fmt.Fprintln(stdout, string(raw))
		return
	}
	if r.Error != "" {
		fmt.Fprintf(stderr, "error: %s\n", r.Error)
	} else if len(r.Columns) > 0 {
		printTable(stdout, r.Columns, r.Rows)
	} else if r.Message != "" {
		fmt.Fprintln(stdout, r.Message)
	} else if r.Affected != 0 {
		fmt.Fprintf(stdout, "%d row(s) affected\n", r.Affected)
	} else {
		fmt.Fprintln(stdout, "OK")
	}
	if timing != "" {
		fmt.Fprintf(stderr, "Time: %s\n", timing)
	}
}

// printTable renders columns+rows as an aligned text table:
//
//	 id | name  | score
//	----+-------+------
//	  1 | Ada   |    10
//
// Numeric columns right-align; everything else left-aligns. NULL values
// print as NULL.
func printTable(w io.Writer, columns []string, rows [][]any) {
	widths := make([]int, len(columns))
	numeric := make([]bool, len(columns))
	for i, c := range columns {
		widths[i] = len(c)
		numeric[i] = true
	}
	cells := make([][]string, len(rows))
	for i, row := range rows {
		cells[i] = make([]string, len(columns))
		for j := range columns {
			var v string
			if j < len(row) {
				v = formatValue(row[j])
			} else {
				v = "NULL"
			}
			cells[i][j] = v
			if len(v) > widths[j] {
				widths[j] = len(v)
			}
			if !isNumericCell(row, j) {
				numeric[j] = false
			}
		}
	}
	writeRow := func(vals []string, header bool) {
		for i, v := range vals {
			if i > 0 {
				if header {
					fmt.Fprint(w, "-+-")
				} else {
					fmt.Fprint(w, " | ")
				}
			}
			pad := widths[i] - len(v)
			if !header && numeric[i] {
				fmt.Fprintf(w, "%s%s", strings.Repeat(" ", pad), v)
			} else {
				fmt.Fprintf(w, "%s%s", v, strings.Repeat(" ", pad))
			}
		}
		fmt.Fprintln(w)
	}
	writeRow(columns, true)
	for _, row := range cells {
		writeRow(row, false)
	}
	fmt.Fprintf(w, "(%d row(s))\n", len(rows))
}

func isNumericCell(row []any, j int) bool {
	if j >= len(row) || row[j] == nil {
		return true
	}
	_, ok := row[j].(json.Number)
	return ok
}

// formatValue renders one decoded cell. json.Number keeps its exact text
// so integers never degrade to float formatting.
func formatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case json.Number:
		return t.String()
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(t)
	}
}

// splitStatements divides text into statements at semicolons outside
// single-quoted literals (” escapes a quote). When final is true a
// trailing fragment without a terminator still counts as a statement;
// otherwise it is returned as rest for the caller to keep buffering.
func splitStatements(text string, final bool) (stmts []string, rest string) {
	quote := false
	start := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\'' {
			if quote && i+1 < len(text) && text[i+1] == '\'' {
				i++
				continue
			}
			quote = !quote
			continue
		}
		if c == ';' && !quote {
			if s := strings.TrimSpace(text[start:i]); s != "" {
				stmts = append(stmts, s)
			}
			start = i + 1
		}
	}
	rest = text[start:]
	if final {
		if s := strings.TrimSpace(rest); s != "" {
			stmts = append(stmts, s)
		}
		return stmts, ""
	}
	return stmts, rest
}
