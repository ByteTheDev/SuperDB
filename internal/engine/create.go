package engine

import (
	"errors"
	"strings"
)

func (d *Database) create(s string) (Result, error) {
	a := strings.Index(s, "(")
	z := strings.LastIndex(s, ")")
	if a < 0 || z < a {
		return Result{}, errors.New("invalid CREATE TABLE")
	}
	head := strings.Fields(s[:a])
	if len(head) < 3 {
		return Result{}, errors.New("invalid table name")
	}
	name := strings.ToLower(head[2])
	cols := []Column{}
	seen := make(map[string]struct{}, 8)
	primaryCount := 0
	for _, part := range strings.Split(s[a+1:z], ",") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) < 2 {
			return Result{}, errors.New("invalid column")
		}
		colName := strings.ToLower(f[0])
		if _, dup := seen[colName]; dup {
			return Result{}, errors.New("duplicate column")
		}
		seen[colName] = struct{}{}
		colType := Type(strings.ToUpper(f[1]))
		switch colType {
		case Int, Float, Text, Bool:
		default:
			return Result{}, errors.New("invalid column type")
		}
		c := Column{Name: colName, Type: colType}
		for _, x := range f[2:] {
			if strings.EqualFold(x, "PRIMARY") {
				c.Primary = true
			}
		}
		if c.Primary {
			primaryCount++
			if primaryCount > 1 {
				return Result{}, errors.New("multiple primary keys")
			}
		}
		cols = append(cols, c)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.Tables[name]; ok {
		return Result{}, errors.New("table exists")
	}
	d.Tables[name] = &Table{Name: name, Columns: cols, Rows: map[string]map[string]any{}, Indexes: make(map[string]map[string]map[string]struct{})}
	return Result{Message: "table created"}, nil
}
