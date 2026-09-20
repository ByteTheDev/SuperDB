package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type databaseImage struct{ Tables map[string]*Table }

// MarshalJSON provides a consistent image for snapshots and live backups.
// Tables are deep-copied under their read locks so the JSON encoding runs
// without holding database locks and concurrent mutations cannot race the
// marshal (which would otherwise risk concurrent map access panics).
func (d *Database) MarshalJSON() ([]byte, error) {
	// gate.Lock excludes concurrent Exec/Clone/replace publishers while the
	// image is copied; the JSON encoding itself runs lock-free afterwards.
	d.gate.Lock()
	d.mu.RLock()
	tables := make(map[string]*Table, len(d.Tables))
	for name, t := range d.Tables {
		t.mu.RLock()
		cp := &Table{
			Name:    t.Name,
			Columns: append([]Column(nil), t.Columns...),
			Rows:    make(map[string]map[string]any, len(t.Rows)),
			Order:   append([]string(nil), t.Order...),
			Indexes: make(map[string]map[string]map[string]struct{}, len(t.Indexes)),
		}
		for key, row := range t.Rows {
			rcp := make(map[string]any, len(row))
			for k, v := range row {
				rcp[k] = v
			}
			cp.Rows[key] = rcp
		}
		for column, idx := range t.Indexes {
			icp := make(map[string]map[string]struct{}, len(idx))
			for val, members := range idx {
				mcp := make(map[string]struct{}, len(members))
				for m := range members {
					mcp[m] = struct{}{}
				}
				icp[val] = mcp
			}
			cp.Indexes[column] = icp
		}
		t.mu.RUnlock()
		tables[name] = cp
	}
	d.mu.RUnlock()
	d.gate.Unlock()
	return json.Marshal(databaseImage{Tables: tables})
}

func (d *Database) Snapshot() ([]byte, error) { return d.MarshalJSON() }

func (d *Database) UnmarshalJSON(b []byte) error {
	var img databaseImage
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&img); err != nil {
		return err
	}
	if img.Tables == nil {
		img.Tables = make(map[string]*Table)
	}
	for name, t := range img.Tables {
		if t == nil {
			return fmt.Errorf("invalid table %s", name)
		}
		if t.Rows == nil {
			t.Rows = make(map[string]map[string]any)
		}
		if t.Indexes == nil {
			t.Indexes = make(map[string]map[string]map[string]struct{})
		}
		for _, row := range t.Rows {
			if row == nil {
				return fmt.Errorf("invalid row in %s", name)
			}
			for _, col := range t.Columns {
				if n, ok := row[col.Name].(json.Number); ok {
					v, err := parseVal(string(n), col.Type)
					if err != nil {
						return fmt.Errorf("restore %s.%s: %w", name, col.Name, err)
					}
					row[col.Name] = v
				}
			}
		}
		seen := make(map[string]bool, len(t.Rows))
		order := make([]string, 0, len(t.Rows))
		for _, key := range t.Order {
			if t.Rows[key] != nil && !seen[key] {
				seen[key] = true
				order = append(order, key)
			}
		}
		// Tolerate legacy images: drop stale Order keys and reattach any
		// rows missing from Order instead of rejecting the snapshot.
		for key := range t.Rows {
			if !seen[key] {
				seen[key] = true
				order = append(order, key)
			}
		}
		t.Order = order
		for column := range t.Indexes {
			t.Indexes[column] = make(map[string]map[string]struct{})
			for key, row := range t.Rows {
				t.addIndexValue(column, valueKey(row[column]), key)
			}
		}
		t.rebuildFastPathLocked()
	}
	d.commitMu.Lock()
	defer d.commitMu.Unlock()
	d.gate.Lock()
	defer d.gate.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Tables = img.Tables
	d.revision.Add(1)
	return nil
}

func (d *Database) Clone() (*Database, error) {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()
	b, err := d.Snapshot()
	if err != nil {
		return nil, err
	}
	clone := New()
	if err := json.Unmarshal(b, clone); err != nil {
		return nil, fmt.Errorf("clone database: %w", err)
	}
	clone.baseRevision = d.revision.Load()
	return clone, nil
}

func (d *Database) ReplaceFrom(source *Database) error {
	return d.replace(source, false, nil)
}

// CommitFrom rejects stale clones and persists before publishing. The callback
// must use the supplied image, never the destination database.
func (d *Database) CommitFrom(source *Database, persist func(*Database) error) error {
	return d.replace(source, true, persist)
}

func (d *Database) replace(source *Database, check bool, persist func(*Database) error) error {
	b, err := source.Snapshot()
	if err != nil {
		return err
	}
	clone := New()
	if err := json.Unmarshal(b, clone); err != nil {
		return fmt.Errorf("replace database: %w", err)
	}
	d.commitMu.Lock()
	defer d.commitMu.Unlock()
	if check && d.revision.Load() != source.baseRevision {
		return fmt.Errorf("transaction conflict: database changed since BEGIN")
	}
	d.gate.Lock()
	defer d.gate.Unlock()
	if persist != nil {
		if err := persist(clone); err != nil {
			return err
		}
	}
	d.mu.Lock()
	d.Tables = clone.Tables
	d.mu.Unlock()
	d.revision.Add(1)
	return nil
}
