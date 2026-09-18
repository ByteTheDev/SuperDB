package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

type Type string

const (
	Int   Type = "INT"
	Float Type = "FLOAT"
	Text  Type = "TEXT"
	Bool  Type = "BOOL"
)

type Column struct {
	Name    string
	Type    Type
	Primary bool
}
type Table struct {
	Name    string
	Columns []Column
	Rows    map[string]map[string]any
	Order   []string
	mu      sync.RWMutex
}
type Database struct {
	Tables map[string]*Table
	mu     sync.RWMutex
}
type Result struct {
	Columns  []string `json:"columns,omitempty"`
	Rows     [][]any  `json:"rows,omitempty"`
	Affected int      `json:"affected,omitempty"`
	Message  string   `json:"message,omitempty"`
}

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
func splitVals(s string) []string {
	var out []string
	start := 0
	quote := false
	for i, r := range s {
		if r == '\'' {
			quote = !quote
		}
		if r == ',' && !quote {
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}
func parseVal(raw string, typ Type) (any, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "NULL") {
		return nil, nil
	}
	if typ == Text {
		return strings.Trim(raw, "'"), nil
	}
	switch typ {
	case Int:
		return strconv.ParseInt(raw, 10, 64)
	case Float:
		return strconv.ParseFloat(raw, 64)
	case Bool:
		return strconv.ParseBool(raw)
	}
	return nil, errors.New("unknown type")
}
func (d *Database) table(name string) (*Table, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t := d.Tables[strings.ToLower(name)]
	if t == nil {
		return nil, errors.New("table not found")
	}
	return t, nil
}
func (d *Database) insert(s string) (Result, error) {
	u := strings.ToUpper(s)
	vi := strings.Index(u, "VALUES")
	if vi < 0 {
		return Result{}, errors.New("INSERT requires VALUES")
	}
	h := strings.Fields(s[:vi])
	if len(h) < 3 {
		return Result{}, errors.New("invalid INSERT")
	}
	t, e := d.table(h[2])
	if e != nil {
		return Result{}, e
	}
	raw := strings.TrimSpace(s[vi+6:])
	raw = strings.Trim(raw, "() ")
	vals := splitVals(raw)
	if len(vals) != len(t.Columns) {
		return Result{}, errors.New("column count mismatch")
	}
	row := map[string]any{}
	key := ""
	for i, c := range t.Columns {
		v, e := parseVal(vals[i], c.Type)
		if e != nil {
			return Result{}, e
		}
		row[c.Name] = v
		if c.Primary {
			key = fmt.Sprint(v)
		}
	}
	if key == "" {
		return Result{}, errors.New("primary key required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.Rows[key]; ok {
		return Result{}, errors.New("duplicate primary key")
	}
	t.Rows[key] = row
	t.Order = append(t.Order, key)
	return Result{Affected: 1}, nil
}
func (d *Database) selectRows(s string) (Result, error) {
	u := strings.ToUpper(s)
	fi := strings.Index(u, "FROM")
	if fi < 0 {
		return Result{}, errors.New("SELECT requires FROM")
	}
	rest := strings.TrimSpace(s[fi+4:])
	parts := strings.Fields(rest)
	if len(parts) < 1 {
		return Result{}, errors.New("table required")
	}
	t, e := d.table(parts[0])
	if e != nil {
		return Result{}, e
	}
	names := []string{}
	sel := strings.TrimSpace(s[6:fi])
	if sel == "*" {
		for _, c := range t.Columns {
			names = append(names, c.Name)
		}
	} else {
		for _, x := range strings.Split(sel, ",") {
			names = append(names, strings.ToLower(strings.TrimSpace(x)))
		}
	}
	whereCol, whereVal := filter(rest)
	t.mu.RLock()
	defer t.mu.RUnlock()
	r := Result{Columns: names}
	for _, k := range t.Order {
		row := t.Rows[k]
		if whereCol != "" && fmt.Sprint(row[whereCol]) != whereVal {
			continue
		}
		line := []any{}
		for _, n := range names {
			line = append(line, row[n])
		}
		r.Rows = append(r.Rows, line)
	}
	return r, nil
}
func filter(s string) (string, string) {
	u := strings.ToUpper(s)
	i := strings.Index(u, "WHERE")
	if i < 0 {
		return "", ""
	}
	p := strings.SplitN(strings.TrimSpace(s[i+5:]), "=", 2)
	if len(p) != 2 {
		return "", ""
	}
	return strings.ToLower(strings.TrimSpace(p[0])), strings.Trim(strings.TrimSpace(p[1]), "'")
}
func (d *Database) update(s string) (Result, error) {
	u := strings.ToUpper(s)
	si := strings.Index(u, "SET")
	if si < 0 {
		return Result{}, errors.New("UPDATE requires SET")
	}
	head := strings.Fields(s[:si])
	if len(head) < 2 {
		return Result{}, errors.New("table required")
	}
	t, e := d.table(head[1])
	if e != nil {
		return Result{}, e
	}
	tail := s[si+3:]
	wi := strings.Index(strings.ToUpper(tail), "WHERE")
	assign := strings.TrimSpace(tail)
	where := ""
	if wi >= 0 {
		assign = strings.TrimSpace(tail[:wi])
		where = strings.TrimSpace(tail[wi+5:])
	}
	ap := strings.SplitN(assign, "=", 2)
	if len(ap) != 2 {
		return Result{}, errors.New("invalid SET")
	}
	col := strings.ToLower(strings.TrimSpace(ap[0]))
	var typ Type
	for _, c := range t.Columns {
		if c.Name == col {
			typ = c.Type
		}
	}
	v, e := parseVal(ap[1], typ)
	if e != nil {
		return Result{}, e
	}
	wc, wv := "", ""
	if where != "" {
		p := strings.SplitN(where, "=", 2)
		if len(p) == 2 {
			wc = strings.ToLower(strings.TrimSpace(p[0]))
			wv = strings.Trim(strings.TrimSpace(p[1]), "'")
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, row := range t.Rows {
		if wc != "" && fmt.Sprint(row[wc]) != wv {
			continue
		}
		row[col] = v
		n++
	}
	return Result{Affected: n}, nil
}
func (d *Database) delete(s string) (Result, error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return Result{}, errors.New("invalid DELETE")
	}
	t, e := d.table(f[2])
	if e != nil {
		return Result{}, e
	}
	c, v := filter(s)
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for k, row := range t.Rows {
		if c == "" || fmt.Sprint(row[c]) == v {
			delete(t.Rows, k)
			n++
		}
	}
	return Result{Affected: n}, nil
}
func (d *Database) Snapshot() ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return json.Marshal(d)
}
