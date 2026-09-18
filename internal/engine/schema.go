package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func (d *Database) createIndex(s string) (Result, error) {
	upper := strings.ToUpper(s)
	on := strings.Index(upper, " ON ")
	open, close := strings.Index(s, "("), strings.LastIndex(s, ")")
	if on < 0 || open < 0 || close <= open {
		return Result{}, errors.New("invalid CREATE INDEX")
	}
	indexName := strings.TrimSpace(s[len("CREATE INDEX"):on])
	tableName := strings.TrimSpace(s[on+4 : open])
	column := strings.ToLower(strings.TrimSpace(s[open+1 : close]))
	if indexName == "" || tableName == "" || column == "" || strings.Contains(column, ",") {
		return Result{}, errors.New("invalid CREATE INDEX")
	}
	t, err := d.table(tableName)
	if err != nil {
		return Result{}, err
	}
	if !hasColumn(t.Columns, column) {
		return Result{}, fmt.Errorf("column not found: %s", column)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Indexes == nil {
		t.Indexes = make(map[string]map[string]map[string]struct{})
	}
	if _, exists := t.Indexes[column]; exists {
		return Result{}, errors.New("index already exists for column")
	}
	index := make(map[string]map[string]struct{})
	for key, row := range t.Rows {
		value := valueKey(row[column])
		if index[value] == nil {
			index[value] = make(map[string]struct{})
		}
		index[value][key] = struct{}{}
	}
	t.Indexes[column] = index
	return Result{Message: "index created"}, nil
}

func valueKey(value any) string {
	switch v := value.(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case nil:
		return "NULL"
	default:
		return fmt.Sprint(v)
	}
}

func (d *Database) alterTable(s string) (Result, error) {
	parts := strings.Fields(s)
	if len(parts) != 7 || !strings.EqualFold(parts[3], "ADD") || !strings.EqualFold(parts[4], "COLUMN") {
		return Result{}, errors.New("only ALTER TABLE ... ADD COLUMN is supported")
	}
	t, err := d.table(parts[2])
	if err != nil {
		return Result{}, err
	}
	column := strings.ToLower(parts[5])
	typ := Type(strings.ToUpper(parts[6]))
	if column == "" || (typ != Int && typ != Float && typ != Text && typ != Bool) {
		return Result{}, errors.New("invalid column name or type")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if hasColumn(t.Columns, column) {
		return Result{}, errors.New("column already exists")
	}
	t.Columns = append(t.Columns, Column{Name: column, Type: typ})
	for _, row := range t.Rows {
		row[column] = nil
	}
	if len(t.fast) > 0 {
		t.rebuildFastPathLocked()
	}
	return Result{Message: "column added"}, nil
}

func hasColumn(columns []Column, name string) bool {
	for _, column := range columns {
		if column.Name == name {
			return true
		}
	}
	return false
}

func (t *Table) addIndexValue(column, value, primary string) {
	if t.Indexes == nil || t.Indexes[column] == nil {
		return
	}
	if t.Indexes[column][value] == nil {
		t.Indexes[column][value] = make(map[string]struct{})
	}
	t.Indexes[column][value][primary] = struct{}{}
}

func (t *Table) removeIndexValue(column, value, primary string) {
	if t.Indexes == nil || t.Indexes[column] == nil {
		return
	}
	delete(t.Indexes[column][value], primary)
	if len(t.Indexes[column][value]) == 0 {
		delete(t.Indexes[column], value)
	}
}
