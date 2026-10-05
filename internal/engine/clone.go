package engine

// Direct in-memory database copies.
//
// Clone/CommitFrom previously round-tripped through Snapshot() +
// json.Unmarshal: a full JSON encode, parse, type normalization, index
// rebuild, and fast-path rebuild for every BEGIN, COMMIT, and atomic
// batch. A direct deep copy produces an identical image — same Rows,
// Order, indexes, fast path, tombstone counters, and synthetic-key
// allocator — at a fraction of the cost. Storage and wire formats are
// untouched; this changes only how the in-memory copy is made.

// cloneImage returns a private deep copy of the database. It locks the
// source internally and releases all locks before returning, so callers
// may then lock the destination database without lock-ordering concerns.
func cloneImage(d *Database) *Database {
	d.gate.Lock()
	defer d.gate.Unlock()
	return d.cloneTablesLocked()
}

// cloneTablesLocked deep-copies every table while gate is held (no
// statement can be in-flight). Each table is additionally read-locked as
// it is copied, mirroring the defensive locking MarshalJSON used.
func (d *Database) cloneTablesLocked() *Database {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := New()
	out.Tables = make(map[string]*Table, len(d.Tables))
	for name, t := range d.Tables {
		t.mu.RLock()
		out.Tables[name] = t.cloneLocked()
		t.mu.RUnlock()
	}
	return out
}

// cloneLocked deep-copies one table including the secondary indexes, the
// fast row array, and the tombstone counters, so the copy is execution-
// ready without a rebuild pass. Caller holds t.mu (read) and the database
// gate.
func (t *Table) cloneLocked() *Table {
	cp := &Table{
		Name:    t.Name,
		Columns: append([]Column(nil), t.Columns...),
		Rows:    make(map[string]map[string]any, len(t.Rows)),
		Order:   append([]string(nil), t.Order...),
		Indexes: make(map[string]map[string]map[string]struct{}, len(t.Indexes)),
		dead:    t.dead,
		autoKey: t.autoKey,
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
	if t.IndexNames != nil {
		cp.IndexNames = make(map[string]string, len(t.IndexNames))
		for name, column := range t.IndexNames {
			cp.IndexNames[name] = column
		}
	}
	if len(t.fast) > 0 {
		cp.columns = make(map[string]int, len(t.columns))
		for name, i := range t.columns {
			cp.columns[name] = i
		}
		cp.fast = make([]fastRow, len(t.fast))
		for i, fr := range t.fast {
			// The values slice must be private: updateFastRowLocked writes
			// through it and the source table must never observe clone edits.
			cp.fast[i] = fastRow{key: fr.key, values: append([]any(nil), fr.values...)}
		}
		cp.fastPos = make(map[string]int, len(t.fastPos))
		for key, i := range t.fastPos {
			cp.fastPos[key] = i
		}
		cp.fastDead = t.fastDead
	}
	return cp
}
