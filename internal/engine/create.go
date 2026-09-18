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
	for _, part := range strings.Split(s[a+1:z], ",") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) < 2 {
			return Result{}, errors.New("invalid column")
		}
		c := Column{Name: strings.ToLower(f[0]), Type: Type(strings.ToUpper(f[1]))}
		for _, x := range f[2:] {
			if strings.EqualFold(x, "PRIMARY") {
				c.Primary = true
			}
		}
		cols = append(cols, c)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.Tables[name]; ok {
		return Result{}, errors.New("table exists")
	}
	d.Tables[name] = &Table{Name: name, Columns: cols, Rows: map[string]map[string]any{}}
	return Result{Message: "table created"}, nil
}
