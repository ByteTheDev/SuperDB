package engine

import (
	"fmt"
	"strings"
)

func New() *Database { return &Database{Tables: make(map[string]*Table)} }

func (d *Database) Exec(sql string) (Result, error) {
	s := strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	upper := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(upper, "CREATE TABLE"):
		return d.create(s)
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
