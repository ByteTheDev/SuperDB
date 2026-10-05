package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type databaseImage struct{ Tables map[string]*Table }

// MarshalJSON provides a consistent image for snapshots and live backups.
// The database is encoded directly while gate is held — no statement can be
// in flight, so the stream sees one atomic image without paying for a
// deep copy plus a reflection walk. Output is byte-compatible with the
// previous copy-then-marshal path (same shape, sorted keys).
func (d *Database) MarshalJSON() ([]byte, error) {
	// gate.Lock excludes concurrent Exec/Clone/replace publishers for the
	// whole encode; each table's read lock is still taken while it is
	// written out.
	d.gate.Lock()
	defer d.gate.Unlock()
	d.mu.RLock()
	defer d.mu.RUnlock()
	return appendDatabaseJSON(make([]byte, 0, snapshotEstimate(d)), d), nil
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
		if t.IndexNames == nil {
			t.IndexNames = make(map[string]string)
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

// Clone returns a private, execution-ready deep copy of the database.
// The copy is made in memory — no JSON round-trip — but preserves the
// same end state the old encode/decode path produced: rebuilt-ready
// tables with indexes, fast paths, and counters intact.
func (d *Database) Clone() (*Database, error) {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()
	clone := cloneImage(d)
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
	// The publishable image is a private deep copy of source. It is made
	// before any destination lock is taken, and source's own locks are
	// taken and released inside cloneImage, so a source == destination
	// call cannot deadlock on held locks.
	clone := cloneImage(source)
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
