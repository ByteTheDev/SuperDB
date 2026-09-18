package engine

import (
	"encoding/json"
)

func (d *Database) Snapshot() ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return json.Marshal(d)
}
