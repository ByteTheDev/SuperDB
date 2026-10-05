package engine

import (
	"errors"
	"sort"
	"strings"
)

// meta handles read-only introspection statements: SHOW TABLES, SHOW
// COLUMNS FROM t, SHOW INDEX[ES] FROM t, and DESCRIBE t (alias DESC t).
func (d *Database) meta(s string) (Result, error) {
	if hasPrefixFold(s, "SHOW") {
		return d.show(s)
	}
	return d.describe(s)
}

func (d *Database) show(s string) (Result, error) {
	rest := strings.TrimSpace(s[len("SHOW"):])
	switch {
	case hasPrefixFold(rest, "TABLES") && (len(rest) == len("TABLES") || !identifierByte(rest[len("TABLES")])):
		if strings.TrimSpace(rest[len("TABLES"):]) != "" {
			return Result{}, errors.New("invalid SHOW TABLES")
		}
		d.mu.RLock()
		defer d.mu.RUnlock()
		names := make([]string, 0, len(d.Tables))
		for name := range d.Tables {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := make([][]any, 0, len(names))
		for _, name := range names {
			rows = append(rows, []any{name})
		}
		return Result{Columns: []string{"table"}, Rows: rows}, nil
	case hasPrefixFold(rest, "COLUMNS") && (len(rest) == len("COLUMNS") || !identifierByte(rest[len("COLUMNS")])):
		return d.describeFrom(rest[len("COLUMNS"):])
	case hasPrefixFold(rest, "INDEXES") && (len(rest) == len("INDEXES") || !identifierByte(rest[len("INDEXES")])):
		return d.showIndexes(rest[len("INDEXES"):])
	case hasPrefixFold(rest, "INDEX") && (len(rest) == len("INDEX") || !identifierByte(rest[len("INDEX")])):
		return d.showIndexes(rest[len("INDEX"):])
	default:
		return Result{}, errors.New("unsupported SHOW")
	}
}

// describe implements DESCRIBE t and its DESC t alias. Both keywords
// require a blank boundary so prefixes like "DESCT" cannot smuggle a
// table name into the statement.
func (d *Database) describe(s string) (Result, error) {
	var rest string
	switch {
	case hasPrefixFold(s, "DESCRIBE") && (len(s) == len("DESCRIBE") || isBlankByte(s[len("DESCRIBE")])):
		rest = strings.TrimSpace(s[len("DESCRIBE"):])
	case hasPrefixFold(s, "DESC") && (len(s) == len("DESC") || isBlankByte(s[len("DESC")])):
		rest = strings.TrimSpace(s[len("DESC"):])
	default:
		return Result{}, errors.New("invalid DESCRIBE")
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return Result{}, errors.New("invalid DESCRIBE")
	}
	return d.describeTable(fields[0])
}

// describeFrom parses the trailing "FROM t" of SHOW COLUMNS.
func (d *Database) describeFrom(rest string) (Result, error) {
	fields := strings.Fields(rest)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "FROM") {
		return Result{}, errors.New("invalid SHOW COLUMNS")
	}
	return d.describeTable(fields[1])
}

func (d *Database) describeTable(name string) (Result, error) {
	t, err := d.table(name)
	if err != nil {
		return Result{}, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	rows := make([][]any, 0, len(t.Columns))
	for _, c := range t.Columns {
		rows = append(rows, []any{c.Name, string(c.Type), c.Primary})
	}
	return Result{Columns: []string{"name", "type", "primary"}, Rows: rows}, nil
}

// showIndexes lists a table's secondary indexes: SHOW INDEX[ES] FROM t.
// The reported name is the CREATE INDEX name when it was recorded, or the
// column name for indexes restored from older snapshots.
func (d *Database) showIndexes(rest string) (Result, error) {
	fields := strings.Fields(rest)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "FROM") {
		return Result{}, errors.New("invalid SHOW INDEX")
	}
	t, err := d.table(fields[1])
	if err != nil {
		return Result{}, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	names := make(map[string]string, len(t.Indexes))
	for name, column := range t.IndexNames {
		names[column] = name
	}
	columns := make([]string, 0, len(t.Indexes))
	for column := range t.Indexes {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	rows := make([][]any, 0, len(columns))
	for _, column := range columns {
		name := names[column]
		if name == "" {
			name = column
		}
		rows = append(rows, []any{name, column})
	}
	return Result{Columns: []string{"name", "column"}, Rows: rows}, nil
}
