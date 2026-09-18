package engine

import "sync"

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
	Indexes map[string]map[string]map[string]struct{}
	mu      sync.RWMutex
	fast    []fastRow
	columns map[string]int
	fastPos map[string]int
	// dead counts stale keys left in Order by primary-key deletes and
	// fastDead counts tombstoned entries in fast. Both are reaped by
	// compaction once they reach a quarter of their slice. They are
	// lock-guarded by mu and intentionally unexported so snapshots,
	// clones, and the wire format are unchanged.
	dead     int
	fastDead int
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

func (t *Table) primaryColumn() string {
	for _, c := range t.Columns {
		if c.Primary {
			return c.Name
		}
	}
	return ""
}
