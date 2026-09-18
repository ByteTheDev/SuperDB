package engine

import (
	"fmt"
	"strings"
)

func New() *Database { return &Database{Tables: make(map[string]*Table)} }

// ExecBatch executes statements in order and returns one result per statement.
// Callers that need atomicity should use a transaction at the server boundary.
func (d *Database) ExecBatch(sqls []string) ([]Result, error) {
	results := make([]Result, 0, len(sqls))
	for i, sql := range sqls {
		result, err := d.Exec(sql)
		if err != nil {
			return results, fmt.Errorf("statement %d: %w", i+1, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (d *Database) Exec(sql string) (Result, error) {
	s := strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	upper := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(upper, "CREATE TABLE"):
		return d.create(s)
	case strings.HasPrefix(upper, "CREATE INDEX"):
		return d.createIndex(s)
	case strings.HasPrefix(upper, "ALTER TABLE"):
		return d.alterTable(s)
	case strings.HasPrefix(upper, "INSERT INTO"):
		return d.insert(s)
	case strings.HasPrefix(upper, "SELECT"):
		return d.selectRows(s)
	case strings.HasPrefix(upper, "UPDATE"):
		return d.update(s)
	case strings.HasPrefix(upper, "DELETE FROM"):
		return d.delete(s)
	default:
		return Result{}, fmt.Errorf("unsupported SQL")
	}
}

func (d *Database) table(name string) (*Table, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t := d.Tables[strings.ToLower(name)]
	if t == nil {
		return nil, fmt.Errorf("table not found")
	}
	return t, nil
}
