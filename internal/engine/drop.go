package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// drop handles DROP TABLE and DROP INDEX.
func (d *Database) drop(s string) (Result, error) {
	if hasPrefixFold(s, "DROP TABLE") {
		return d.dropTable(s)
	}
	return d.dropIndex(s)
}

// dropTable removes a whole table: DROP TABLE [IF EXISTS] name.
func (d *Database) dropTable(s string) (Result, error) {
	rest := strings.TrimSpace(s[len("DROP TABLE"):])
	ifExists := false
	if hasPrefixFold(rest, "IF EXISTS") && (len(rest) == len("IF EXISTS") || !identifierByte(rest[len("IF EXISTS")])) {
		ifExists = true
		rest = strings.TrimSpace(rest[len("IF EXISTS"):])
	}
	name := strings.ToLower(rest)
	if name == "" || strings.ContainsAny(name, " \t\n\r") {
		return Result{}, errors.New("invalid DROP TABLE")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.Tables[name]; !ok {
		if ifExists {
			return Result{Message: "table does not exist"}, nil
		}
		return Result{}, errors.New("table not found")
	}
	delete(d.Tables, name)
	return Result{Message: "table dropped"}, nil
}

// dropIndex removes a secondary index: DROP INDEX name ON table. The name
// resolves through the recorded CREATE INDEX names; when no name matches
// (for example an index restored from a snapshot written before names were
// tracked) the operand is also tried as a column name.
func (d *Database) dropIndex(s string) (Result, error) {
	rest := strings.TrimSpace(s[len("DROP INDEX"):])
	on := indexKeyword(rest, "ON")
	if on < 0 {
		return Result{}, errors.New("invalid DROP INDEX")
	}
	indexName := strings.ToLower(strings.TrimSpace(rest[:on]))
	tableName := strings.TrimSpace(rest[on+2:])
	if indexName == "" || tableName == "" || len(strings.Fields(indexName)) != 1 || len(strings.Fields(tableName)) != 1 {
		return Result{}, errors.New("invalid DROP INDEX")
	}
	t, err := d.table(tableName)
	if err != nil {
		return Result{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	column, ok := t.IndexNames[indexName]
	if !ok {
		if _, isColumn := t.Indexes[indexName]; isColumn {
			column = indexName
		} else {
			return Result{}, fmt.Errorf("index not found: %s", indexName)
		}
	}
	delete(t.Indexes, column)
	for name, col := range t.IndexNames {
		if col == column {
			delete(t.IndexNames, name)
		}
	}
	return Result{Message: "index dropped"}, nil
}

// dropColumn implements ALTER TABLE ... DROP COLUMN. Every stored row map
// loses the column, any secondary index on it is dropped, and the fast path
// is rebuilt because its value arrays are positional. Dropping the primary
// column is allowed: the table becomes keyless and future inserts receive
// synthetic row keys. Dropping the only column is rejected like MySQL —
// use DROP TABLE instead.
func (d *Database) dropColumn(ctx context.Context, tableName, column string) (Result, error) {
	t, err := d.table(tableName)
	if err != nil {
		return Result{}, err
	}
	column = strings.ToLower(column)
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.Columns) <= 1 {
		return Result{}, errors.New("cannot drop the only column")
	}
	index := -1
	for i, c := range t.Columns {
		if c.Name == column {
			index = i
			break
		}
	}
	if index < 0 {
		return Result{}, fmt.Errorf("column not found: %s", column)
	}
	t.Columns = append(t.Columns[:index], t.Columns[index+1:]...)
	tick := &rowTicker{}
	for _, row := range t.Rows {
		if err := tick.tick(ctx); err != nil {
			return Result{}, err
		}
		delete(row, column)
	}
	delete(t.Indexes, column)
	for name, col := range t.IndexNames {
		if col == column {
			delete(t.IndexNames, name)
		}
	}
	if len(t.fast) > 0 {
		t.rebuildFastPathLocked()
	}
	return Result{Message: "column dropped"}, nil
}

// truncate removes all rows while keeping the schema and secondary-index
// definitions: TRUNCATE [TABLE] name.
func (d *Database) truncate(ctx context.Context, s string) (Result, error) {
	rest := strings.TrimSpace(s[len("TRUNCATE"):])
	if hasPrefixFold(rest, "TABLE") && (len(rest) == len("TABLE") || !identifierByte(rest[len("TABLE")])) {
		rest = strings.TrimSpace(rest[len("TABLE"):])
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return Result{}, errors.New("invalid TRUNCATE")
	}
	t, err := d.table(fields[0])
	if err != nil {
		return Result{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Rows = make(map[string]map[string]any)
	t.Order = nil
	for column := range t.Indexes {
		t.Indexes[column] = make(map[string]map[string]struct{})
	}
	t.fast = nil
	t.columns = nil
	t.fastPos = nil
	t.dead = 0
	t.fastDead = 0
	t.autoKey = 0
	return Result{Message: "table truncated"}, nil
}
