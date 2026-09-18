package engine

import (
	"encoding/json"
	"fmt"
)

func (d *Database) Snapshot() ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return json.Marshal(d)
}

func (d *Database) Clone() (*Database, error) {
	b, err := d.Snapshot()
	if err != nil {
		return nil, err
	}
	clone := New()
	if err := json.Unmarshal(b, clone); err != nil {
		return nil, fmt.Errorf("clone database: %w", err)
	}
	return clone, nil
}

func (d *Database) ReplaceFrom(source *Database) error {
	b, err := source.Snapshot()
	if err != nil {
		return err
	}
	clone := New()
	if err := json.Unmarshal(b, clone); err != nil {
		return fmt.Errorf("replace database: %w", err)
	}
	d.mu.Lock()
	d.Tables = clone.Tables
	d.mu.Unlock()
	return nil
}
